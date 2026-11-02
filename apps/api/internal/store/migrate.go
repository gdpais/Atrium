package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"heartbeat/db/migrations"
	"heartbeat/internal/credentials"
)

// migrateLockTimeout bounds each wait for another replica's migrations:
// golang-migrate holds a PostgreSQL advisory lock while it migrates, so
// replicas starting together apply each migration once, and the others wait.
// It takes the lock twice, once to create schema_migrations ([newMigrate])
// and once to migrate, and each wait ends after this long, or when the
// context is done. Longer than any migration should take, so a slow one does
// not fail the replicas waiting for it.
const migrateLockTimeout = 5 * time.Minute

// LatestVersion returns the schema version of the newest migration this
// build embeds.
func LatestVersion() (int64, error) {
	latest, err := migrations.Latest(migrations.FS)
	if err != nil {
		return 0, err
	}
	return int64(latest), nil
}

// MigrationResult is what [MigrateUp] did.
type MigrationResult struct {
	// From is the schema version before, 0 for a database never migrated.
	From uint
	// To is the schema version after.
	To uint
	// Newer is set when the database was already past the newest migration
	// this build embeds, as after rolling the API back: nothing was
	// applied.
	Newer bool
}

// MigrateUp applies, in order, every migration in db/migrations newer than
// the database's schema version, and records each in schema_migrations.
// Replicas may run it together; each migration is applied once.
//
// A database already past the newest migration this build embeds is left
// alone and is not an error, so rolling the API back to an older release
// still starts it (migrations are expand then contract). A dirty version,
// left by a migration that failed partway, is an error until repaired.
//
// When ctx is done, MigrateUp stops after the migration in progress.
func MigrateUp(ctx context.Context, cfg Config, resolver credentials.Resolver, logger *slog.Logger) (MigrationResult, error) {
	m, err := newMigrate(ctx, cfg, resolver, logger, migrateLockTimeout)
	if err != nil {
		return MigrationResult{}, err
	}
	defer closeMigrate(m, logger)

	from, dirty, err := version(m)
	if err != nil {
		return MigrationResult{}, err
	}
	latest, err := migrations.Latest(migrations.FS)
	if err != nil {
		return MigrationResult{}, err
	}
	if !dirty && uint64(from) > latest {
		return MigrationResult{From: from, To: from, Newer: true}, nil
	}

	done := make(chan error, 1)
	go func() { done <- m.Up() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		m.GracefulStop <- true
		err = <-done
		if err == nil || errors.Is(err, migrate.ErrNoChange) {
			err = ctx.Err()
		}
	}
	if err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return MigrationResult{From: from}, fmt.Errorf("migrate up from version %d: %w", from, err)
	}
	to, _, err := version(m)
	if err != nil {
		return MigrationResult{From: from}, err
	}
	return MigrationResult{From: from, To: to}, nil
}

// MigrationVersion returns the database's schema version, 0 when it was
// never migrated, and whether the migration to it is running or failed
// partway. It only reads schema_migrations, so it neither creates the table
// nor waits for a migration in progress.
func MigrationVersion(ctx context.Context, cfg Config, resolver credentials.Resolver) (uint, bool, error) {
	conn, err := Connect(ctx, cfg, resolver)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	got, err := New(conn).SchemaVersion(ctx)
	switch {
	case notMigrated(err):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("read schema version: %w", err)
	case got.Version < 0: // golang-migrate's NilVersion
		return 0, got.Dirty, nil
	}
	return uint(got.Version), got.Dirty, nil
}

// version is m.Version, with 0 for a database never migrated.
func version(m *migrate.Migrate) (uint, bool, error) {
	v, dirty, err := m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read schema version: %w", err)
	}
	return v, dirty, nil
}

// newMigrate connects to the database and returns golang-migrate set up
// with the embedded migrations. It creates schema_migrations when missing.
//
// golang-migrate takes its advisory lock to create the table, and waits for
// it without a bound or a context, so a replica started while another one
// migrates would wait here as long as that migration runs. newMigrate waits
// at most lockTimeout, or until ctx is done, and then closes the connection
// to stop the wait. Nothing has run on it yet that closing could interrupt.
func newMigrate(ctx context.Context, cfg Config, resolver credentials.Resolver, logger *slog.Logger, lockTimeout time.Duration) (*migrate.Migrate, error) {
	connConfig, err := cfg.connConfig()
	if err != nil {
		return nil, err
	}
	sockets := &sockets{dial: connConfig.DialFunc}
	connConfig.DialFunc = sockets.Dial
	db := stdlib.OpenDB(*connConfig, stdlib.OptionBeforeConnect(beforeConnect(resolver, cfg.CredentialRef)))

	done := make(chan opened, 1)
	go func() {
		driver, err := migratepgx.WithInstance(db, &migratepgx.Config{})
		done <- opened{driver, err}
	}()
	timer := time.NewTimer(lockTimeout)
	defer timer.Stop()
	var result opened
	select {
	case result = <-done:
	case <-ctx.Done():
		result = abandon(done, sockets, ctx.Err())
	case <-timer.C:
		result = abandon(done, sockets, fmt.Errorf("another replica has held the migration lock for more than %s", lockTimeout))
	}
	if result.err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect to postgresql: %w", result.err)
	}
	driver := result.driver

	source, err := iofs.New(migrations.FS, ".")
	if err != nil {
		_ = driver.Close()
		return nil, err
	}
	m, err := migrate.NewWithInstance("iofs", source, "pgx5", driver)
	if err != nil {
		_ = source.Close()
		_ = driver.Close()
		return nil, err
	}
	m.LockTimeout = lockTimeout
	m.Log = migrateLogger{logger: logger}
	return m, nil
}

// opened is what migratepgx.WithInstance returned.
type opened struct {
	driver database.Driver
	err    error
}

// abandon closes every connection s dialled, which ends the wait of
// migratepgx.WithInstance, waits for it to return, and fails with reason
// even when the driver opened in the meantime.
func abandon(done <-chan opened, s *sockets, reason error) opened {
	s.closeAll()
	if result := <-done; result.err == nil {
		_ = result.driver.Close()
	}
	return opened{err: reason}
}

// sockets records the connections pgx dials, so they can be closed from
// another goroutine; once they are, it dials no more.
type sockets struct {
	dial   pgconn.DialFunc
	mu     sync.Mutex
	conns  []net.Conn
	closed bool
}

// Dial implements pgconn.DialFunc.
func (s *sockets) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	conn, err := s.dial(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		_ = conn.Close()
		return nil, net.ErrClosed
	}
	s.conns = append(s.conns, conn)
	return conn, nil
}

// closeAll closes every connection dialled so far, and the ones dialled
// later.
func (s *sockets) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for _, conn := range s.conns {
		_ = conn.Close()
	}
}

func closeMigrate(m *migrate.Migrate, logger *slog.Logger) {
	sourceErr, dbErr := m.Close()
	if err := errors.Join(sourceErr, dbErr); err != nil {
		logger.Warn("migrate: close", "error", err)
	}
}

// migrateLogger sends golang-migrate's progress lines to slog.
type migrateLogger struct {
	logger *slog.Logger
}

func (l migrateLogger) Printf(format string, v ...any) {
	l.logger.Info("migrate: " + strings.TrimSpace(fmt.Sprintf(format, v...)))
}

func (migrateLogger) Verbose() bool { return false }
