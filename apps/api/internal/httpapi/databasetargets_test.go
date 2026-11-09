package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"atrium/apps/api/internal/auth"
	"atrium/apps/api/internal/inventory"
)

// fakeTargets answers each call with its fields and records the last one.
type fakeTargets struct {
	target inventory.DatabaseTarget
	err    error

	calls  []string
	id     uuid.UUID
	filter inventory.DatabaseTargetFilter
	in     inventory.NewDatabaseTarget
	change inventory.DatabaseTargetChange
	caller inventory.Caller
}

func (f *fakeTargets) ListDatabaseTargets(_ context.Context, filter inventory.DatabaseTargetFilter, _ inventory.Page) (inventory.DatabaseTargetPage, error) {
	f.calls, f.filter = append(f.calls, "List"), filter
	return inventory.DatabaseTargetPage{Items: []inventory.DatabaseTarget{f.target}, NextCursor: "next"}, f.err
}

func (f *fakeTargets) GetDatabaseTarget(_ context.Context, id uuid.UUID) (inventory.DatabaseTarget, error) {
	f.calls, f.id = append(f.calls, "Get"), id
	return f.target, f.err
}

func (f *fakeTargets) CreateDatabaseTarget(_ context.Context, c inventory.Caller, in inventory.NewDatabaseTarget) (inventory.DatabaseTarget, error) {
	f.calls, f.caller, f.in = append(f.calls, "Create"), c, in
	return f.target, f.err
}

func (f *fakeTargets) UpdateDatabaseTarget(_ context.Context, c inventory.Caller, id uuid.UUID, ch inventory.DatabaseTargetChange) (inventory.DatabaseTarget, error) {
	f.calls, f.caller, f.id, f.change = append(f.calls, "Update"), c, id, ch
	return f.target, f.err
}

func (f *fakeTargets) DeleteDatabaseTarget(_ context.Context, c inventory.Caller, id uuid.UUID) error {
	f.calls, f.caller, f.id = append(f.calls, "Delete"), c, id
	return f.err
}

var financeProd = inventory.DatabaseTarget{
	ID: uuid.MustParse("3d6f1a2b-7c8d-4e9f-a0b1-c2d3e4f5a6b7"), EnvironmentID: uuid.MustParse("9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d"),
	Engine: "sqlserver", Name: "finance-prod", Host: "sql01.finance.internal", Port: 1433,
	CredentialRef: "env/finance-prod", Status: inventory.StatusActive, Labels: map[string]string{},
	CreatedAt: time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC),
}

func targetServer(t *testing.T, targets *fakeTargets) *Server {
	t.Helper()
	return newServer(t, Options{Auth: roleAuth{}, Site: testSite, DatabaseTargets: targets})
}

const newTargetBody = `{"environment_id":"9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d","name":"finance-prod","host":"sql01.finance.internal","credential_ref":"env/finance-prod"}`

// Viewers read targets; operators change them.
func TestDatabaseTargetRoles(t *testing.T) {
	id := financeProd.ID.String()
	for _, op := range []struct {
		method, path, body string
		least              auth.Role
		ok                 int
	}{
		{http.MethodGet, "/api/v1/database-targets", "", auth.Viewer, http.StatusOK},
		{http.MethodGet, "/api/v1/database-targets/" + id, "", auth.Viewer, http.StatusOK},
		{http.MethodPost, "/api/v1/database-targets", newTargetBody, auth.Operator, http.StatusCreated},
		{http.MethodPatch, "/api/v1/database-targets/" + id, `{"status":"disabled"}`, auth.Operator, http.StatusOK},
		{http.MethodDelete, "/api/v1/database-targets/" + id, "", auth.Operator, http.StatusNoContent},
	} {
		t.Run(op.method+" "+op.path, func(t *testing.T) {
			if rec := send(t, targetServer(t, &fakeTargets{target: financeProd}), "", op.method, op.path, op.body); rec.Code != http.StatusUnauthorized {
				t.Errorf("without a session: %d, want 401", rec.Code)
			}
			for _, role := range auth.Roles {
				targets := &fakeTargets{target: financeProd}
				rec := send(t, targetServer(t, targets), string(role), op.method, op.path, op.body)
				allowed := auth.Allows([]string{string(role)}, op.least)
				switch {
				case allowed && rec.Code != op.ok:
					t.Errorf("as %s: %d %s, want %d", role, rec.Code, rec.Body, op.ok)
				case !allowed && (rec.Code != http.StatusForbidden || len(targets.calls) != 0):
					t.Errorf("as %s: %d, calls %v; want 403 and no call", role, rec.Code, targets.calls)
				}
			}
		})
	}
}

func TestCreateDatabaseTarget(t *testing.T) {
	targets := &fakeTargets{target: financeProd}
	rec := send(t, targetServer(t, targets), "operator", http.MethodPost, "/api/v1/database-targets",
		`{"environment_id":"9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d","engine":"sqlserver","name":"finance-prod","host":"sql01","port":14330,"database_name":"Finance","credential_ref":"kv/finance","status":"disabled","labels":{"tier":"gold"}}`)
	if rec.Code != http.StatusCreated || rec.Header().Get("Location") != "/api/v1/database-targets/"+financeProd.ID.String() {
		t.Fatalf("POST = %d, Location %q", rec.Code, rec.Header().Get("Location"))
	}
	in := targets.in
	if in.EnvironmentID != financeProd.EnvironmentID || in.Engine != "sqlserver" || in.Name != "finance-prod" || in.Host != "sql01" || in.Port != 14330 ||
		in.DatabaseName != "Finance" || in.CredentialRef != "kv/finance" || in.Status != "disabled" || in.Labels["tier"] != "gold" {
		t.Errorf("CreateDatabaseTarget(%+v)", in)
	}
	if targets.caller.Actor.Email != "operator@example.com" || targets.caller.RequestID != rec.Header().Get(requestIDHeader) {
		t.Errorf("caller = %+v, want the operator and the request's ID", targets.caller)
	}

	// Left out, the port is the engine's default; zero is refused.
	targets = &fakeTargets{target: financeProd}
	if rec := send(t, targetServer(t, targets), "operator", http.MethodPost, "/api/v1/database-targets", newTargetBody); rec.Code != http.StatusCreated || targets.in.Port != 0 {
		t.Errorf("no port: %d, port %d; want 201 and 0 (the default)", rec.Code, targets.in.Port)
	}
	targets = &fakeTargets{target: financeProd}
	rec = send(t, targetServer(t, targets), "operator", http.MethodPost, "/api/v1/database-targets", strings.TrimSuffix(newTargetBody, "}")+`,"port":0}`)
	if rec.Code != http.StatusBadRequest || len(targets.calls) != 0 {
		t.Errorf("port 0: %d, calls %v; want 400 and no call", rec.Code, targets.calls)
	}
	// A body member the schema does not have is refused: a password field
	// sent by mistake is not silently dropped.
	targets = &fakeTargets{target: financeProd}
	rec = send(t, targetServer(t, targets), "operator", http.MethodPost, "/api/v1/database-targets", strings.TrimSuffix(newTargetBody, "}")+`,"password":"hunter2"}`)
	if rec.Code != http.StatusBadRequest || len(targets.calls) != 0 {
		t.Errorf("a password member: %d %s, calls %v; want 400 and no call", rec.Code, rec.Body, targets.calls)
	}
}

func TestDatabaseTargetErrors(t *testing.T) {
	id := financeProd.ID.String()
	for _, tc := range []struct {
		name, method, path, body string
		err                      error
		code                     int
	}{
		{"create: invalid", http.MethodPost, "/api/v1/database-targets", newTargetBody, &inventory.InvalidError{Field: "host", Reason: "bad"}, http.StatusBadRequest},
		{"create: taken", http.MethodPost, "/api/v1/database-targets", newTargetBody, &inventory.ConflictError{Detail: "taken"}, http.StatusConflict},
		{"create: no environment", http.MethodPost, "/api/v1/database-targets", newTargetBody, &inventory.OwnerError{Detail: "no environment"}, http.StatusUnprocessableEntity},
		{"get: unknown", http.MethodGet, "/api/v1/database-targets/" + id, "", inventory.ErrNotFound, http.StatusNotFound},
		{"list: invalid", http.MethodGet, "/api/v1/database-targets", "", &inventory.InvalidError{Field: "status", Reason: "bad"}, http.StatusBadRequest},
		{"update: invalid", http.MethodPatch, "/api/v1/database-targets/" + id, `{"port":1}`, &inventory.InvalidError{Field: "port", Reason: "bad"}, http.StatusBadRequest},
		{"update: unknown", http.MethodPatch, "/api/v1/database-targets/" + id, `{"port":1}`, inventory.ErrNotFound, http.StatusNotFound},
		{"update: taken", http.MethodPatch, "/api/v1/database-targets/" + id, `{"port":1}`, &inventory.ConflictError{Detail: "taken"}, http.StatusConflict},
		{"delete: unknown", http.MethodDelete, "/api/v1/database-targets/" + id, "", inventory.ErrNotFound, http.StatusNotFound},
		{"delete: assigned", http.MethodDelete, "/api/v1/database-targets/" + id, "", &inventory.ConflictError{Detail: "has assignments"}, http.StatusConflict},
		{"get: failure", http.MethodGet, "/api/v1/database-targets/" + id, "", context.DeadlineExceeded, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := send(t, targetServer(t, &fakeTargets{err: tc.err}), "operator", tc.method, tc.path, tc.body)
			if rec.Code != tc.code || rec.Header().Get("Content-Type") != "application/problem+json" {
				t.Fatalf("%d %s (%s), want %d problem+json", rec.Code, rec.Body, rec.Header().Get("Content-Type"), tc.code)
			}
		})
	}
}

func TestUpdateDatabaseTargetPassesOnlyTheBodysFields(t *testing.T) {
	targets := &fakeTargets{target: financeProd}
	rec := send(t, targetServer(t, targets), "operator", http.MethodPatch, "/api/v1/database-targets/"+financeProd.ID.String(),
		`{"credential_ref":"kv/finance","database_name":"","labels":{"tier":"gold"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d %s", rec.Code, rec.Body)
	}
	c := targets.change
	if c.Host != nil || c.Port != nil || c.Status != nil || c.CredentialRef == nil || *c.CredentialRef != "kv/finance" ||
		c.DatabaseName == nil || *c.DatabaseName != "" || c.Labels == nil || (*c.Labels)["tier"] != "gold" || targets.id != financeProd.ID {
		t.Errorf("UpdateDatabaseTarget(%s, %+v)", targets.id, c)
	}
	// The name, engine and environment are fixed: the body cannot name them.
	for _, body := range []string{`{"name":"other"}`, `{"engine":"sqlserver"}`, `{"environment_id":"` + uuid.NewString() + `"}`} {
		targets := &fakeTargets{target: financeProd}
		if rec := send(t, targetServer(t, targets), "operator", http.MethodPatch, "/api/v1/database-targets/"+financeProd.ID.String(), body); rec.Code != http.StatusBadRequest || len(targets.calls) != 0 {
			t.Errorf("PATCH %s = %d, calls %v; want 400 and no call", body, rec.Code, targets.calls)
		}
	}
}

// A target shows its database_name, empty for the login's default, and its
// credential reference, and the list passes the filter on.
func TestDatabaseTargetBody(t *testing.T) {
	targets := &fakeTargets{target: financeProd}
	envID := financeProd.EnvironmentID.String()
	rec := send(t, targetServer(t, targets), "viewer", http.MethodGet, "/api/v1/database-targets?environment_id="+envID+"&status=active", "")
	var body struct {
		Items      []map[string]any `json:"items"`
		NextCursor string           `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Items) != 1 || body.NextCursor != "next" {
		t.Fatalf("GET = %d %s, %v", rec.Code, rec.Body, err)
	}
	item := body.Items[0]
	if item["database_name"] != "" || item["credential_ref"] != "env/finance-prod" || item["port"] != 1433.0 || item["engine"] != "sqlserver" {
		t.Errorf("item = %v", item)
	}
	if targets.filter.EnvironmentID != financeProd.EnvironmentID || targets.filter.Status != "active" {
		t.Errorf("filter = %+v", targets.filter)
	}
}

// Without a database targets service, the operations fail instead of
// answering empty.
func TestDatabaseTargetsWithoutAService(t *testing.T) {
	server := newServer(t, Options{Auth: roleAuth{}, Site: testSite})
	if rec := send(t, server, "viewer", http.MethodGet, "/api/v1/database-targets", ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("GET = %d, want 500", rec.Code)
	}
}
