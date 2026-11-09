package store

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"atrium/db/migrations"
	"atrium/internal/credentials"
	"atrium/internal/credentials/credentialstest"
)

func validConfig() Config {
	return Config{
		Host: "postgresql.atrium.svc", Port: 5432, Database: "atrium",
		SSLMode: "verify-full", CredentialRef: "secret/api-postgres",
	}
}

func TestConfigValidate(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	for _, tc := range []struct {
		name   string
		change func(*Config)
		want   string
	}{
		{"no host", func(c *Config) { c.Host = "" }, "no host"},
		{"port 0", func(c *Config) { c.Port = 0 }, "port 0"},
		{"port too high", func(c *Config) { c.Port = 70000 }, "port 70000"},
		{"no database", func(c *Config) { c.Database = "" }, "no database"},
		{"prefer falls back to plaintext", func(c *Config) { c.SSLMode = "prefer" }, "sslmode"},
		{"no sslmode", func(c *Config) { c.SSLMode = "" }, "sslmode"},
		{"no credential", func(c *Config) { c.CredentialRef = "" }, "credential"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.change(&cfg)
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want an error about %s", err, tc.want)
			}
			if _, err := NewPool(cfg, credentials.Env{}); err == nil {
				t.Fatal("NewPool accepted a config Validate rejects")
			}
		})
	}
}

func TestConnConfigUsesTheSettings(t *testing.T) {
	cfg := validConfig()
	cfg.Host, cfg.Port, cfg.Database, cfg.SSLMode = "2001:db8::5", 6432, "atrium prod", "disable"
	cc, err := cfg.connConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cc.Host != "2001:db8::5" || cc.Port != 6432 || cc.Database != "atrium prod" {
		t.Fatalf("host %q port %d database %q", cc.Host, cc.Port, cc.Database)
	}
	if cc.TLSConfig != nil {
		t.Fatal("sslmode disable still configures TLS")
	}
	if cc.ConnectTimeout != connectTimeout {
		t.Fatalf("ConnectTimeout = %s, want %s", cc.ConnectTimeout, connectTimeout)
	}
	if got := cc.RuntimeParams["application_name"]; got != applicationName {
		t.Fatalf("application_name = %q, want %q", got, applicationName)
	}
	// No password in the settings: the login is set on each connection.
	if cc.Password != "" {
		t.Fatal("connection settings carry a password")
	}
}

func TestConnConfigVerifiesTLSByDefault(t *testing.T) {
	cc, err := validConfig().connConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cc.TLSConfig == nil || cc.TLSConfig.InsecureSkipVerify && cc.TLSConfig.VerifyPeerCertificate == nil {
		t.Fatalf("verify-full does not verify the server: %+v", cc.TLSConfig)
	}
	if cc.TLSConfig.ServerName != "postgresql.atrium.svc" {
		t.Fatalf("TLS ServerName = %q, want the host", cc.TLSConfig.ServerName)
	}
	if len(cc.Fallbacks) != 0 {
		t.Fatalf("verify-full has %d fallbacks; none may be plaintext", len(cc.Fallbacks))
	}
}

func TestConnConfigIgnoresPGEnvironment(t *testing.T) {
	// pgx reads libpq's variables too; the settings the API sets win.
	t.Setenv("PGHOST", "elsewhere.example")
	t.Setenv("PGPORT", "1")
	t.Setenv("PGDATABASE", "other")
	t.Setenv("PGSSLMODE", "disable")
	cc, err := validConfig().connConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cc.Host != "postgresql.atrium.svc" || cc.Port != 5432 || cc.Database != "atrium" || cc.TLSConfig == nil {
		t.Fatalf("PG* variables won: host %q port %d database %q TLS %v", cc.Host, cc.Port, cc.Database, cc.TLSConfig != nil)
	}
}

func TestBeforeConnectReadsTheCredentialEveryTime(t *testing.T) {
	dir := t.TempDir()
	credentialstest.WriteSecretVolume(t, dir, map[string]string{
		"ATRIUM_CREDENTIAL_SECRET_API_POSTGRES": "atrium_api:first\n",
	})
	hook := beforeConnect(credentials.Files{Dir: dir}, "secret/api-postgres")
	cc := &pgx.ConnConfig{}
	if err := hook(context.Background(), cc); err != nil {
		t.Fatal(err)
	}
	if cc.User != "atrium_api" || cc.Password != "first" {
		t.Fatalf("login %q / %q", cc.User, cc.Password)
	}

	credentialstest.WriteSecretVolume(t, dir, map[string]string{
		"ATRIUM_CREDENTIAL_SECRET_API_POSTGRES": "atrium_api:rotated\n",
	})
	if err := hook(context.Background(), cc); err != nil {
		t.Fatal(err)
	}
	if cc.Password != "rotated" {
		t.Fatal("the next connection does not use the rotated credential")
	}

	credentialstest.WriteSecretVolume(t, dir, map[string]string{
		"ATRIUM_CREDENTIAL_SECRET_API_POSTGRES": ":no-user\n",
	})
	if err := hook(context.Background(), cc); err == nil || !strings.Contains(err.Error(), "no username") {
		t.Fatalf("credential without a username: err = %v", err)
	}
	if err := beforeConnect(credentials.Files{Dir: dir}, "secret/absent")(context.Background(), cc); err == nil || !strings.Contains(err.Error(), "missing credential") {
		t.Fatalf("missing credential: err = %v", err)
	}
}

// A pool with nothing listening: NewPool must not connect, and the first
// query fails with the connection error, not a hang.
func TestNewPoolDoesNotConnect(t *testing.T) {
	cfg := validConfig()
	cfg.Host, cfg.Port, cfg.SSLMode = "127.0.0.1", closedPort(t), "disable"
	t.Setenv("ATRIUM_CREDENTIAL_SECRET_API_POSTGRES", "atrium:unused")
	pool, err := NewPool(cfg, credentials.Env{})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if got := pool.Config().MaxConns; got != maxConns {
		t.Fatalf("MaxConns = %d, want %d", got, maxConns)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = SchemaChecker{Queries: New(pool), Want: 1}.Check(ctx)
	if err == nil || ctx.Err() != nil {
		t.Fatalf("Check against a closed port = %v (context %v), want a prompt connection error", err, ctx.Err())
	}
}

// closedPort returns a loopback port nothing listens on.
func closedPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// fakeRow answers SchemaVersion with a fixed result.
type fakeRow struct {
	version int64
	dirty   bool
	err     error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*int64), *dest[1].(*bool) = r.version, r.dirty
	return nil
}

type fakeDB struct{ row fakeRow }

func (fakeDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("not used")
}

func (fakeDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("not used")
}

func (db fakeDB) QueryRow(context.Context, string, ...any) pgx.Row { return db.row }

func TestSchemaCheckerCheck(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  fakeRow
		want string // "" when the check passes
	}{
		{"at the wanted version", fakeRow{version: 3}, ""},
		{"newer: a previous release during a rollout", fakeRow{version: 4}, ""},
		{"older: migrations not applied", fakeRow{version: 2}, "schema version 2 is older than 3"},
		{"dirty", fakeRow{version: 3, dirty: true}, "schema version 3 is dirty"},
		{"dirty and older", fakeRow{version: 2, dirty: true}, "schema version 2 is dirty"},
		{"dirty and newer: the next release migrating", fakeRow{version: 4, dirty: true}, ""},
		{"never migrated", fakeRow{err: &pgconn.PgError{Code: "42P01", Message: `relation "schema_migrations" does not exist`}}, "no schema_migrations table"},
		{"empty table", fakeRow{err: pgx.ErrNoRows}, "schema_migrations is empty"},
		{"unreachable", fakeRow{err: errors.New("dial tcp 10.0.0.7:5432: connect: connection refused")}, "connection refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := SchemaChecker{Queries: New(fakeDB{row: tc.row}), Want: 3}.Check(context.Background())
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Check = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Check = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestLatestVersionIsTheNewestEmbeddedMigration(t *testing.T) {
	got, err := LatestVersion()
	if err != nil {
		t.Fatal(err)
	}
	want, err := migrations.Latest(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if got != int64(want) || got < 1 {
		t.Fatalf("LatestVersion = %d, want %d", got, want)
	}
}
