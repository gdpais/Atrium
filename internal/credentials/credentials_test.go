package credentials

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"atrium/internal/credentials/credentialstest"
)

const testPassword = "s3cr3t-P@ssw0rd"

func mustResolve(t *testing.T, r Resolver, ref string) Credential {
	t.Helper()
	creds, err := r.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve(%s): %v", ref, err)
	}
	return creds
}

func TestKey(t *testing.T) {
	for ref, want := range map[string]string{
		"env/sqlserver-b":         "ATRIUM_CREDENTIAL_ENV_SQLSERVER_B",
		"kv/sqlserver-default":    "ATRIUM_CREDENTIAL_KV_SQLSERVER_DEFAULT",
		"secret/finance.prod-ro":  "ATRIUM_CREDENTIAL_SECRET_FINANCE_PROD_RO",
		"env/Mixed/Case_name-1.2": "ATRIUM_CREDENTIAL_ENV_MIXED_CASE_NAME_1_2",
	} {
		if got := Key(ref); got != want {
			t.Errorf("Key(%q) = %q, want %q", ref, got, want)
		}
	}
}

func TestEnv(t *testing.T) {
	t.Setenv("ATRIUM_CREDENTIAL_KV_SQLSERVER_DEFAULT", "collector:pass:with:colons")
	t.Setenv("ATRIUM_CREDENTIAL_ENV_MALFORMED", "no-separator-"+testPassword)

	got := mustResolve(t, Env{}, "kv/sqlserver-default")
	if want := (Credential{Username: "collector", Password: "pass:with:colons"}); got != want {
		t.Fatalf("Resolve = %+v, want %+v (split at the first colon)", got, want)
	}
	if _, err := (Env{}).Resolve(context.Background(), "env/absent"); err == nil || !strings.Contains(err.Error(), "missing credential for ref env/absent") {
		t.Fatalf("absent variable: err = %v", err)
	}
	_, err := (Env{}).Resolve(context.Background(), "env/malformed")
	if err == nil || !strings.Contains(err.Error(), "must be username:password") {
		t.Fatalf("malformed variable: err = %v", err)
	}
	if strings.Contains(err.Error(), testPassword) {
		t.Fatalf("error quotes the value: %v", err)
	}
}

// A rotated, added or removed Secret key is seen by the next Resolve of the
// same resolver: nothing is cached at startup.
func TestFilesFollowsSecretUpdates(t *testing.T) {
	dir := t.TempDir()
	credentialstest.WriteSecretVolume(t, dir, map[string]string{
		"ATRIUM_CREDENTIAL_ENV_SQLSERVER_A": "collector:old-password\n",
	})
	r := Files{Dir: dir}

	if got := mustResolve(t, r, "env/sqlserver-a"); got != (Credential{Username: "collector", Password: "old-password"}) {
		t.Fatalf("before rotation: %+v", got)
	}

	credentialstest.WriteSecretVolume(t, dir, map[string]string{
		"ATRIUM_CREDENTIAL_ENV_SQLSERVER_A": "collector:new-password\n",
		"ATRIUM_CREDENTIAL_KV_SQLSERVER_B":  "reader:b-password",
	})
	if got := mustResolve(t, r, "env/sqlserver-a"); got != (Credential{Username: "collector", Password: "new-password"}) {
		t.Fatalf("after rotation: %+v", got)
	}
	if got := mustResolve(t, r, "kv/sqlserver-b"); got != (Credential{Username: "reader", Password: "b-password"}) {
		t.Fatalf("added key: %+v", got)
	}

	credentialstest.WriteSecretVolume(t, dir, map[string]string{
		"ATRIUM_CREDENTIAL_KV_SQLSERVER_B": "reader:b-password",
	})
	if _, err := r.Resolve(context.Background(), "env/sqlserver-a"); err == nil || !strings.Contains(err.Error(), "missing credential for ref env/sqlserver-a") {
		t.Fatalf("removed key: err = %v", err)
	}
}

func TestFilesFallsBack(t *testing.T) {
	const ref = "env/sqlserver-a"
	t.Setenv("ATRIUM_CREDENTIAL_ENV_SQLSERVER_A", "env-user:env-password")
	fromEnv := Credential{Username: "env-user", Password: "env-password"}
	withFile := t.TempDir()
	credentialstest.WriteSecretVolume(t, withFile, map[string]string{
		"ATRIUM_CREDENTIAL_ENV_SQLSERVER_A": "file-user:from-file",
	})
	withoutFile := t.TempDir()
	credentialstest.WriteSecretVolume(t, withoutFile, map[string]string{
		"ATRIUM_CREDENTIAL_ENV_OTHER": "other:other-password",
	})

	for _, tc := range []struct {
		name string
		dir  string
		want Credential
	}{
		{"no directory configured", "", fromEnv},
		{"directory does not exist", filepath.Join(t.TempDir(), "absent"), fromEnv},
		{"no file for the ref", withoutFile, fromEnv},
		{"file wins over the environment", withFile, Credential{Username: "file-user", Password: "from-file"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Files{Dir: tc.dir, Fallback: Env{}}
			if got := mustResolve(t, r, ref); got != tc.want {
				t.Fatalf("Resolve = %+v, want %+v", got, tc.want)
			}
		})
	}

	t.Run("no fallback", func(t *testing.T) {
		_, err := Files{Dir: withoutFile}.Resolve(context.Background(), ref)
		if err == nil || !strings.Contains(err.Error(), "missing credential for ref "+ref) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestFilesValues(t *testing.T) {
	for value, want := range map[string]Credential{
		"collector:" + testPassword:            {Username: "collector", Password: testPassword},
		"collector:" + testPassword + "\n":     {Username: "collector", Password: testPassword},
		"collector:" + testPassword + "\r\n":   {Username: "collector", Password: testPassword},
		"collector:pass:with:colons\n":         {Username: "collector", Password: "pass:with:colons"},
		"collector: spaces are kept \n":        {Username: "collector", Password: " spaces are kept "},
		`DOMAIN\collector:` + testPassword:     {Username: `DOMAIN\collector`, Password: testPassword},
		"collector:" + testPassword + "\n\n\n": {Username: "collector", Password: testPassword},
		":" + testPassword:                     {Username: "", Password: testPassword},
	} {
		dir := t.TempDir()
		credentialstest.WriteSecretVolume(t, dir, map[string]string{"ATRIUM_CREDENTIAL_ENV_A": value})
		if got := mustResolve(t, Files{Dir: dir}, "env/a"); got != want {
			t.Errorf("file %q: Resolve = %+v, want %+v", value, got, want)
		}
	}
}

// A file that is there but unusable is an error, never a reason to use the
// environment's credential instead, and the error never quotes the file.
func TestFilesDoesNotFallBackOnBadFiles(t *testing.T) {
	t.Setenv("ATRIUM_CREDENTIAL_ENV_A", "env-user:env-password")
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("outside:"+testPassword), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  string
	}{
		{
			name: "no colon",
			setup: func(t *testing.T, dir string) {
				credentialstest.WriteSecretVolume(t, dir, map[string]string{"ATRIUM_CREDENTIAL_ENV_A": "collector" + testPassword})
			},
			want: "credential env/a must be username:password",
		},
		{
			name: "empty file",
			setup: func(t *testing.T, dir string) {
				credentialstest.WriteSecretVolume(t, dir, map[string]string{"ATRIUM_CREDENTIAL_ENV_A": ""})
			},
			want: "credential env/a must be username:password",
		},
		{
			// os.Root refuses symlinks that leave the directory.
			name: "symlink out of the directory",
			setup: func(t *testing.T, dir string) {
				if err := os.Symlink(outside, filepath.Join(dir, "ATRIUM_CREDENTIAL_ENV_A")); err != nil {
					t.Fatal(err)
				}
			},
			want: "read credential for ref env/a",
		},
		{
			name: "unreadable entry",
			setup: func(t *testing.T, dir string) {
				if err := os.Mkdir(filepath.Join(dir, "ATRIUM_CREDENTIAL_ENV_A"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
			want: "read credential for ref env/a",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(t, dir)
			r := Files{Dir: dir, Fallback: Env{}}
			creds, err := r.Resolve(context.Background(), "env/a")
			if err == nil {
				t.Fatalf("Resolve = %+v, want an error", creds)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), testPassword) {
				t.Fatalf("error quotes the credential: %v", err)
			}
		})
	}
}
