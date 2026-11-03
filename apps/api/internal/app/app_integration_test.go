//go:build integration

package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"heartbeat/apps/api/internal/audit"
	"heartbeat/apps/api/internal/audit/audittest"
	"heartbeat/apps/api/internal/httpapi"
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

// Logging in and out end to end, through the server Run builds, against
// PostgreSQL and Valkey: a login from the site sets the session cookie,
// the cookie names the session until logout, and every step is audited,
// with the request's ID and without a secret, and counted.
func TestRunLogsInAndOut(t *testing.T) {
	t.Parallel()
	const site, secret = "https://heartbeat.example.com", "correct horse battery staple, admin"
	pg, pgCfg, pool := testenv.MigratedPostgres(t)
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
		Site:           site,
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
	waitReadyz(t, base+"/readyz", http.StatusOK, "migrated")
	created, err := users.CreateAdmin(context.Background(), pool, password.NewHasher(password.DefaultParams, 1),
		users.NewAdmin{Email: "admin@example.com", Password: secret}, audit.CLIActor("api users create-admin", "test"), "req-create")
	if err != nil {
		t.Fatal(err)
	}

	type response struct {
		code      int
		body      string
		requestID string
		cookies   []*http.Cookie
	}
	send := func(method, path, body, token, origin string) response {
		t.Helper()
		req, err := http.NewRequest(method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if token != "" {
			req.AddCookie(&http.Cookie{Name: httpapi.SessionCookie, Value: token})
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response{resp.StatusCode, string(data), resp.Header.Get("X-Request-Id"), resp.Cookies()}
	}
	form := `{"email":"Admin@Example.com","password":"` + secret + `"}`

	if r := send(http.MethodPost, "/api/v1/session", form, "", "https://evil.example"); r.code != http.StatusForbidden {
		t.Fatalf("login from another site = %d, want 403", r.code)
	}
	failed := send(http.MethodPost, "/api/v1/session", `{"email":"admin@example.com","password":"guess"}`, "", site)
	if failed.code != http.StatusUnauthorized {
		t.Fatalf("login with a wrong password = %d, want 401: %s", failed.code, failed.body)
	}
	login := send(http.MethodPost, "/api/v1/session", form, "", site)
	if login.code != http.StatusCreated || len(login.cookies) != 1 || login.cookies[0].Name != httpapi.SessionCookie {
		t.Fatalf("login = %d %v: %s", login.code, login.cookies, login.body)
	}
	token := login.cookies[0].Value

	if r := send(http.MethodGet, "/api/v1/session", "", "", ""); r.code != http.StatusUnauthorized {
		t.Fatalf("GET /api/v1/session without the cookie = %d, want 401", r.code)
	}
	r := send(http.MethodGet, "/api/v1/session", "", token, "")
	if r.code != http.StatusOK || !strings.Contains(r.body, `"email":"admin@example.com"`) || !strings.Contains(r.body, `"roles":["admin"]`) {
		t.Fatalf("GET /api/v1/session = %d: %s", r.code, r.body)
	}
	logout := send(http.MethodDelete, "/api/v1/session", "", token, site)
	if logout.code != http.StatusNoContent {
		t.Fatalf("logout = %d: %s", logout.code, logout.body)
	}
	if r := send(http.MethodGet, "/api/v1/session", "", token, ""); r.code != http.StatusUnauthorized {
		t.Fatalf("GET /api/v1/session after logout = %d, want 401", r.code)
	}

	var hash string
	if err := pool.QueryRow(context.Background(), "select password_hash from users where id = $1", created.UserID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(context.Background(), "select record::text from audit_outbox order by seq")
	if err != nil {
		t.Fatal(err)
	}
	records, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	for _, record := range records {
		audittest.Validate(t, []byte(record))
		audittest.NoSecrets(t, []byte(record), secret, "guess", token, hash)
		var head struct {
			Action    string `json:"action"`
			RequestID string `json:"request_id"`
		}
		if err := json.Unmarshal([]byte(record), &head); err != nil {
			t.Fatal(err)
		}
		steps = append(steps, head.Action+" "+head.RequestID)
	}
	want := []string{
		"create req-create",
		"login_failed " + failed.requestID,
		"login " + login.requestID,
		"logout " + logout.requestID,
	}
	if !slices.Equal(steps, want) {
		t.Fatalf("audit records %q, want %q", steps, want)
	}

	_, metrics := get(t, base+"/metrics")
	for _, line := range []string{
		`heartbeat_api_login_attempts_total{outcome="succeeded"} 1`,
		`heartbeat_api_login_attempts_total{outcome="failed"} 1`,
		`heartbeat_api_login_attempts_total{outcome="rate_limited"} 0`,
	} {
		if !strings.Contains(metrics, line+"\n") {
			t.Errorf("/metrics lacks %q", line)
		}
	}
	for _, s := range []string{secret, token, hash} {
		if strings.Contains(logs.String(), s) {
			t.Fatal("the logs hold a secret")
		}
	}
}
