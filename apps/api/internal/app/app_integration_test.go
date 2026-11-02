//go:build integration

package app

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"heartbeat/apps/api/internal/audit"
	"heartbeat/apps/api/internal/audit/audittest"
	"heartbeat/apps/api/internal/password"
	"heartbeat/apps/api/internal/store"
	"heartbeat/apps/api/internal/testenv"
	"heartbeat/apps/api/internal/users"
	"heartbeat/internal/credentials"
	"heartbeat/internal/credentials/credentialstest"
)

// The API against real PostgreSQL and Valkey, with its credentials in a
// mounted-Secret-style directory: not ready until the schema is migrated,
// ready once it is, unready while either dependency stops answering, and
// ready again when it comes back, without a restart.
func TestRunAgainstPostgresAndValkey(t *testing.T) {
	t.Parallel()
	pg, pgCfg := testenv.Postgres(t)
	valkey, redisCfg := testenv.Valkey(t)
	dir := t.TempDir()
	credentialstest.WriteSecretVolume(t, dir, map[string]string{
		credentials.Key(testenv.PostgresRef): pg.Credential.Username + ":" + pg.Credential.Password + "\n",
		credentials.Key(testenv.RedisRef):    valkey.Credential.Username + ":" + valkey.Credential.Password + "\n",
	})

	var logs syncBuffer
	deps, err := newRunDeps(Config{
		ListenAddr:     "127.0.0.1:0",
		Postgres:       pgCfg,
		Redis:          redisCfg,
		CredentialsDir: dir,
		Logger:         slog.New(slog.NewJSONHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	addr := make(chan net.Addr, 1)
	deps.onListen = func(a net.Addr) { addr <- a }
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- run(ctx, deps) }()
	t.Cleanup(func() {
		cancel()
		if err := <-result; err != nil {
			t.Errorf("run returned %v", err)
		}
	})
	readyz := "http://" + (<-addr).String() + "/readyz"

	waitReadyz(t, readyz, http.StatusServiceUnavailable, "before migrating")
	if !strings.Contains(logs.String(), "schema not migrated") {
		t.Fatalf("logs do not say the schema is not migrated:\n%s", logs.String())
	}
	if _, err := store.MigrateUp(context.Background(), pgCfg, credentials.Files{Dir: dir}, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	waitReadyz(t, readyz, http.StatusOK, "after migrating")

	for _, dep := range []struct {
		name      string
		container testenv.Container
	}{{"valkey", valkey}, {"postgresql", pg}} {
		dep.container.Pause(t)
		waitReadyz(t, readyz, http.StatusServiceUnavailable, dep.name+" paused")
		dep.container.Unpause(t)
		waitReadyz(t, readyz, http.StatusOK, dep.name+" back")
	}
	for _, want := range []string{`postgresql: `, `redis: `} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs never name %q as failing:\n%s", want, logs.String())
		}
	}
	for _, secret := range []string{pg.Credential.Password, valkey.Credential.Password} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("logs contain a password")
		}
	}
}

// waitReadyz polls GET /readyz until it answers want, for up to 30
// seconds: a paused dependency takes the 2s readiness timeout to fail, and
// one that comes back may take a few probes.
func waitReadyz(t *testing.T, url string, want int, when string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	last := 0
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			last = resp.StatusCode
			_ = resp.Body.Close()
			if last == want {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("%s: GET /readyz = %d for 30s, want %d", when, last, want)
}

// With an audit endpoint, the API delivers each committed audit record to
// the OTel Collector, deletes it from the outbox, and says so in /metrics.
func TestRunDeliversAuditRecords(t *testing.T) {
	t.Parallel()
	pg, pgCfg, pool := testenv.MigratedPostgres(t)
	valkey, redisCfg := testenv.Valkey(t)
	collector := audittest.NewCollector(t)
	dir := t.TempDir()
	credentialstest.WriteSecretVolume(t, dir, map[string]string{
		credentials.Key(testenv.PostgresRef): pg.Credential.Username + ":" + pg.Credential.Password + "\n",
		credentials.Key(testenv.RedisRef):    valkey.Credential.Username + ":" + valkey.Credential.Password + "\n",
	})
	var logs syncBuffer
	deps, err := newRunDeps(Config{
		ListenAddr:     "127.0.0.1:0",
		Postgres:       pgCfg,
		Redis:          redisCfg,
		CredentialsDir: dir,
		AuditEndpoint:  collector.URL,
		Logger:         slog.New(slog.NewJSONHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	addr := make(chan net.Addr, 1)
	deps.onListen = func(a net.Addr) { addr <- a }
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- run(ctx, deps) }()
	t.Cleanup(func() {
		cancel()
		if err := <-result; err != nil {
			t.Errorf("run returned %v", err)
		}
	})
	base := "http://" + (<-addr).String()

	created, err := users.CreateAdmin(context.Background(), pool, password.NewHasher(password.DefaultParams, 1),
		users.NewAdmin{Email: "admin@example.com", Password: "correct horse battery staple, admin"},
		audit.CLIActor("api users create-admin", "test"), "req-1")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for len(collector.Records()) == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	records := collector.Records()
	if len(records) != 1 || audittest.Attribute(records[0], audit.AttributeRecordID) != created.AuditRecord.ID {
		t.Fatalf("the Collector got %d records, want the admin's creation\n%s", len(records), logs.String())
	}
	audittest.Validate(t, []byte(records[0].Body.GetStringValue()))
	var metrics string
	for time.Now().Before(deadline) {
		_, metrics = get(t, base+"/metrics")
		if strings.Contains(metrics, audit.MetricRecordsDelivered+" 1\n") && strings.Contains(metrics, audit.MetricOutboxRows+" 0\n") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, want := range []string{audit.MetricRecordsDelivered + " 1\n", audit.MetricOutboxRows + " 0\n", audit.MetricOutboxOldestAge + " 0\n"} {
		if !strings.Contains(metrics, want) {
			t.Errorf("/metrics lacks %q", want)
		}
	}
	if !strings.Contains(logs.String(), `"msg":"audit delivery on"`) {
		t.Errorf("logs do not say audit delivery is on:\n%s", logs.String())
	}
}
