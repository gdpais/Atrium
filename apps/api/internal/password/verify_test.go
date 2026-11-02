package password

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"
)

// Verify accepts the password a hash was made from, and only that one.
func TestVerifyMatchesOnlyTheHashedPassword(t *testing.T) {
	h := NewHasher(cheap, 1)
	ctx := context.Background()
	const secret = "correct horse battery staple"
	encoded, err := h.Hash(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		password string
		match    bool
	}{
		{secret, true},
		{"correct horse battery staplE", false},
		{secret + " ", false},
		{"", false},
	} {
		match, rehash, err := h.Verify(ctx, tc.password, encoded)
		if err != nil || match != tc.match || rehash {
			t.Errorf("Verify(%q) = %v, %v, %v; want %v, false, nil", tc.password, match, rehash, err, tc.match)
		}
	}
}

// A hash in the PHC format checks with the parameters it names, whatever
// the Hasher's are, and asks to be replaced when they differ from them, as
// after DefaultParams is raised.
func TestVerifyUsesTheHashsParametersAndAsksForAnUpgrade(t *testing.T) {
	ctx := context.Background()
	const secret = "correct horse battery staple"
	old := Params{Time: 2, MemoryKiB: 32, Threads: 1, SaltBytes: 16, KeyBytes: 32}
	encoded, err := NewHasher(old, 1).Hash(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}
	for name, current := range map[string]Params{
		"time":    {Time: 3, MemoryKiB: 32, Threads: 1, SaltBytes: 16, KeyBytes: 32},
		"memory":  {Time: 2, MemoryKiB: 64, Threads: 1, SaltBytes: 16, KeyBytes: 32},
		"threads": {Time: 2, MemoryKiB: 32, Threads: 2, SaltBytes: 16, KeyBytes: 32},
		"salt":    {Time: 2, MemoryKiB: 32, Threads: 1, SaltBytes: 32, KeyBytes: 32},
		"key":     {Time: 2, MemoryKiB: 32, Threads: 1, SaltBytes: 16, KeyBytes: 64},
	} {
		t.Run(name, func(t *testing.T) {
			match, rehash, err := NewHasher(current, 1).Verify(ctx, secret, encoded)
			if err != nil || !match || !rehash {
				t.Fatalf("Verify = %v, %v, %v; want a match that asks to be rehashed", match, rehash, err)
			}
			if match, rehash, _ := NewHasher(current, 1).Verify(ctx, "wrong password, long enough", encoded); match || rehash {
				t.Fatalf("a wrong password = %v, %v; want no match and no rehash", match, rehash)
			}
		})
	}
	if match, rehash, err := NewHasher(old, 1).Verify(ctx, secret, encoded); err != nil || !match || rehash {
		t.Fatalf("same parameters = %v, %v, %v; want a match without rehash", match, rehash, err)
	}
}

// A hash written elsewhere in the same format, such as by another argon2id
// library, checks too: Verify reads the PHC string, not this package's
// output alone.
func TestVerifyReadsAPHCStringFromElsewhere(t *testing.T) {
	salt := []byte("0123456789abcdef")
	key := argon2.IDKey([]byte("correct horse battery staple"), salt, 1, 64, 1, 32)
	b64 := base64.RawStdEncoding
	encoded := fmt.Sprintf("$argon2id$v=19$m=64,t=1,p=1$%s$%s", b64.EncodeToString(salt), b64.EncodeToString(key))
	if match, _, err := NewHasher(cheap, 1).Verify(context.Background(), "correct horse battery staple", encoded); err != nil || !match {
		t.Fatalf("Verify = %v, %v; want a match", match, err)
	}
}

// A stored hash Verify cannot read, or whose parameters would cost too much
// to check, is an error and is never checked.
func TestVerifyRejectsMalformedHashes(t *testing.T) {
	b64 := base64.RawStdEncoding
	salt, key := b64.EncodeToString([]byte("0123456789abcdef")), b64.EncodeToString(make([]byte, 32))
	valid := fmt.Sprintf("$argon2id$v=19$m=64,t=1,p=1$%s$%s", salt, key)
	if _, _, err := NewHasher(cheap, 1).Verify(context.Background(), "x", valid); err != nil {
		t.Fatalf("the valid hash the cases start from: %v", err)
	}
	for name, encoded := range map[string]string{
		"empty":            "",
		"argon2i":          strings.Replace(valid, "argon2id", "argon2i", 1),
		"bcrypt":           "$2b$12$R9h/cIPz0gi.URNNX3kh2OPST9/PgBkqquzi.Ss7KIUgO2t0jWMUW",
		"old version":      strings.Replace(valid, "v=19", "v=16", 1),
		"extra field":      valid + "$x",
		"no leading $":     valid[1:],
		"no time":          strings.Replace(valid, "m=64,t=1,p=1", "m=64,p=1", 1),
		"time 0":           strings.Replace(valid, "t=1,", "t=0,", 1),
		"time too high":    strings.Replace(valid, "t=1,", "t=17,", 1),
		"memory too high":  strings.Replace(valid, "m=64,", fmt.Sprintf("m=%d,", maxMemoryKiB+1), 1),
		"memory below 8p":  strings.Replace(valid, "m=64,t=1,p=1", "m=15,t=1,p=2", 1),
		"threads 0":        strings.Replace(valid, "p=1$", "p=0$", 1),
		"threads too high": strings.Replace(valid, "p=1$", "p=256$", 1),
		"trailing text":    strings.Replace(valid, "p=1$", "p=1x$", 1),
		"padded salt":      strings.Replace(valid, salt, salt+"==", 1),
		"short salt":       strings.Replace(valid, salt, b64.EncodeToString([]byte("0123456")), 1),
		"short key":        strings.Replace(valid, key, b64.EncodeToString(make([]byte, 15)), 1),
		"long key":         strings.Replace(valid, key, b64.EncodeToString(make([]byte, 129)), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := NewHasher(cheap, 1).Verify(context.Background(), "x", encoded); !errors.Is(err, ErrMalformedHash) {
				t.Fatalf("Verify(%q) = %v, want ErrMalformedHash", encoded, err)
			}
		})
	}
}

// A password longer than MaxBytes is refused before any hashing.
func TestVerifyRefusesALongPasswordWithoutHashing(t *testing.T) {
	h := NewHasher(cheap, 1)
	encoded, err := h.Hash(context.Background(), "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	h.slots <- struct{}{} // every slot taken: hashing would block
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, _, err := h.Verify(ctx, strings.Repeat("a", MaxBytes+1), encoded); !errors.Is(err, ErrTooLong) {
		t.Fatalf("Verify = %v, want ErrTooLong at once", err)
	}
}

// Verifying takes a slot as hashing does: with every slot taken it waits,
// and fails when its context ends.
func TestVerifyWaitsForAFreeSlot(t *testing.T) {
	h := NewHasher(cheap, 1)
	encoded, err := h.Hash(context.Background(), "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	h.slots <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := h.Verify(ctx, "correct horse battery staple", encoded); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Verify with every slot taken = %v, want to wait until the deadline", err)
	}
	<-h.slots
	if match, _, err := h.Verify(context.Background(), "correct horse battery staple", encoded); err != nil || !match {
		t.Fatalf("Verify once a slot is free = %v, %v", match, err)
	}
}
