package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"heartbeat/apps/api/internal/audit"
	"heartbeat/apps/api/internal/auth"
	"heartbeat/apps/api/internal/probedefs"
)

const testSite = "https://heartbeat.example.com"

// roleAuth authenticates a session token that is a role's name as a user
// with that role alone.
type roleAuth struct{}

func (roleAuth) Login(context.Context, string, string, auth.Client) (auth.Principal, string, error) {
	return auth.Principal{}, "", auth.ErrInvalidCredentials
}

func (roleAuth) Authenticate(_ context.Context, token string) (auth.Principal, error) {
	for _, role := range auth.Roles {
		if token == string(role) {
			return auth.Principal{
				Session: auth.Session{ID: "0b8c2a52-55a5-4d6c-9a0e-6f2f8d0c1a11", Created: time.Now()},
				User:    auth.User{ID: "6f1c0e8a-3c55-4f0b-9d2b-4a7e8c1d2e3f", Email: token + "@example.com", Roles: []string{token}},
			}, nil
		}
	}
	return auth.Principal{}, auth.ErrNoSession
}

func (roleAuth) Logout(context.Context, auth.Principal, auth.Client) error { return nil }

func (roleAuth) ExpiresAt(s auth.Session) time.Time { return s.Created.Add(time.Hour) }

// fakeProbeDefinitions answers each call with its fields and records the
// last one.
type fakeProbeDefinitions struct {
	def   probedefs.Definition
	added bool
	err   error

	calls     []string
	id        uuid.UUID
	newDef    probedefs.NewDefinition
	content   probedefs.Content
	enabled   bool
	actor     audit.Actor
	requestID string
}

func (f *fakeProbeDefinitions) List(context.Context) ([]probedefs.Definition, error) {
	f.calls = append(f.calls, "List")
	return []probedefs.Definition{f.def}, f.err
}

func (f *fakeProbeDefinitions) Get(_ context.Context, id uuid.UUID) (probedefs.Definition, error) {
	f.calls, f.id = append(f.calls, "Get"), id
	return f.def, f.err
}

func (f *fakeProbeDefinitions) Versions(_ context.Context, id uuid.UUID) ([]probedefs.Definition, error) {
	f.calls, f.id = append(f.calls, "Versions"), id
	return []probedefs.Definition{f.def}, f.err
}

func (f *fakeProbeDefinitions) Create(_ context.Context, def probedefs.NewDefinition, actor audit.Actor, requestID string) (probedefs.Definition, error) {
	f.calls, f.newDef, f.actor, f.requestID = append(f.calls, "Create"), def, actor, requestID
	return f.def, f.err
}

func (f *fakeProbeDefinitions) AddVersion(_ context.Context, from uuid.UUID, content probedefs.Content, actor audit.Actor, requestID string) (probedefs.Definition, bool, error) {
	f.calls, f.id, f.content, f.actor, f.requestID = append(f.calls, "AddVersion"), from, content, actor, requestID
	return f.def, f.added, f.err
}

func (f *fakeProbeDefinitions) SetEnabled(_ context.Context, id uuid.UUID, enabled bool, actor audit.Actor, requestID string) (probedefs.Definition, error) {
	f.calls, f.id, f.enabled, f.actor, f.requestID = append(f.calls, "SetEnabled"), id, enabled, actor, requestID
	return f.def, f.err
}

var (
	builtinWaits = probedefs.Definition{
		ID: uuid.MustParse("2f6a1d0e-8b3c-4e5f-9a1b-0c2d3e4f5a6b"), Engine: "sqlserver", Name: "waits", Version: 1,
		Builtin: true, Category: "waits", DefaultIntervalSeconds: 30, Enabled: true, Latest: true,
		CreatedAt: time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC),
	}
	customWaits = probedefs.Definition{
		ID: uuid.MustParse("7c9e6679-7425-40de-944b-e07fc1f90ae7"), Engine: "sqlserver", Name: "waits_lean", Version: 2,
		Overrides: "waits", Category: "waits", Description: "fewer wait types", DefaultIntervalSeconds: 60, TimeoutMS: 5000,
		QueryTemplate: "SELECT wait_type, wait_time_ms FROM sys.dm_os_wait_stats", Enabled: true, Latest: true,
		CreatedAt: time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC),
	}
)

// probeServer is a server with fake auth and probe definitions.
func probeServer(t *testing.T, defs *fakeProbeDefinitions) *Server {
	t.Helper()
	return newServer(t, Options{Auth: roleAuth{}, Site: testSite, ProbeDefinitions: defs})
}

// send makes a request as role ("" for no session), from the site, and
// returns the response.
func send(t *testing.T, server *Server, role, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	server.Register(mux)
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Origin", testSite)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if role != "" {
		req.AddCookie(&http.Cookie{Name: SessionCookie, Value: role})
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

const newDefinitionBody = `{"engine":"sqlserver","name":"waits_lean","overrides":"waits","query_template":"SELECT wait_type, wait_time_ms FROM sys.dm_os_wait_stats"}`

// Anyone logged in reads probe definitions; only admins change them, since
// they carry SQL.
func TestProbeDefinitionOperationsNeedTheirRole(t *testing.T) {
	id := customWaits.ID.String()
	for _, tc := range []struct {
		method, path, body string
		least              auth.Role
		ok                 int
	}{
		{http.MethodGet, "/api/v1/probe-definitions", "", auth.Viewer, http.StatusOK},
		{http.MethodGet, "/api/v1/probe-definitions/" + id, "", auth.Viewer, http.StatusOK},
		{http.MethodGet, "/api/v1/probe-definitions/" + id + "/versions", "", auth.Viewer, http.StatusOK},
		{http.MethodPost, "/api/v1/probe-definitions", newDefinitionBody, auth.Admin, http.StatusCreated},
		{http.MethodPost, "/api/v1/probe-definitions/" + id + "/versions", `{"overrides":"waits","query_template":"SELECT 1"}`, auth.Admin, http.StatusCreated},
		{http.MethodPatch, "/api/v1/probe-definitions/" + id, `{"enabled":false}`, auth.Admin, http.StatusOK},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			if rec := send(t, probeServer(t, &fakeProbeDefinitions{def: customWaits, added: true}), "", tc.method, tc.path, tc.body); rec.Code != http.StatusUnauthorized {
				t.Errorf("without a session: %d, want 401", rec.Code)
			}
			for _, role := range auth.Roles {
				defs := &fakeProbeDefinitions{def: customWaits, added: true}
				rec := send(t, probeServer(t, defs), string(role), tc.method, tc.path, tc.body)
				allowed := auth.Allows([]string{string(role)}, tc.least)
				switch {
				case allowed && rec.Code != tc.ok:
					t.Errorf("as %s: %d %s, want %d", role, rec.Code, rec.Body, tc.ok)
				case !allowed && (rec.Code != http.StatusForbidden || len(defs.calls) != 0):
					t.Errorf("as %s: %d, calls %v; want 403 and no call", role, rec.Code, defs.calls)
				}
			}
		})
	}
}

func TestCreateProbeDefinition(t *testing.T) {
	defs := &fakeProbeDefinitions{def: customWaits}
	rec := send(t, probeServer(t, defs), "admin", http.MethodPost, "/api/v1/probe-definitions",
		`{"engine":"sqlserver","name":"waits_lean","overrides":"waits","query_template":"SELECT 1","description":"d","default_interval_seconds":60,"timeout_ms":5000}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST = %d %s, want 201", rec.Code, rec.Body)
	}
	if got, want := rec.Header().Get("Location"), "/api/v1/probe-definitions/"+customWaits.ID.String(); got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
	want := probedefs.NewDefinition{Engine: "sqlserver", Name: "waits_lean", Content: probedefs.Content{
		Overrides: "waits", QueryTemplate: "SELECT 1", Description: "d", DefaultIntervalSeconds: 60, TimeoutMS: 5000,
	}}
	if defs.newDef != want {
		t.Errorf("Create(%+v), want %+v", defs.newDef, want)
	}
	if defs.actor != audit.UserActor("6f1c0e8a-3c55-4f0b-9d2b-4a7e8c1d2e3f", "admin@example.com") {
		t.Errorf("actor = %+v, want the logged-in admin", defs.actor)
	}
	if defs.requestID == "" || defs.requestID != rec.Header().Get(requestIDHeader) {
		t.Errorf("request ID = %q, want the response's %s %q", defs.requestID, requestIDHeader, rec.Header().Get(requestIDHeader))
	}
}

// A field left out takes its default; zero, which the spec does not allow,
// is refused rather than read as left out.
func TestCreateProbeDefinitionRefusesExplicitZeros(t *testing.T) {
	for _, field := range []string{"default_interval_seconds", "timeout_ms"} {
		defs := &fakeProbeDefinitions{def: customWaits}
		body := strings.TrimSuffix(newDefinitionBody, "}") + `,"` + field + `":0}`
		rec := send(t, probeServer(t, defs), "admin", http.MethodPost, "/api/v1/probe-definitions", body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), field+" 0") || len(defs.calls) != 0 {
			t.Errorf("%s 0: %d %s, calls %v; want 400 naming it and no call", field, rec.Code, rec.Body, defs.calls)
		}
	}
	defs := &fakeProbeDefinitions{def: customWaits}
	if rec := send(t, probeServer(t, defs), "admin", http.MethodPost, "/api/v1/probe-definitions", newDefinitionBody); rec.Code != http.StatusCreated || defs.newDef.DefaultIntervalSeconds != 0 || defs.newDef.TimeoutMS != 0 {
		t.Errorf("left out: %d, %+v; want 201 and zero (the defaults)", rec.Code, defs.newDef)
	}
}

// Each error of the service is the status the spec declares for it, with
// its reason as the detail.
func TestProbeDefinitionErrors(t *testing.T) {
	id := customWaits.ID.String()
	invalid := probedefsInvalid(t)
	for _, tc := range []struct {
		name               string
		method, path, body string
		err                error
		code               int
	}{
		{"create: invalid", http.MethodPost, "/api/v1/probe-definitions", newDefinitionBody, invalid, http.StatusBadRequest},
		{"create: name taken", http.MethodPost, "/api/v1/probe-definitions", newDefinitionBody, probedefs.ErrNameTaken, http.StatusConflict},
		{"get: not found", http.MethodGet, "/api/v1/probe-definitions/" + id, "", probedefs.ErrNotFound, http.StatusNotFound},
		{"versions: not found", http.MethodGet, "/api/v1/probe-definitions/" + id + "/versions", "", probedefs.ErrNotFound, http.StatusNotFound},
		{"add version: invalid", http.MethodPost, "/api/v1/probe-definitions/" + id + "/versions", `{"overrides":"waits","query_template":"x"}`, invalid, http.StatusBadRequest},
		{"add version: not found", http.MethodPost, "/api/v1/probe-definitions/" + id + "/versions", `{"overrides":"waits","query_template":"x"}`, probedefs.ErrNotFound, http.StatusNotFound},
		{"add version: built-in", http.MethodPost, "/api/v1/probe-definitions/" + id + "/versions", `{"overrides":"waits","query_template":"x"}`, probedefs.ErrBuiltin, http.StatusConflict},
		{"add version: disabled", http.MethodPost, "/api/v1/probe-definitions/" + id + "/versions", `{"overrides":"waits","query_template":"x"}`, probedefs.ErrDisabled, http.StatusConflict},
		{"patch: not found", http.MethodPatch, "/api/v1/probe-definitions/" + id, `{"enabled":false}`, probedefs.ErrNotFound, http.StatusNotFound},
		{"patch: built-in", http.MethodPatch, "/api/v1/probe-definitions/" + id, `{"enabled":false}`, probedefs.ErrBuiltin, http.StatusConflict},
		{"list: failure", http.MethodGet, "/api/v1/probe-definitions", "", context.DeadlineExceeded, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := send(t, probeServer(t, &fakeProbeDefinitions{err: tc.err}), "admin", tc.method, tc.path, tc.body)
			if rec.Code != tc.code {
				t.Fatalf("%d %s, want %d", rec.Code, rec.Body, tc.code)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
				t.Errorf("Content-Type = %q, want application/problem+json", got)
			}
			var p struct {
				Status int    `json:"status"`
				Detail string `json:"detail"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
				t.Fatal(err)
			}
			if p.Status != tc.code || (tc.code != http.StatusInternalServerError && tc.code != http.StatusNotFound && p.Detail != tc.err.Error()) {
				t.Errorf("problem %+v, want status %d and detail %q", p, tc.code, tc.err)
			}
		})
	}
}

// probedefsInvalid returns an *probedefs.InvalidError, as the service makes
// one: content is checked before the database is used.
func probedefsInvalid(t *testing.T) error {
	t.Helper()
	_, _, err := probedefs.NewService(nil).AddVersion(context.Background(), uuid.New(), probedefs.Content{}, audit.Actor{}, "")
	if err == nil {
		t.Fatal("AddVersion of empty content succeeded")
	}
	return err
}

// The guard runs before the path is parsed: a malformed ID is a 400 only
// for a caller the operation allows, and gets a request ID like any other.
func TestMalformedIDsAreCheckedByTheGuardFirst(t *testing.T) {
	defs := &fakeProbeDefinitions{def: customWaits}
	server := probeServer(t, defs)
	if rec := send(t, server, "", http.MethodGet, "/api/v1/probe-definitions/waits", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET with a malformed ID, no session = %d, want 401", rec.Code)
	}
	if rec := send(t, server, "viewer", http.MethodPatch, "/api/v1/probe-definitions/waits", `{"enabled":false}`); rec.Code != http.StatusForbidden {
		t.Errorf("PATCH with a malformed ID, as a viewer = %d, want 403", rec.Code)
	}
	rec := send(t, server, "viewer", http.MethodGet, "/api/v1/probe-definitions/waits", "")
	if rec.Code != http.StatusBadRequest || rec.Header().Get(requestIDHeader) == "" || len(defs.calls) != 0 {
		t.Errorf("GET with a malformed ID, as a viewer = %d, %s %q, calls %v; want 400, a request ID and no call",
			rec.Code, requestIDHeader, rec.Header().Get(requestIDHeader), defs.calls)
	}
}

// A built-in has no query, overrides or timeout of its own: they are null,
// not absent or empty.
func TestProbeDefinitionBody(t *testing.T) {
	defs := &fakeProbeDefinitions{def: builtinWaits}
	rec := send(t, probeServer(t, defs), "viewer", http.MethodGet, "/api/v1/probe-definitions", "")
	var body struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Items) != 1 {
		t.Fatalf("GET = %d %s, %v; want one item", rec.Code, rec.Body, err)
	}
	item := body.Items[0]
	for _, field := range []string{"overrides", "query_template", "timeout_ms"} {
		if v, ok := item[field]; !ok || v != nil {
			t.Errorf("%s = %v (present %t), want null", field, v, ok)
		}
	}
	if item["id"] != builtinWaits.ID.String() || item["builtin"] != true || item["version"] != 1.0 || item["latest"] != true || item["created_at"] != "2026-11-03T09:00:00Z" {
		t.Errorf("item = %v", item)
	}

	defs = &fakeProbeDefinitions{def: customWaits}
	rec = send(t, probeServer(t, defs), "viewer", http.MethodGet, "/api/v1/probe-definitions/"+customWaits.ID.String(), "")
	var custom map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &custom); err != nil {
		t.Fatal(err)
	}
	if custom["overrides"] != "waits" || custom["query_template"] != customWaits.QueryTemplate || custom["timeout_ms"] != 5000.0 || custom["version"] != 2.0 {
		t.Errorf("custom = %v", custom)
	}
	if defs.id != customWaits.ID {
		t.Errorf("Get(%s), want the path's ID %s", defs.id, customWaits.ID)
	}
}

func TestCreateProbeDefinitionVersion(t *testing.T) {
	path := "/api/v1/probe-definitions/" + builtinWaits.ID.String() + "/versions"
	body := `{"overrides":"waits","query_template":"SELECT 1","timeout_ms":2000}`

	defs := &fakeProbeDefinitions{def: customWaits, added: true}
	rec := send(t, probeServer(t, defs), "admin", http.MethodPost, path, body)
	if rec.Code != http.StatusCreated || rec.Header().Get("Location") != "/api/v1/probe-definitions/"+customWaits.ID.String() {
		t.Fatalf("added: %d, Location %q; want 201 and the new version's URL", rec.Code, rec.Header().Get("Location"))
	}
	if defs.id != builtinWaits.ID || defs.content != (probedefs.Content{Overrides: "waits", QueryTemplate: "SELECT 1", TimeoutMS: 2000}) {
		t.Errorf("AddVersion(%s, %+v)", defs.id, defs.content)
	}

	defs = &fakeProbeDefinitions{def: customWaits, added: false}
	if rec := send(t, probeServer(t, defs), "admin", http.MethodPost, path, body); rec.Code != http.StatusOK || rec.Header().Get("Location") != "" {
		t.Errorf("unchanged: %d, Location %q; want 200 and none", rec.Code, rec.Header().Get("Location"))
	}

}

func TestUpdateProbeDefinition(t *testing.T) {
	path := "/api/v1/probe-definitions/" + customWaits.ID.String()
	for _, tc := range []struct {
		body    string
		enabled bool
	}{{`{"enabled":false}`, false}, {`{"enabled":true}`, true}} {
		defs := &fakeProbeDefinitions{def: customWaits}
		rec := send(t, probeServer(t, defs), "admin", http.MethodPatch, path, tc.body)
		if rec.Code != http.StatusOK || defs.enabled != tc.enabled || defs.id != customWaits.ID {
			t.Errorf("PATCH %s = %d; SetEnabled(%s, %t); want 200 and %t", tc.body, rec.Code, defs.id, defs.enabled, tc.enabled)
		}
	}
	// Without enabled, a decoded body would read as false: it must not
	// disable anything.
	defs := &fakeProbeDefinitions{def: customWaits}
	if rec := send(t, probeServer(t, defs), "admin", http.MethodPatch, path, `{}`); rec.Code != http.StatusBadRequest || len(defs.calls) != 0 {
		t.Errorf("PATCH {} = %d, calls %v; want 400 and no call", rec.Code, defs.calls)
	}
}

// Without a probe definitions service, the operations fail instead of
// answering empty.
func TestProbeDefinitionsWithoutAService(t *testing.T) {
	server := newServer(t, Options{Auth: roleAuth{}, Site: testSite})
	if rec := send(t, server, "viewer", http.MethodGet, "/api/v1/probe-definitions", ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("GET = %d, want 500", rec.Code)
	}
}
