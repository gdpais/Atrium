package password

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"
)

// cheap keeps the tests fast; the format and the derivation are the same.
var cheap = Params{Time: 1, MemoryKiB: 64, Threads: 1, SaltBytes: 16, KeyBytes: 32}

var phc = regexp.MustCompile(`^\$argon2id\$v=19\$m=(\d+),t=(\d+),p=(\d+)\$([A-Za-z0-9+/]+)\$([A-Za-z0-9+/]+)$`)

// The default hash is RFC 9106's second recommended setting in the PHC
// string format, and its digest is argon2id of the password and the salt it
// names.
func TestHashIsArgon2idInPHCFormat(t *testing.T) {
	const secret = "correct horse battery staple"
	encoded, err := NewHasher(DefaultParams, DefaultConcurrency).Hash(context.Background(), secret)
	if err != nil {
		t.Fatal(err)
	}
	m := phc.FindStringSubmatch(encoded)
	if m == nil {
		t.Fatalf("%q is not a PHC argon2id string", encoded)
	}
	if params := strings.Join(m[1:4], ","); params != "65536,3,4" {
		t.Fatalf("m,t,p = %s, want 65536,3,4 (RFC 9106 section 4, second recommended option)", params)
	}
	salt, err := base64.RawStdEncoding.DecodeString(m[4])
	if err != nil || len(salt) != 16 {
		t.Fatalf("salt %q: %d bytes, %v; want 16 bytes of unpadded base64", m[4], len(salt), err)
	}
	digest, err := base64.RawStdEncoding.DecodeString(m[5])
	if err != nil || len(digest) != 32 {
		t.Fatalf("digest %q: %d bytes, %v; want 32 bytes of unpadded base64", m[5], len(digest), err)
	}
	if want := argon2.IDKey([]byte(secret), salt, 3, 64*1024, 4, 32); !bytes.Equal(digest, want) {
		t.Fatal("digest is not argon2id(password, salt, t=3, m=64 MiB, p=4)")
	}
	if strings.Contains(encoded, secret) {
		t.Fatal("the hash contains the password")
	}
}

// Every hash has a salt of its own, so equal passwords hash differently.
func TestHashSaltsEachPassword(t *testing.T) {
	h := NewHasher(cheap, 1)
	seen := map[string]bool{}
	for range 50 {
		encoded, err := h.Hash(context.Background(), "correct horse battery staple")
		if err != nil {
			t.Fatal(err)
		}
		salt := phc.FindStringSubmatch(encoded)[4]
		if seen[salt] {
			t.Fatalf("salt %s used twice", salt)
		}
		seen[salt] = true
	}
}

// A Hasher runs at most its concurrency at once: the next one waits for a
// slot, and gives up when its context ends first.
func TestHashWaitsForAFreeSlot(t *testing.T) {
	h := NewHasher(cheap, 2)
	h.slots <- struct{}{}
	h.slots <- struct{}{}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := h.Hash(ctx, "correct horse battery staple"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Hash with every slot taken = %v, want to wait until the deadline", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := h.Hash(context.Background(), "correct horse battery staple")
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("Hash returned (%v) while every slot was taken", err)
	case <-time.After(50 * time.Millisecond):
	}
	<-h.slots
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Hash did not run once a slot was free")
	}
	if len(h.slots) != 1 {
		t.Fatalf("%d slots taken after Hash returned, want the 1 the test holds", len(h.slots))
	}
}

func TestNewHasherRunsAtLeastOne(t *testing.T) {
	if got := cap(NewHasher(cheap, 0).slots); got != 1 {
		t.Fatalf("concurrency 0 gives %d slots, want 1", got)
	}
}

func TestCheck(t *testing.T) {
	for _, tc := range []struct {
		password string
		want     error
	}{
		{"", ErrTooShort},
		{"fourteen chars", ErrTooShort},
		{"fifteen chars!!", nil},
		{"ééééééééééééééé", nil}, // 15 characters, 30 bytes
		{strings.Repeat("x", MaxBytes), nil},
		{strings.Repeat("x", MaxBytes+1), ErrTooLong},
		{"correct horse \xff\xfe battery", errors.New("password: not valid UTF-8")},
	} {
		t.Run(fmt.Sprintf("%.20q", tc.password), func(t *testing.T) {
			err := Check(tc.password)
			if fmt.Sprint(err) != fmt.Sprint(tc.want) {
				t.Fatalf("Check = %v, want %v", err, tc.want)
			}
		})
	}
}
