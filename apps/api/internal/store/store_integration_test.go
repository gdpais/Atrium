//go:build integration

package store_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4/database"

	"atrium/apps/api/internal/store"
	"atrium/apps/api/internal/testenv"
)

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// golang-migrate applies every embedded migration to an empty postgres:18
// database, records the version, and the schema it builds is the one the
// psql-applied tests in tests/ check. Running it again changes nothing.
func TestMigrateUpAppliesTheMigrations(t *testing.T) {
	t.Parallel()
	pg, cfg := testenv.Postgres(t)
	resolver := testenv.Resolver{testenv.PostgresRef: pg.Credential}
	ctx := context.Background()
	latest, err := store.LatestVersion()
	if err != nil {
		t.Fatal(err)
	}

	if v, dirty, err := store.MigrationVersion(context.Background(), cfg, resolver); err != nil || v != 0 || dirty {
		t.Fatalf("fresh database: version %d dirty %v err %v, want 0", v, dirty, err)
	}
	result, err := store.MigrateUp(ctx, cfg, resolver, discard())
	if err != nil {
		t.Fatal(err)
	}
	if result.From != 0 || int64(result.To) != latest || result.Newer {
		t.Fatalf("MigrateUp = %+v, want 0 to %d", result, latest)
	}
	if v, dirty, err := store.MigrationVersion(context.Background(), cfg, resolver); err != nil || int64(v) != latest || dirty {
		t.Fatalf("after up: version %d dirty %v err %v, want %d", v, dirty, err, latest)
	}
	tables := pg.Exec(t, "psql", "-U", "atrium", "-d", "atrium", "-tAc",
		"select string_agg(coalesce(to_regclass(name)::text, '-'), ',') from unnest(array['users','applications','investigations','report_runs','schema_migrations']) as name")
	if strings.TrimSpace(tables) != "users,applications,investigations,report_runs,schema_migrations" {
		t.Fatalf("tables after migrate up: %s", tables)
	}

	again, err := store.MigrateUp(ctx, cfg, resolver, discard())
	if err != nil {
		t.Fatalf("second MigrateUp: %v", err)
	}
	if int64(again.From) != latest || again.To != again.From {
		t.Fatalf("second MigrateUp = %+v, want no change at %d", again, latest)
	}
}

// Replicas whose init containers start together apply each migration once:
// golang-migrate serialises them with an advisory lock.
func TestMigrateUpConcurrently(t *testing.T) {
	t.Parallel()
	pg, cfg := testenv.Postgres(t)
	resolver := testenv.Resolver{testenv.PostgresRef: pg.Credential}
	latest, err := store.LatestVersion()
	if err != nil {
		t.Fatal(err)
	}
	const replicas = 4
	var wg sync.WaitGroup
	errs := make(chan error, replicas)
	for range replicas {
		wg.Go(func() {
			result, err := store.MigrateUp(context.Background(), cfg, resolver, discard())
			if err == nil && int64(result.To) != latest {
				t.Errorf("MigrateUp ended at %d, want %d", result.To, latest)
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent MigrateUp: %v", err)
		}
	}
	if rows := strings.TrimSpace(pg.Exec(t, "psql", "-U", "atrium", "-d", "atrium", "-tAc", "select count(*) from schema_migrations")); rows != "1" {
		t.Fatalf("schema_migrations has %s rows, want 1", rows)
	}
}

// A replica started while another one migrates waits for the advisory
// lock, but no longer than its context: golang-migrate's own wait, to
// create schema_migrations, has no bound.
func TestMigrateUpStopsWaitingForTheLock(t *testing.T) {
	t.Parallel()
	pg, cfg := testenv.Postgres(t)
	resolver := testenv.Resolver{testenv.PostgresRef: pg.Credential}
	pool, err := store.NewPool(cfg, resolver)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	// The lock golang-migrate's pgx v5 driver takes.
	lockID, err := database.GenerateAdvisoryLockId(cfg.Database, "public", "schema_migrations")
	if err != nil {
		t.Fatal(err)
	}
	holder, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	if _, err := holder.Exec(context.Background(), "select pg_advisory_lock($1)", lockID); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	_, err = store.MigrateUp(ctx, cfg, resolver, discard())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("MigrateUp while the lock is held = %v, want the context's deadline", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("MigrateUp returned after %s, want soon after the 2s deadline", elapsed)
	}

	if _, err := holder.Exec(context.Background(), "select pg_advisory_unlock($1)", lockID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MigrateUp(context.Background(), cfg, resolver, discard()); err != nil {
		t.Fatalf("MigrateUp once the lock is free: %v", err)
	}
}

// Rolling the API back to an older release runs that release's
// `api migrate up` against a newer schema: it must start, not fail.
func TestMigrateUpLeavesANewerSchemaAlone(t *testing.T) {
	t.Parallel()
	pg, cfg := testenv.Postgres(t)
	resolver := testenv.Resolver{testenv.PostgresRef: pg.Credential}
	if _, err := store.MigrateUp(context.Background(), cfg, resolver, discard()); err != nil {
		t.Fatal(err)
	}
	pg.Exec(t, "psql", "-U", "atrium", "-d", "atrium", "-c", "update schema_migrations set version = 9999")
	result, err := store.MigrateUp(context.Background(), cfg, resolver, discard())
	if err != nil {
		t.Fatalf("MigrateUp on a newer schema: %v", err)
	}
	if !result.Newer || result.To != 9999 {
		t.Fatalf("MigrateUp = %+v, want Newer at 9999", result)
	}

	// A dirty version is not skipped: it needs repair first.
	pg.Exec(t, "psql", "-U", "atrium", "-d", "atrium", "-c", "update schema_migrations set version = 1, dirty = true")
	if _, err := store.MigrateUp(context.Background(), cfg, resolver, discard()); err == nil || !strings.Contains(err.Error(), "Dirty") {
		t.Fatalf("MigrateUp on a dirty schema = %v, want golang-migrate's dirty error", err)
	}
}

// The readiness check reads golang-migrate's own table through the
// sqlc-generated query, so sqlc's idea of schema_migrations
// (schema/schema_migrations.sql) must match what golang-migrate creates.
func TestSchemaCheckerAgainstPostgres(t *testing.T) {
	t.Parallel()
	pg, cfg := testenv.Postgres(t)
	resolver := testenv.Resolver{testenv.PostgresRef: pg.Credential}
	pool, err := store.NewPool(cfg, resolver)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	latest, err := store.LatestVersion()
	if err != nil {
		t.Fatal(err)
	}
	checker := store.SchemaChecker{Queries: store.New(pool), Want: latest}
	check := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return checker.Check(ctx)
	}

	if err := check(); err == nil || !strings.Contains(err.Error(), "no schema_migrations table") {
		t.Fatalf("before migrating: Check = %v", err)
	}
	if _, err := store.MigrateUp(context.Background(), cfg, resolver, discard()); err != nil {
		t.Fatal(err)
	}
	if err := check(); err != nil {
		t.Fatalf("after migrating: Check = %v", err)
	}
	pg.Exec(t, "psql", "-U", "atrium", "-d", "atrium", "-c", "update schema_migrations set dirty = true")
	if err := check(); err == nil || !strings.Contains(err.Error(), "dirty") {
		t.Fatalf("dirty: Check = %v", err)
	}
	// The next release's migration is running: this build's schema is
	// complete, so it stays ready.
	pg.Exec(t, "psql", "-U", "atrium", "-d", "atrium", "-c", fmt.Sprintf("update schema_migrations set version = %d, dirty = true", latest+1))
	if err := check(); err != nil {
		t.Fatalf("dirty newer version: Check = %v", err)
	}
}

// The pool reads the credential for every new connection: after the
// password changes in PostgreSQL and in the credential, new connections
// log in with the new one, without a new pool.
func TestPoolUsesARotatedCredential(t *testing.T) {
	t.Parallel()
	pg, cfg := testenv.Postgres(t)
	resolver := testenv.Resolver{testenv.PostgresRef: pg.Credential}
	pool, err := store.NewPool(cfg, resolver)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx := context.Background()
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}

	rotated := pg.Credential
	rotated.Password += "-rotated"
	pg.Exec(t, "psql", "-U", "atrium", "-d", "atrium", "-c", "alter role atrium password '"+rotated.Password+"'")
	pool.Reset()
	if err := pool.Ping(ctx); err == nil || !strings.Contains(err.Error(), "password authentication failed") {
		t.Fatalf("old credential after the password changed: Ping = %v", err)
	}
	resolver[testenv.PostgresRef] = rotated
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("rotated credential: Ping = %v", err)
	}
}
