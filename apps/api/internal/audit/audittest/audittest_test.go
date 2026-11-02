package audittest

import (
	"testing"
)

// spy is a testing.TB that records whether it was failed.
type spy struct {
	testing.TB
	failed bool
}

func (s *spy) Helper()               {}
func (s *spy) Errorf(string, ...any) { s.failed = true }

// NoSecrets catches a declared secret anywhere in a record, a password hash
// taken apart, and a hash nobody declared.
func TestNoSecrets(t *testing.T) {
	const (
		password = "correct horse battery staple"
		salt     = "c2FsdHNhbHRzYWx0c2FsdA"
		digest   = "3q2+7wABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhs"
		hash     = "$argon2id$v=19$m=65536,t=3,p=4$" + salt + "$" + digest
	)
	for _, tc := range []struct {
		name    string
		record  string
		secrets []string
		fail    bool
	}{
		{"clean", `{"after":{"email":"admin@example.com"}}`, []string{password, hash}, false},
		{"password", `{"after":{"note":"` + password + `"}}`, []string{password, hash}, true},
		{"hash", `{"after":{"password_hash":"` + hash + `"}}`, []string{password, hash}, true},
		{"salt alone", `{"after":{"salt":"` + salt + `"}}`, []string{hash}, true},
		{"digest alone", `{"after":{"key":"` + digest + `"}}`, []string{hash}, true},
		{"undeclared hash", `{"after":{"h":"$argon2id$v=19$m=1,t=1,p=1$YWJj$ZGVm"}}`, nil, true},
		{"empty secret ignored", `{"after":{}}`, []string{""}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &spy{}
			NoSecrets(s, []byte(tc.record), tc.secrets...)
			if s.failed != tc.fail {
				t.Fatalf("NoSecrets failed = %v, want %v", s.failed, tc.fail)
			}
		})
	}
}
