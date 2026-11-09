//go:build integration

package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"atrium/apps/api/internal/audit"
	"atrium/apps/api/internal/audit/audittest"
	"atrium/apps/api/internal/httpapi"
	"atrium/apps/api/internal/password"
	"atrium/apps/api/internal/testenv"
	"atrium/apps/api/internal/users"
	atriumconfig "atrium/internal/config"
	"atrium/internal/credentials"
	"atrium/internal/credentials/credentialstest"
)

// inventoryClient sends requests to the API as a browser on the site does.
type inventoryClient struct {
	t    *testing.T
	base string
	site string
}

type inventoryResponse struct {
	code      int
	body      string
	requestID string
	cookies   []*http.Cookie
}

func (c inventoryClient) send(method, path, body, token string) inventoryResponse {
	c.t.Helper()
	req, err := http.NewRequest(method, c.base+path, strings.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if method != http.MethodGet {
		req.Header.Set("Origin", c.site)
	}
	if token != "" {
		req.AddCookie(&http.Cookie{Name: httpapi.SessionCookie, Value: token})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	return inventoryResponse{resp.StatusCode, string(data), resp.Header.Get("X-Request-Id"), resp.Cookies()}
}

// expect sends a request and fails unless it is answered with code; it
// returns the response, with its body decoded into out when out is set.
func (c inventoryClient) expect(code int, method, path, body, token string, out any) inventoryResponse {
	c.t.Helper()
	r := c.send(method, path, body, token)
	if r.code != code {
		c.t.Fatalf("%s %s = %d, want %d: %s", method, path, r.code, code, r.body)
	}
	if out != nil {
		if err := json.Unmarshal([]byte(r.body), out); err != nil {
			c.t.Fatalf("%s %s: %v: %s", method, path, err, r.body)
		}
	}
	return r
}

// userWithRole creates a user who has only role and logs them in,
// returning their ID and session token.
func userWithRole(t *testing.T, c inventoryClient, pool *pgxpool.Pool, email, role string) (string, string) {
	t.Helper()
	const secret = "correct horse battery staple, inventory"
	ctx := context.Background()
	created, err := users.CreateAdmin(ctx, pool, password.NewHasher(password.DefaultParams, 1),
		users.NewAdmin{Email: email, Password: secret}, audit.CLIActor("api users create-admin", "test"), "req-create-"+role)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `with dropped as (delete from user_roles where user_id = $1)
		insert into user_roles (user_id, role_id) select $1, id from roles where name = $2`, created.UserID, role); err != nil {
		t.Fatal(err)
	}
	login := c.expect(http.StatusCreated, http.MethodPost, "/api/v1/session", `{"email":"`+email+`","password":"`+secret+`"}`, "", nil)
	return created.UserID, login.cookies[0].Value
}

// Onboarding an environment, an application and a component through the
// server Run builds, against PostgreSQL and Valkey, with real sessions: a
// viewer reads but changes nothing, an operator makes each change, the
// ownership path holds, and each change has its audit record, by the
// operator, in its request.
func TestRunManagesTheInventory(t *testing.T) {
	t.Parallel()
	const site = "https://atrium.example.com"
	pg, pgCfg, pool := testenv.MigratedPostgres(t)
	valkey, redisCfg := testenv.Valkey(t)
	dir := t.TempDir()
	credentialstest.WriteSecretVolume(t, dir, map[string]string{
		credentials.Key(testenv.PostgresRef): pg.Credential.Username + ":" + pg.Credential.Password + "\n",
		credentials.Key(testenv.RedisRef):    valkey.Credential.Username + ":" + valkey.Credential.Password + "\n",
	})
	// The DB collector's admin API, as the collector serves it: an enabled
	// outsystems_logs collector extracting environment prod.
	const collectorToken = "collector-admin-token-for-the-test"
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/config" || r.Header.Get("Authorization") != "Bearer "+collectorToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"config": atriumconfig.RuntimeConfig{Collectors: []atriumconfig.CollectorRuntimeConfig{{
			ID: "oslogs-prod", Kind: "outsystems_logs", Enabled: true, Environment: "prod",
			Targets: []atriumconfig.TargetRuntimeConfig{{Name: "logging-db", EnvironmentSlug: "prod", CredentialRef: "env/oslog"}},
		}}}.Redacted()})
	}))
	t.Cleanup(collector.Close)
	deps, err := newRunDeps(Config{
		ListenAddr: "127.0.0.1:0", Postgres: pgCfg, Redis: redisCfg, CredentialsDir: dir, Site: site,
		CollectorURL: collector.URL, CollectorAdminToken: collectorToken,
		Logger: slog.New(slog.DiscardHandler),
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
	c := inventoryClient{t: t, base: "http://" + (<-addr).String(), site: site}
	waitReadyz(t, c.base+"/readyz", http.StatusOK, "migrated")

	operatorID, operator := userWithRole(t, c, pool, "operator@example.com", "operator")
	_, viewer := userWithRole(t, c, pool, "viewer@example.com", "viewer")
	var mark int64
	if err := pool.QueryRow(ctx, "select max(seq) from audit_outbox").Scan(&mark); err != nil {
		t.Fatal(err)
	}

	c.expect(http.StatusUnauthorized, http.MethodGet, "/api/v1/environments", "", "", nil)
	c.expect(http.StatusForbidden, http.MethodPost, "/api/v1/environments", `{"name":"Production","slug":"prod","type":"production"}`, viewer, nil)

	var env, other struct{ ID string }
	createdEnv := c.expect(http.StatusCreated, http.MethodPost, "/api/v1/environments", `{"name":"Production","slug":"prod","type":"production"}`, operator, &env)
	c.expect(http.StatusCreated, http.MethodPost, "/api/v1/environments", `{"name":"Staging","slug":"staging","type":"staging"}`, operator, &other)
	c.expect(http.StatusConflict, http.MethodPost, "/api/v1/environments", `{"name":"Again","slug":"prod","type":"production"}`, operator, nil)
	c.expect(http.StatusBadRequest, http.MethodPost, "/api/v1/environments", `{"name":"Prod","slug":"Prod","type":"production"}`, operator, nil)

	var app struct {
		ID            string `json:"id"`
		EnvironmentID string `json:"environment_id"`
		Criticality   string `json:"criticality"`
	}
	createdApp := c.expect(http.StatusCreated, http.MethodPost, "/api/v1/applications",
		`{"environment_id":"`+env.ID+`","name":"Claims Portal","platform":"outsystems_traditional","criticality":"high"}`, operator, &app)
	if app.EnvironmentID != env.ID {
		t.Fatalf("application %+v: want it in %s", app, env.ID)
	}
	c.expect(http.StatusUnprocessableEntity, http.MethodPost, "/api/v1/applications",
		`{"environment_id":"00000000-0000-4000-8000-000000000000","name":"Orphan","platform":"dotnet"}`, operator, nil)

	var component struct {
		ID            string `json:"id"`
		ApplicationID string `json:"application_id"`
		EnvironmentID string `json:"environment_id"`
	}
	createdComponent := c.expect(http.StatusCreated, http.MethodPost, "/api/v1/components",
		`{"application_id":"`+app.ID+`","name":"ClaimsWeb","component_type":"module","runtime_name":"ClaimsWeb"}`, operator, &component)
	if component.ApplicationID != app.ID || component.EnvironmentID != env.ID {
		t.Fatalf("component %+v: want application %s in environment %s", component, app.ID, env.ID)
	}

	// The ownership path cannot move: the PATCH is refused whole, and the
	// application stays where it was, unchanged.
	c.expect(http.StatusBadRequest, http.MethodPatch, "/api/v1/applications/"+app.ID, `{"criticality":"low","environment_id":"`+other.ID+`"}`, operator, nil)
	c.expect(http.StatusOK, http.MethodGet, "/api/v1/applications/"+app.ID, "", viewer, &app)
	if app.EnvironmentID != env.ID || app.Criticality != "high" {
		t.Fatalf("after a refused move, application %+v", app)
	}
	changedApp := c.expect(http.StatusOK, http.MethodPatch, "/api/v1/applications/"+app.ID, `{"criticality":"critical"}`, operator, &app)
	c.expect(http.StatusForbidden, http.MethodPatch, "/api/v1/applications/"+app.ID, `{"criticality":"low"}`, viewer, nil)

	// Deletes follow the foreign keys.
	refused := c.expect(http.StatusConflict, http.MethodDelete, "/api/v1/environments/"+env.ID, "", operator, nil)
	if !strings.Contains(refused.body, "still has applications") {
		t.Fatalf("refused delete: %s", refused.body)
	}
	c.expect(http.StatusConflict, http.MethodDelete, "/api/v1/applications/"+app.ID, "", operator, nil)

	var page struct {
		Items []struct {
			ID            string `json:"id"`
			EnvironmentID string `json:"environment_id"`
		} `json:"items"`
	}
	c.expect(http.StatusOK, http.MethodGet, "/api/v1/components?application_id="+app.ID, "", viewer, &page)
	if len(page.Items) != 1 || page.Items[0].EnvironmentID != env.ID {
		t.Fatalf("components of the application: %+v", page)
	}
	c.expect(http.StatusOK, http.MethodGet, "/api/v1/applications?environment_id="+other.ID, "", viewer, &page)
	if len(page.Items) != 0 {
		t.Fatalf("staging lists %+v", page.Items)
	}

	// The application's telemetry source: its environment is the
	// application's, and the collector observes it; a viewer may not
	// register one.
	sourceBody := `{"application_id":"` + app.ID + `","source_type":"outsystems","ingest_mode":"agentless","config":{"application_name":"Claims"}}`
	c.expect(http.StatusForbidden, http.MethodPost, "/api/v1/telemetry-sources", sourceBody, viewer, nil)
	var src struct {
		ID            string `json:"id"`
		EnvironmentID string `json:"environment_id"`
		Observation   struct {
			Status     string   `json:"status"`
			Collectors []string `json:"collectors"`
		} `json:"observation"`
	}
	createdSource := c.expect(http.StatusCreated, http.MethodPost, "/api/v1/telemetry-sources", sourceBody, operator, &src)
	if src.EnvironmentID != env.ID || src.Observation.Status != "observed" || len(src.Observation.Collectors) != 1 || src.Observation.Collectors[0] != "oslogs-prod" {
		t.Fatalf("source %+v: want it in %s, observed by oslogs-prod", src, env.ID)
	}
	c.expect(http.StatusBadRequest, http.MethodPost, "/api/v1/telemetry-sources",
		`{"application_id":"`+app.ID+`","source_type":"outsystems","ingest_mode":"otlp","config":{"application_name":"Claims","password":"hunter2"}}`, operator, nil)
	c.expect(http.StatusOK, http.MethodGet, "/api/v1/telemetry-sources/"+src.ID, "", viewer, &src)
	if src.Observation.Status != "observed" {
		t.Fatalf("source read back: %+v", src)
	}

	deletedComponent := c.expect(http.StatusNoContent, http.MethodDelete, "/api/v1/components/"+component.ID, "", operator, nil)
	c.expect(http.StatusNotFound, http.MethodGet, "/api/v1/components/"+component.ID, "", viewer, nil)

	rows, err := pool.Query(ctx, "select record::text from audit_outbox where seq > $1 order by seq", mark)
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
		audittest.NoSecrets(t, []byte(record), operator, viewer, collectorToken, "hunter2")
		var head struct {
			Action     string      `json:"action"`
			EntityType string      `json:"entity_type"`
			RequestID  string      `json:"request_id"`
			Actor      audit.Actor `json:"actor"`
		}
		if err := json.Unmarshal([]byte(record), &head); err != nil {
			t.Fatal(err)
		}
		if head.Actor != audit.UserActor(operatorID, "operator@example.com") {
			t.Errorf("record by %+v, want the operator: %s", head.Actor, record)
		}
		steps = append(steps, head.Action+" "+head.EntityType+" "+head.RequestID)
	}
	want := []string{
		"create environment " + createdEnv.requestID,
		"create environment", // staging
		"create application " + createdApp.requestID,
		"create application_component " + createdComponent.requestID,
		"update application " + changedApp.requestID,
		"create telemetry_source " + createdSource.requestID,
		"delete application_component " + deletedComponent.requestID,
	}
	if len(steps) != len(want) {
		t.Fatalf("audit records %v, want %v: refusals record nothing", steps, want)
	}
	for i := range want {
		if !strings.HasPrefix(steps[i], want[i]) {
			t.Errorf("record %d = %q, want %q", i, steps[i], want[i])
		}
	}
}
