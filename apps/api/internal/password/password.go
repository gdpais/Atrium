// Package password hashes local users' passwords with argon2id (ADR 0013
// §7), in the PHC string format, which carries the parameters with the hash
// so they can be raised later and old hashes upgraded at the next login:
// [Hasher.Verify] says when a hash was made with other parameters.
//
// Each hash, and each verification, takes [Params.MemoryKiB] of memory for
// the time it runs, so a [Hasher] runs at most a fixed number at once and
// makes the others wait: a burst of logins queues instead of running the
// pod out of memory.
package password

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Params are argon2id's cost parameters.
type Params struct {
	// Time is the number of passes over the memory (t).
	Time uint32
	// MemoryKiB is the memory used, in KiB (m).
	MemoryKiB uint32
	// Threads is the degree of parallelism (p).
	Threads uint8
	// SaltBytes is the length of the random salt.
	SaltBytes int
	// KeyBytes is the length of the derived hash.
	KeyBytes uint32
}

// DefaultParams is RFC 9106's second recommended setting (section 4): t=3,
// p=4, 64 MiB, a 128-bit salt and a 256-bit tag.
var DefaultParams = Params{Time: 3, MemoryKiB: 64 * 1024, Threads: 4, SaltBytes: 16, KeyBytes: 32}

// DefaultConcurrency is how many hashes a Hasher runs at once by default:
// with DefaultParams, 128 MiB of a replica's memory at most.
const DefaultConcurrency = 2

// Limits on a password's length. The minimum is NIST SP 800-63B-4's for a
// password that is the only factor; the maximum keeps a login request from
// hashing megabytes.
const (
	MinLength = 15
	MaxBytes  = 1024
)

// phcFormat is the PHC string of an argon2id hash: version, memory, time,
// threads, then the salt and the hash in unpadded standard base64.
const phcFormat = "$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s"

// Hasher hashes passwords, at most a fixed number at once. It is safe for
// concurrent use.
type Hasher struct {
	params Params
	slots  chan struct{}
}

// NewHasher returns a Hasher with params that runs at most concurrency
// hashes at once; below 1 means 1.
func NewHasher(params Params, concurrency int) *Hasher {
	return &Hasher{params: params, slots: make(chan struct{}, max(concurrency, 1))}
}

// ErrTooShort and ErrTooLong report a password outside the length limits.
var (
	ErrTooShort = fmt.Errorf("password: shorter than %d characters", MinLength)
	ErrTooLong  = fmt.Errorf("password: longer than %d bytes", MaxBytes)
)

// Check reports whether password is acceptable: valid UTF-8 of at least
// MinLength characters and at most MaxBytes bytes.
func Check(password string) error {
	switch {
	case !utf8.ValidString(password):
		return errors.New("password: not valid UTF-8")
	case len(password) > MaxBytes:
		return ErrTooLong
	case utf8.RuneCountInString(password) < MinLength:
		return ErrTooShort
	}
	return nil
}

// Hash returns password's argon2id hash with a new random salt, as a PHC
// string: $argon2id$v=19$m=65536,t=3,p=4$<salt>$<hash>, both in unpadded
// standard base64. It waits for a free slot, or fails when ctx ends first.
// It does not check the password's length; [Check] does.
func (h *Hasher) Hash(ctx context.Context, password string) (string, error) {
	salt := make([]byte, h.params.SaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("password: salt: %w", err)
	}
	p := h.params
	key, err := h.derive(ctx, password, salt, p)
	if err != nil {
		return "", err
	}
	b64 := base64.RawStdEncoding
	return fmt.Sprintf(phcFormat, argon2.Version, p.MemoryKiB, p.Time, p.Threads,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// Bounds on the parameters Verify accepts in a stored hash, so that a hash
// edited in the database cannot make one login take gigabytes or minutes.
// They leave room to raise DefaultParams.
const (
	maxTime      = 16
	maxMemoryKiB = 256 * 1024
	minSaltBytes = 8
	minKeyBytes  = 16
	maxKeyBytes  = 128
)

// ErrMalformedHash reports a stored hash Verify cannot read.
var ErrMalformedHash = errors.New("password: not an argon2id PHC string this build can check")

// Verify reports whether password matches hash, a PHC string as [Hasher.Hash]
// writes, and whether hash should be replaced with a new one because it was
// made with other parameters than the Hasher's. It waits for a slot as Hash
// does, since checking costs what hashing with the hash's own parameters
// costs. A hash it cannot read is [ErrMalformedHash], not a mismatch; a
// password longer than [MaxBytes] is [ErrTooLong], checked before any work.
func (h *Hasher) Verify(ctx context.Context, password, hash string) (match, rehash bool, err error) {
	if len(password) > MaxBytes {
		return false, false, ErrTooLong
	}
	p, salt, key, err := parse(hash)
	if err != nil {
		return false, false, err
	}
	got, err := h.derive(ctx, password, salt, p)
	if err != nil {
		return false, false, err
	}
	if subtle.ConstantTimeCompare(got, key) != 1 {
		return false, false, nil
	}
	return true, p != h.params, nil
}

// derive runs argon2id once a slot is free, or fails when ctx ends first.
func (h *Hasher) derive(ctx context.Context, password string, salt []byte, p Params) ([]byte, error) {
	select {
	case h.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("password: waiting to hash: %w", ctx.Err())
	}
	defer func() { <-h.slots }()
	return argon2.IDKey([]byte(password), salt, p.Time, p.MemoryKiB, p.Threads, p.KeyBytes), nil
}

// parse reads a PHC string as Hash writes it, within the bounds above.
func parse(hash string) (Params, []byte, []byte, error) {
	fields := strings.Split(hash, "$")
	if len(fields) != 6 || fields[0] != "" || fields[1] != "argon2id" {
		return Params{}, nil, nil, ErrMalformedHash
	}
	var version int
	var p Params
	if n, err := fmt.Sscanf(fields[2], "v=%d", &version); err != nil || n != 1 || version != argon2.Version {
		return Params{}, nil, nil, ErrMalformedHash
	}
	if n, err := fmt.Sscanf(fields[3], "m=%d,t=%d,p=%d", &p.MemoryKiB, &p.Time, &p.Threads); err != nil || n != 3 ||
		fmt.Sprintf("m=%d,t=%d,p=%d", p.MemoryKiB, p.Time, p.Threads) != fields[3] {
		return Params{}, nil, nil, ErrMalformedHash
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(fields[4])
	if err != nil {
		return Params{}, nil, nil, ErrMalformedHash
	}
	key, err := b64.DecodeString(fields[5])
	if err != nil {
		return Params{}, nil, nil, ErrMalformedHash
	}
	p.SaltBytes, p.KeyBytes = len(salt), uint32(len(key))
	switch {
	case p.Time < 1 || p.Time > maxTime,
		p.Threads < 1,
		p.MemoryKiB < 8*uint32(p.Threads) || p.MemoryKiB > maxMemoryKiB,
		len(salt) < minSaltBytes,
		len(key) < minKeyBytes || len(key) > maxKeyBytes:
		return Params{}, nil, nil, ErrMalformedHash
	}
	return p, salt, key, nil
}
