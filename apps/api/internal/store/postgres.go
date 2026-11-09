// Package store is the API's access to PostgreSQL (ADR 0013): a pgx
// connection pool whose credential is resolved on every new connection, the
// typed queries sqlc generates from queries/*.sql (the *.gen.go files), and
// the golang-migrate runner that applies db/migrations.
//
// Run `make generate` after editing a query or a migration; `make test`
// fails while the generated code is stale.
package store

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"atrium/internal/credentials"
)

// SSLModes are the values [Config.SSLMode] accepts: libpq's, without
// "allow" and "prefer", which fall back to plaintext without saying so.
var SSLModes = []string{"disable", "require", "verify-ca", "verify-full"}

// Pool settings. The API is a small control plane: a few connections per
// replica leave the database's connection limit for the other replicas and
// the workers, whatever the node's CPU count (pgxpool's default).
const (
	maxConns = 8
	// connectTimeout bounds one connection attempt, dial and TLS and
	// authentication included, when the caller's context does not end
	// sooner.
	connectTimeout = 5 * time.Second
	// applicationName shows in pg_stat_activity.
	applicationName = "atrium-api"
)

// Config says where the API's PostgreSQL database is and how to log in.
type Config struct {
	// Host is the server's DNS name or IP address.
	Host string
	// Port is the server's TCP port.
	Port int
	// Database is the database name.
	Database string
	// SSLMode is one of disable, require, verify-ca and verify-full, with
	// libpq's meaning.
	SSLMode string
	// SSLRootCert, when set, is a file of PEM CA certificates that
	// verify-ca and verify-full trust instead of the system's, such as the
	// Amazon RDS bundle.
	SSLRootCert string
	// CredentialRef names the "username:password" credential the login
	// uses (package credentials).
	CredentialRef string
}

// Validate reports the first setting that cannot work.
func (c Config) Validate() error {
	switch {
	case c.Host == "":
		return errors.New("no host")
	case c.Port < 1 || c.Port > 65535:
		return fmt.Errorf("port %d: want 1-65535", c.Port)
	case c.Database == "":
		return errors.New("no database name")
	case !slices.Contains(SSLModes, c.SSLMode):
		return fmt.Errorf("sslmode %q: want one of %v", c.SSLMode, SSLModes)
	case c.CredentialRef == "":
		return errors.New("no credential reference")
	}
	return nil
}

// connString returns the connection URL without credentials: the login is
// set per connection by [beforeConnect].
func (c Config) connString() string {
	query := url.Values{
		"sslmode":          {c.SSLMode},
		"application_name": {applicationName},
		"connect_timeout":  {strconv.Itoa(int(connectTimeout / time.Second))},
	}
	if c.SSLRootCert != "" {
		query.Set("sslrootcert", c.SSLRootCert)
	}
	u := url.URL{
		Scheme:   "postgres",
		Host:     net.JoinHostPort(c.Host, strconv.Itoa(c.Port)),
		Path:     "/" + c.Database,
		RawQuery: query.Encode(),
	}
	return u.String()
}

// connConfig parses the connection settings. The values Config sets win
// over libpq's PG* environment variables, which pgx also reads; the ones it
// leaves unset, such as PGSSLROOTCERT when SSLRootCert is empty, or
// PGSSLCERT, still apply.
func (c Config) connConfig() (*pgx.ConnConfig, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	cfg, err := pgx.ParseConfig(c.connString())
	if err != nil {
		return nil, fmt.Errorf("postgresql settings: %w", err)
	}
	return cfg, nil
}

// beforeConnect returns a hook that sets the login of each new connection
// from the credential ref names, read again every time, so a rotated
// credential is used by the next connection without a restart.
func beforeConnect(resolver credentials.Resolver, ref string) func(context.Context, *pgx.ConnConfig) error {
	return func(ctx context.Context, cfg *pgx.ConnConfig) error {
		creds, err := resolver.Resolve(ctx, ref)
		if err != nil {
			return err
		}
		if creds.Username == "" {
			return fmt.Errorf("credential %s has no username", ref)
		}
		cfg.User, cfg.Password = creds.Username, creds.Password
		return nil
	}
}

// NewPool returns a connection pool for cfg. It does not connect: the first
// query does, so the API starts while PostgreSQL is down and becomes ready
// once it is up. It fails only on settings that cannot work.
func NewPool(cfg Config, resolver credentials.Resolver) (*pgxpool.Pool, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	poolConfig, err := pgxpool.ParseConfig(cfg.connString())
	if err != nil {
		return nil, fmt.Errorf("postgresql settings: %w", err)
	}
	poolConfig.MaxConns = maxConns
	poolConfig.BeforeConnect = beforeConnect(resolver, cfg.CredentialRef)
	poolConfig.ConnConfig.Tracer = queryTracer{}
	return pgxpool.NewWithConfig(context.Background(), poolConfig)
}

// Connect opens one connection for cfg, for a command that runs a few
// queries and exits. The caller closes it.
func Connect(ctx context.Context, cfg Config, resolver credentials.Resolver) (*pgx.Conn, error) {
	connConfig, err := cfg.connConfig()
	if err != nil {
		return nil, err
	}
	if err := beforeConnect(resolver, cfg.CredentialRef)(ctx, connConfig); err != nil {
		return nil, err
	}
	conn, err := pgx.ConnectConfig(ctx, connConfig)
	if err != nil {
		return nil, fmt.Errorf("connect to postgresql: %w", err)
	}
	return conn, nil
}

// SchemaChecker tells whether the database is reachable and its schema is
// at least the version this build of the API was made for.
type SchemaChecker struct {
	// Queries runs the generated queries, normally on the pool.
	Queries *Queries
	// Want is the schema version the API needs: the latest migration it
	// embeds.
	Want int64
}

// Check returns nil when the schema version is at least Want and the
// migrations up to Want are complete. A newer version passes: migrations are
// expand then contract, so a replica of the previous release keeps working
// while the next one rolls out.
//
// golang-migrate marks a version dirty before it runs that migration, and
// clean once it succeeds, so a dirty version V means the schema is complete
// up to V-1. A dirty version above Want, a newer release migrating or one
// whose migration failed, therefore still passes: otherwise every replica
// would leave its Service for as long as each migration runs.
func (s SchemaChecker) Check(ctx context.Context) error {
	got, err := s.Queries.SchemaVersion(ctx)
	switch {
	case isUndefinedTable(err):
		return errors.New("schema not migrated: no schema_migrations table; run api migrate up")
	case errors.Is(err, pgx.ErrNoRows):
		return errors.New("schema not migrated: schema_migrations is empty; run api migrate up")
	case err != nil:
		return err
	case got.Dirty && got.Version <= s.Want:
		return fmt.Errorf("schema version %d is dirty: its migration is running, or failed partway and needs repair", got.Version)
	case got.Version < s.Want:
		return fmt.Errorf("schema version %d is older than %d, the version this API needs; run api migrate up", got.Version, s.Want)
	}
	return nil
}

// notMigrated tells whether err, from the SchemaVersion query, means the
// database was never migrated: schema_migrations is missing or empty.
func notMigrated(err error) bool {
	return isUndefinedTable(err) || errors.Is(err, pgx.ErrNoRows)
}

func isUndefinedTable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42P01" // undefined_table
}
