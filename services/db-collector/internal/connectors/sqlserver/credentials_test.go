package sqlserver

import (
	"net/url"
	"strings"
	"testing"

	"atrium/internal/credentials/credentialstest"
)

// The resolvers themselves are tested in package credentials.

// Manager.Open resolves on every call, so after a rotation the next Open
// connects with the new credential, in a new pool, without a restart.
func TestOpenUsesRotatedCredentialFile(t *testing.T) {
	dir := t.TempDir()
	credentialstest.WriteSecretVolume(t, dir, map[string]string{
		"ATRIUM_CREDENTIAL_ENV_SQLSERVER_A": "collector:" + testPassword + "\n",
	})
	d, name := newFakeDriver(t)
	m := NewManager(FileCredentialResolver{Dir: dir})
	m.driverName = name
	t.Cleanup(func() { _ = m.Close() })
	tg := target("a", "db-a.example", "env/sqlserver-a")

	db1, c1 := mustOpen(t, m, tg)
	c1()
	if !strings.Contains(d.lastDSN(), url.UserPassword("collector", testPassword).String()+"@") {
		t.Fatal("first connection does not use the mounted credential")
	}

	rotated := testPassword + "-rotated"
	credentialstest.WriteSecretVolume(t, dir, map[string]string{
		"ATRIUM_CREDENTIAL_ENV_SQLSERVER_A": "collector:" + rotated + "\n",
	})
	db2, c2 := mustOpen(t, m, tg)
	defer c2()
	if db2 == db1 {
		t.Fatal("rotated credential reused the old pool")
	}
	if !strings.Contains(d.lastDSN(), url.UserPassword("collector", rotated).String()+"@") {
		t.Fatal("connection after rotation does not use the rotated credential")
	}
}
