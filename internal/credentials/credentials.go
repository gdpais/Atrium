// Package credentials resolves Atrium's credential references into a
// username and password, the same way in every service.
//
// A credential reference, such as "kv/sqlserver-default", is resolved under
// the name
//
//	ATRIUM_CREDENTIAL_<REF>
//
// where <REF> is the reference upper-cased, with "/", "-" and "." replaced
// by underscores ([Key]). The value must be a colon-separated
// "username:password" pair; the password is everything after the first
// colon. [Files] reads it from a file of that name in a directory, such as a
// mounted Kubernetes Secret, on every call, so a rotated credential needs no
// restart; [Env] reads it from the environment variable of that name.
//
// No error of this package quotes a credential's value.
package credentials

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// Credential is a username and password.
type Credential struct {
	Username string
	Password string
}

// Resolver resolves a credential reference into a [Credential].
type Resolver interface {
	Resolve(ctx context.Context, ref string) (Credential, error)
}

// Env resolves credentials from environment variables.
type Env struct{}

// Resolve implements [Resolver]. It looks up the environment variable
// ATRIUM_CREDENTIAL_<REF> (see [Key]), which must contain a
// "username:password" pair; an error is returned when the variable is absent
// or malformed.
func (Env) Resolve(_ context.Context, ref string) (Credential, error) {
	value := os.Getenv(Key(ref))
	if value == "" {
		return Credential{}, fmt.Errorf("missing credential for ref %s", ref)
	}
	return parse(ref, value)
}

// Files resolves credentials from files in Dir, one per reference, named
// like the variable [Env] reads (ATRIUM_CREDENTIAL_<REF>) and holding the
// same "username:password" pair; a trailing line break is ignored. Dir is
// meant to be a Kubernetes Secret mounted as a directory, whose keys are
// those names.
//
// Every Resolve reads the file again, so a key added to the Secret, or a
// rotated one, is used by the next connection once the kubelet has updated
// the volume, without a restart. The kubelet swaps the whole volume at once,
// so a read never sees half an update.
//
// A reference with no file, and every reference while Dir is empty or does
// not exist, goes to Fallback, or is missing when Fallback is nil. A file
// that exists but cannot be read or parsed is an error and never falls back,
// so a stale credential from elsewhere is not used in its place.
type Files struct {
	// Dir is the directory holding the credential files.
	Dir string
	// Fallback resolves the references Dir has no file for, typically
	// [Env].
	Fallback Resolver
}

// Resolve implements [Resolver].
func (r Files) Resolve(ctx context.Context, ref string) (Credential, error) {
	if r.Dir != "" {
		value, err := readFile(r.Dir, Key(ref))
		switch {
		case err == nil:
			return parse(ref, strings.TrimRight(value, "\r\n"))
		case !errors.Is(err, fs.ErrNotExist):
			return Credential{}, fmt.Errorf("read credential for ref %s: %w", ref, err)
		}
	}
	if r.Fallback == nil {
		return Credential{}, fmt.Errorf("missing credential for ref %s", ref)
	}
	return r.Fallback.Resolve(ctx, ref)
}

// readFile reads the file name in dir through an [os.Root], so no reference
// can name a file outside dir, while the relative symlinks the kubelet keeps
// inside a Secret volume are still followed.
func readFile(dir, name string) (string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	data, err := root.ReadFile(name)
	return string(data), err
}

// Key returns the name ref's credential is stored under, as an environment
// variable or a file: ATRIUM_CREDENTIAL_ followed by ref upper-cased,
// with "/", "-" and "." replaced by "_".
func Key(ref string) string {
	return "ATRIUM_CREDENTIAL_" + strings.NewReplacer("/", "_", "-", "_", ".", "_").Replace(strings.ToUpper(ref))
}

// parse splits a "username:password" value at its first colon. Its error
// never quotes value.
func parse(ref, value string) (Credential, error) {
	username, password, ok := strings.Cut(value, ":")
	if !ok {
		return Credential{}, fmt.Errorf("credential %s must be username:password", ref)
	}
	return Credential{Username: username, Password: password}, nil
}
