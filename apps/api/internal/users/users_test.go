package users

import (
	"context"
	"errors"
	"strings"
	"testing"

	"atrium/apps/api/internal/audit"
	"atrium/apps/api/internal/password"
)

func TestNormalizeEmail(t *testing.T) {
	for input, want := range map[string]string{
		"admin@example.com":         "admin@example.com",
		"  Admin@Example.COM\t":     "admin@example.com",
		"first.last+ops@example.pt": "first.last+ops@example.pt",
	} {
		if got, err := NormalizeEmail(input); err != nil || got != want {
			t.Errorf("NormalizeEmail(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{
		"", "admin", "@example.com", "admin@", "Admin <admin@example.com>", "<admin@example.com>",
		"admin@example.com, other@example.com", strings.Repeat("a", 250) + "@example.com",
	} {
		if got, err := NormalizeEmail(input); err == nil {
			t.Errorf("NormalizeEmail(%q) = %q, want an error", input, got)
		}
	}
}

func TestDisplayName(t *testing.T) {
	for _, tc := range []struct{ name, email, want string }{
		{"", "ops.admin@example.com", "ops.admin"},
		{"  First Admin ", "admin@example.com", "First Admin"},
		{"Zé Admin", "admin@example.com", "Zé Admin"},
	} {
		if got, err := displayName(tc.name, tc.email); err != nil || got != tc.want {
			t.Errorf("displayName(%q, %q) = %q, %v; want %q", tc.name, tc.email, got, err, tc.want)
		}
	}
	for _, name := range []string{"line\nbreak", "bell\a", strings.Repeat("x", 201), "bad \xff utf-8"} {
		if _, err := displayName(name, "admin@example.com"); err == nil {
			t.Errorf("displayName(%q) accepted", name)
		}
	}
}

// Input that cannot be stored is refused before hashing or touching the
// database, which is nil here.
func TestCreateAdminChecksItsInputFirst(t *testing.T) {
	for name, admin := range map[string]NewAdmin{
		"email":    {Email: "admin", Password: "correct horse battery staple"},
		"name":     {Email: "admin@example.com", DisplayName: "a\nb", Password: "correct horse battery staple"},
		"password": {Email: "admin@example.com", Password: "short"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := CreateAdmin(context.Background(), nil, password.NewHasher(password.DefaultParams, 1), admin,
				audit.CLIActor("api users create-admin", "test"), "req-1")
			if err == nil {
				t.Fatal("CreateAdmin accepted the admin")
			}
			if name == "password" && !errors.Is(err, password.ErrTooShort) {
				t.Fatalf("err = %v, want ErrTooShort", err)
			}
		})
	}
}
