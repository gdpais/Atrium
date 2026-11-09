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
	"github.com/jackc/pgx/v5/pgxpool"

	"atrium/apps/api/internal/audit"
	"atrium/apps/api/internal/audit/audittest"
	"atrium/apps/api/internal/httpapi"
	"atrium/apps/api/internal/password"
	"atrium/apps/api/internal/store"
	"atrium/apps/api/internal/testenv"
	"atrium/apps/api/internal/users"
	"atrium/internal/credentials"
	"atrium/internal/credentials/credentialstest"
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
	const secret = "correct horse battery staple, admin"
	a := startAPI(t)
	base, pool, logs := a.base, a.pool, a.logs
	send := func(method, path, body, token, origin string) response {
		t.Helper()
		return a.send(t, method, path, body, token, origin)
	}
	created, err := users.CreateAdmin(context.Background(), pool, password.NewHasher(password.DefaultParams, 1),
		users.NewAdmin{Email: "admin@example.com", Password: secret}, audit.CLIActor("api users create-admin", "test"), "req-create")
	if err != nil {
		t.Fatal(err)
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
		`atrium_api_login_attempts_total{outcome="succeeded"} 1`,
		`atrium_api_login_attempts_total{outcome="failed"} 1`,
		`atrium_api_login_attempts_total{outcome="rate_limited"} 0`,
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

// site is the origin the API under test serves.
const site = "https://atrium.example.com"

// api is the API, run against its own PostgreSQL and Valkey, with site
// as its origin.
type api struct {
	base string
	pool *pgxpool.Pool
	logs *syncBuffer
}

func startAPI(t *testing.T) api {
	t.Helper()
	pg, pgCfg, pool := testenv.MigratedPostgres(t)
	valkey, redisCfg := testenv.Valkey(t)
	dir := t.TempDir()
	credentialstest.WriteSecretVolume(t, dir, map[string]string{
		credentials.Key(testenv.PostgresRef): pg.Credential.Username + ":" + pg.Credential.Password + "\n",
		credentials.Key(testenv.RedisRef):    valkey.Credential.Username + ":" + valkey.Credential.Password + "\n",
	})
	logs := &syncBuffer{}
	deps, err := newRunDeps(Config{
		ListenAddr:     "127.0.0.1:0",
		Postgres:       pgCfg,
		Redis:          redisCfg,
		CredentialsDir: dir,
		Site:           site,
		Logger:         slog.New(slog.NewJSONHandler(logs, nil)),
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
	a := api{base: "http://" + (<-addr).String(), pool: pool, logs: logs}
	waitReadyz(t, a.base+"/readyz", http.StatusOK, "migrated")
	return a
}

// response is what send got back.
type response struct {
	code      int
	body      string
	requestID string
	cookies   []*http.Cookie
}

// send sends a JSON request with the session cookie token, when set, and
// an Origin header, when set.
func (a api) send(t *testing.T, method, path, body, token, origin string) response {
	t.Helper()
	req, err := http.NewRequest(method, a.base+path, strings.NewReader(body))
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

// login logs email in and returns the session cookie's value.
func (a api) login(t *testing.T, email, secret string) string {
	t.Helper()
	r := a.send(t, http.MethodPost, "/api/v1/session", `{"email":"`+email+`","password":"`+secret+`"}`, "", site)
	if r.code != http.StatusCreated || len(r.cookies) != 1 {
		t.Fatalf("login as %s = %d: %s", email, r.code, r.body)
	}
	return r.cookies[0].Value
}

// Admins manage users end to end, through the server Run builds: a new
// operator logs in but may not manage users; disabling them ends their
// session; a new password ends it too and replaces the old one; a user
// changes their own; the last active admin keeps the role. Each change is
// audited without a secret.
func TestRunManagesUsers(t *testing.T) {
	t.Parallel()
	const rootPW, bobPW, resetPW, ownPW = "correct horse battery staple, root", "correct horse battery staple, bob",
		"correct horse battery staple, reset", "correct horse battery staple, own"
	a := startAPI(t)
	send := func(method, path, body, token string) response {
		t.Helper()
		return a.send(t, method, path, body, token, site)
	}
	root, err := users.CreateAdmin(context.Background(), a.pool, password.NewHasher(password.DefaultParams, 1),
		users.NewAdmin{Email: "root@example.com", Password: rootPW}, audit.CLIActor("api users create-admin", "test"), "req-create")
	if err != nil {
		t.Fatal(err)
	}
	admin := a.login(t, "root@example.com", rootPW)

	r := send(http.MethodPost, "/api/v1/users", `{"email":"Bob@Example.com","display_name":"Bob","password":"`+bobPW+`","roles":["operator"]}`, admin)
	if r.code != http.StatusCreated || !strings.Contains(r.body, `"email":"bob@example.com"`) || !strings.Contains(r.body, `"roles":["operator"]`) {
		t.Fatalf("create bob = %d: %s", r.code, r.body)
	}
	var bob struct{ ID string }
	if err := json.Unmarshal([]byte(r.body), &bob); err != nil {
		t.Fatal(err)
	}
	if r := send(http.MethodPost, "/api/v1/users", `{"email":"BOB@example.com","password":"`+bobPW+`","roles":["viewer"]}`, admin); r.code != http.StatusConflict {
		t.Fatalf("create a second bob = %d, want 409", r.code)
	}

	bobSession := a.login(t, "bob@example.com", bobPW)
	if r := send(http.MethodGet, "/api/v1/users", "", bobSession); r.code != http.StatusForbidden {
		t.Fatalf("an operator lists users = %d, want 403", r.code)
	}
	if r := send(http.MethodGet, "/api/v1/users?status=active", "", admin); r.code != http.StatusOK || !strings.Contains(r.body, "bob@example.com") {
		t.Fatalf("list = %d: %s", r.code, r.body)
	}

	// Disabled: the session ends at once, and logins are refused.
	if r := send(http.MethodPatch, "/api/v1/users/"+bob.ID, `{"status":"disabled"}`, admin); r.code != http.StatusOK || !strings.Contains(r.body, `"status":"disabled"`) {
		t.Fatalf("disable bob = %d: %s", r.code, r.body)
	}
	if r := send(http.MethodGet, "/api/v1/session", "", bobSession); r.code != http.StatusUnauthorized {
		t.Fatalf("bob's session after disabling = %d, want 401", r.code)
	}
	if r := send(http.MethodPost, "/api/v1/session", `{"email":"bob@example.com","password":"`+bobPW+`"}`, ""); r.code != http.StatusUnauthorized {
		t.Fatalf("a disabled user's login = %d, want 401", r.code)
	}
	if r := send(http.MethodPatch, "/api/v1/users/"+bob.ID, `{"status":"active"}`, admin); r.code != http.StatusOK {
		t.Fatalf("enable bob = %d", r.code)
	}

	// A new password ends the session and replaces the old one.
	bobSession = a.login(t, "bob@example.com", bobPW)
	if r := send(http.MethodPut, "/api/v1/users/"+bob.ID+"/password", `{"password":"`+resetPW+`"}`, admin); r.code != http.StatusNoContent {
		t.Fatalf("set bob's password = %d: %s", r.code, r.body)
	}
	if r := send(http.MethodGet, "/api/v1/session", "", bobSession); r.code != http.StatusUnauthorized {
		t.Fatalf("bob's session after a new password = %d, want 401", r.code)
	}
	if r := send(http.MethodPost, "/api/v1/session", `{"email":"bob@example.com","password":"`+bobPW+`"}`, ""); r.code != http.StatusUnauthorized {
		t.Fatalf("login with the old password = %d, want 401", r.code)
	}

	// Bob changes his own: this session ends too.
	bobSession = a.login(t, "bob@example.com", resetPW)
	if r := send(http.MethodPut, "/api/v1/account/password", `{"current_password":"wrong horse","new_password":"`+ownPW+`"}`, bobSession); r.code != http.StatusForbidden {
		t.Fatalf("own password with a wrong current one = %d, want 403", r.code)
	}
	r = send(http.MethodPut, "/api/v1/account/password", `{"current_password":"`+resetPW+`","new_password":"`+ownPW+`"}`, bobSession)
	if r.code != http.StatusNoContent || len(r.cookies) != 1 || r.cookies[0].MaxAge >= 0 {
		t.Fatalf("own password = %d %v: %s", r.code, r.cookies, r.body)
	}
	if r := send(http.MethodGet, "/api/v1/session", "", bobSession); r.code != http.StatusUnauthorized {
		t.Fatalf("bob's session after changing his password = %d, want 401", r.code)
	}
	a.login(t, "bob@example.com", ownPW)

	// Roles: bob becomes a viewer; root cannot lose admin, being the only one.
	if r := send(http.MethodPut, "/api/v1/users/"+bob.ID+"/roles/viewer", "", admin); r.code != http.StatusOK || !strings.Contains(r.body, `"roles":["operator","viewer"]`) {
		t.Fatalf("grant viewer = %d: %s", r.code, r.body)
	}
	if r := send(http.MethodDelete, "/api/v1/users/"+bob.ID+"/roles/operator", "", admin); r.code != http.StatusOK || !strings.Contains(r.body, `"roles":["viewer"]`) {
		t.Fatalf("revoke operator = %d: %s", r.code, r.body)
	}
	if r := send(http.MethodDelete, "/api/v1/users/"+root.UserID+"/roles/admin", "", admin); r.code != http.StatusConflict || !strings.Contains(r.body, "last active admin") {
		t.Fatalf("revoke the last admin's role = %d: %s", r.code, r.body)
	}
	if r := send(http.MethodPatch, "/api/v1/users/"+root.UserID, `{"status":"disabled"}`, admin); r.code != http.StatusConflict {
		t.Fatalf("disable the last admin = %d, want 409", r.code)
	}

	var hashes []string
	rows, err := a.pool.Query(context.Background(), "select password_hash from users")
	if err != nil {
		t.Fatal(err)
	}
	if hashes, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatal(err)
	}
	rows, err = a.pool.Query(context.Background(), "select record::text from audit_outbox order by seq")
	if err != nil {
		t.Fatal(err)
	}
	records, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, record := range records {
		audittest.Validate(t, []byte(record))
		audittest.NoSecrets(t, []byte(record), append(hashes, rootPW, bobPW, resetPW, ownPW, admin, bobSession)...)
		var head struct {
			Action     string `json:"action"`
			EntityType string `json:"entity_type"`
		}
		if err := json.Unmarshal([]byte(record), &head); err != nil {
			t.Fatal(err)
		}
		if head.EntityType == "user" {
			actions = append(actions, head.Action)
		}
	}
	want := []string{"create", "create", "update", "update", "set_password", "set_password_failed", "set_password", "grant_role", "revoke_role"}
	if !slices.Equal(actions, want) {
		t.Fatalf("user audit records %v, want %v", actions, want)
	}
	for _, s := range []string{rootPW, bobPW, resetPW, ownPW} {
		if strings.Contains(a.logs.String(), s) {
			t.Fatal("the logs hold a password")
		}
	}
}
