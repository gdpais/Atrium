package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"heartbeat/apps/api/internal/auth"
	"heartbeat/apps/api/internal/inventory"
)

// fakeAssignments answers each call with its fields and records the last one.
type fakeAssignments struct {
	assignment inventory.ProbeAssignment
	rendered   []byte
	err        error

	calls  []string
	id     uuid.UUID
	filter inventory.ProbeAssignmentFilter
	in     inventory.NewProbeAssignment
	change inventory.ProbeAssignmentChange
	reason string
	caller inventory.Caller
}

func (f *fakeAssignments) ListProbeAssignments(_ context.Context, filter inventory.ProbeAssignmentFilter, _ inventory.Page) (inventory.ProbeAssignmentPage, error) {
	f.calls, f.filter = append(f.calls, "List"), filter
	return inventory.ProbeAssignmentPage{Items: []inventory.ProbeAssignment{f.assignment}, NextCursor: "next"}, f.err
}

func (f *fakeAssignments) GetProbeAssignment(_ context.Context, id uuid.UUID) (inventory.ProbeAssignment, error) {
	f.calls, f.id = append(f.calls, "Get"), id
	return f.assignment, f.err
}

func (f *fakeAssignments) CreateProbeAssignment(_ context.Context, c inventory.Caller, in inventory.NewProbeAssignment) (inventory.ProbeAssignment, error) {
	f.calls, f.caller, f.in = append(f.calls, "Create"), c, in
	return f.assignment, f.err
}

func (f *fakeAssignments) UpdateProbeAssignment(_ context.Context, c inventory.Caller, id uuid.UUID, ch inventory.ProbeAssignmentChange) (inventory.ProbeAssignment, error) {
	f.calls, f.caller, f.id, f.change = append(f.calls, "Update"), c, id, ch
	return f.assignment, f.err
}

func (f *fakeAssignments) ApproveProbeAssignment(_ context.Context, c inventory.Caller, id uuid.UUID) (inventory.ProbeAssignment, error) {
	f.calls, f.caller, f.id = append(f.calls, "Approve"), c, id
	return f.assignment, f.err
}

func (f *fakeAssignments) RejectProbeAssignment(_ context.Context, c inventory.Caller, id uuid.UUID, reason string) (inventory.ProbeAssignment, error) {
	f.calls, f.caller, f.id, f.reason = append(f.calls, "Reject"), c, id, reason
	return f.assignment, f.err
}

func (f *fakeAssignments) DeleteProbeAssignment(_ context.Context, c inventory.Caller, id uuid.UUID) error {
	f.calls, f.caller, f.id = append(f.calls, "Delete"), c, id
	return f.err
}

func (f *fakeAssignments) RenderProbeAssignments(context.Context) ([]byte, error) {
	f.calls = append(f.calls, "Render")
	return f.rendered, f.err
}

var waitsOnFinance = inventory.ProbeAssignment{
	ID: uuid.MustParse("7e6d5c4b-3a29-4180-9f7e-6d5c4b3a2918"), DatabaseTargetID: financeProd.ID,
	ProbeDefinitionID: uuid.MustParse("1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"), IntervalSeconds: 30, Enabled: true,
	State: inventory.StateDraft, CollectorID: inventory.CollectorID(financeProd.ID, 30),
	CreatedAt: time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC),
}

func assignmentServer(t *testing.T, assignments *fakeAssignments) *Server {
	t.Helper()
	return newServer(t, Options{Auth: roleAuth{}, Site: testSite, ProbeAssignments: assignments})
}

var newAssignmentBody = `{"database_target_id":"` + financeProd.ID.String() + `","probe_definition_id":"1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"}`

// Viewers read assignments; operators change them and read the render;
// admins approve and reject them.
func TestProbeAssignmentRoles(t *testing.T) {
	id := waitsOnFinance.ID.String()
	for _, op := range []struct {
		method, path, body string
		least              auth.Role
		ok                 int
	}{
		{http.MethodGet, "/api/v1/probe-assignments", "", auth.Viewer, http.StatusOK},
		{http.MethodGet, "/api/v1/probe-assignments/" + id, "", auth.Viewer, http.StatusOK},
		{http.MethodGet, "/api/v1/probe-assignments/rendered", "", auth.Operator, http.StatusOK},
		{http.MethodPost, "/api/v1/probe-assignments", newAssignmentBody, auth.Operator, http.StatusCreated},
		{http.MethodPatch, "/api/v1/probe-assignments/" + id, `{"enabled":false}`, auth.Operator, http.StatusOK},
		{http.MethodDelete, "/api/v1/probe-assignments/" + id, "", auth.Operator, http.StatusNoContent},
		{http.MethodPost, "/api/v1/probe-assignments/" + id + "/approve", "", auth.Admin, http.StatusOK},
		{http.MethodPost, "/api/v1/probe-assignments/" + id + "/reject", `{}`, auth.Admin, http.StatusOK},
	} {
		t.Run(op.method+" "+op.path, func(t *testing.T) {
			if rec := send(t, assignmentServer(t, &fakeAssignments{assignment: waitsOnFinance}), "", op.method, op.path, op.body); rec.Code != http.StatusUnauthorized {
				t.Errorf("without a session: %d, want 401", rec.Code)
			}
			for _, role := range auth.Roles {
				assignments := &fakeAssignments{assignment: waitsOnFinance, rendered: []byte("managedCollectors: []\n")}
				rec := send(t, assignmentServer(t, assignments), string(role), op.method, op.path, op.body)
				allowed := auth.Allows([]string{string(role)}, op.least)
				switch {
				case allowed && rec.Code != op.ok:
					t.Errorf("as %s: %d %s, want %d", role, rec.Code, rec.Body, op.ok)
				case !allowed && (rec.Code != http.StatusForbidden || len(assignments.calls) != 0):
					t.Errorf("as %s: %d, calls %v; want 403 and no call", role, rec.Code, assignments.calls)
				}
			}
		})
	}
}

func TestCreateProbeAssignment(t *testing.T) {
	assignments := &fakeAssignments{assignment: waitsOnFinance}
	rec := send(t, assignmentServer(t, assignments), "operator", http.MethodPost, "/api/v1/probe-assignments",
		strings.TrimSuffix(newAssignmentBody, "}")+`,"interval_seconds":60,"timeout_ms":5000,"max_rows":200,"enabled":false}`)
	if rec.Code != http.StatusCreated || rec.Header().Get("Location") != "/api/v1/probe-assignments/"+waitsOnFinance.ID.String() {
		t.Fatalf("POST = %d %s, Location %q", rec.Code, rec.Body, rec.Header().Get("Location"))
	}
	in := assignments.in
	if in.DatabaseTargetID != financeProd.ID || in.ProbeDefinitionID != waitsOnFinance.ProbeDefinitionID || in.IntervalSeconds != 60 ||
		in.TimeoutMS != 5000 || in.MaxRows != 200 || in.Enabled == nil || *in.Enabled {
		t.Errorf("CreateProbeAssignment(%+v)", in)
	}
	if assignments.caller.Actor.Email != "operator@example.com" || assignments.caller.RequestID != rec.Header().Get(requestIDHeader) {
		t.Errorf("caller = %+v, want the operator and the request's ID", assignments.caller)
	}
	// Left out, the interval is the definition's and the policy the
	// collector's. The service checks the ranges; the state and the
	// approver are not the client's to set.
	assignments = &fakeAssignments{assignment: waitsOnFinance}
	if rec := send(t, assignmentServer(t, assignments), "operator", http.MethodPost, "/api/v1/probe-assignments", newAssignmentBody); rec.Code != http.StatusCreated ||
		assignments.in.IntervalSeconds != 0 || assignments.in.TimeoutMS != 0 || assignments.in.Enabled != nil {
		t.Errorf("defaults: %d, %+v", rec.Code, assignments.in)
	}
	for _, extra := range []string{`"state":"approved"`, `"approved_by":"` + uuid.NewString() + `"`} {
		assignments := &fakeAssignments{assignment: waitsOnFinance}
		rec := send(t, assignmentServer(t, assignments), "operator", http.MethodPost, "/api/v1/probe-assignments", strings.TrimSuffix(newAssignmentBody, "}")+","+extra+"}")
		if rec.Code != http.StatusBadRequest || len(assignments.calls) != 0 {
			t.Errorf("%s: %d, calls %v; want 400 and no call", extra, rec.Code, assignments.calls)
		}
	}
}

func TestProbeAssignmentErrors(t *testing.T) {
	id := waitsOnFinance.ID.String()
	for _, tc := range []struct {
		name, method, path, body string
		err                      error
		code                     int
	}{
		{"create: invalid", http.MethodPost, "/api/v1/probe-assignments", newAssignmentBody, &inventory.InvalidError{Field: "timeout_ms", Reason: "bad"}, http.StatusBadRequest},
		{"create: taken", http.MethodPost, "/api/v1/probe-assignments", newAssignmentBody, &inventory.ConflictError{Detail: "taken"}, http.StatusConflict},
		{"create: no target", http.MethodPost, "/api/v1/probe-assignments", newAssignmentBody, &inventory.OwnerError{Detail: "no target"}, http.StatusUnprocessableEntity},
		{"list: invalid", http.MethodGet, "/api/v1/probe-assignments", "", &inventory.InvalidError{Field: "cursor", Reason: "bad"}, http.StatusBadRequest},
		{"get: unknown", http.MethodGet, "/api/v1/probe-assignments/" + id, "", inventory.ErrNotFound, http.StatusNotFound},
		{"update: invalid", http.MethodPatch, "/api/v1/probe-assignments/" + id, `{"max_rows":1}`, &inventory.InvalidError{Field: "max_rows", Reason: "bad"}, http.StatusBadRequest},
		{"update: unknown", http.MethodPatch, "/api/v1/probe-assignments/" + id, `{"max_rows":1}`, inventory.ErrNotFound, http.StatusNotFound},
		{"update: another definition", http.MethodPatch, "/api/v1/probe-assignments/" + id, `{"max_rows":1}`, &inventory.ConflictError{Detail: "not a version"}, http.StatusConflict},
		{"update: no definition", http.MethodPatch, "/api/v1/probe-assignments/" + id, `{"max_rows":1}`, &inventory.OwnerError{Detail: "no definition"}, http.StatusUnprocessableEntity},
		{"approve: unknown", http.MethodPost, "/api/v1/probe-assignments/" + id + "/approve", "", inventory.ErrNotFound, http.StatusNotFound},
		{"approve: approved", http.MethodPost, "/api/v1/probe-assignments/" + id + "/approve", "", &inventory.ConflictError{Detail: "approved"}, http.StatusConflict},
		{"reject: invalid", http.MethodPost, "/api/v1/probe-assignments/" + id + "/reject", `{"reason":"x"}`, &inventory.InvalidError{Field: "reason", Reason: "bad"}, http.StatusBadRequest},
		{"reject: not a draft", http.MethodPost, "/api/v1/probe-assignments/" + id + "/reject", `{}`, &inventory.ConflictError{Detail: "approved"}, http.StatusConflict},
		{"delete: unknown", http.MethodDelete, "/api/v1/probe-assignments/" + id, "", inventory.ErrNotFound, http.StatusNotFound},
		{"render: failure", http.MethodGet, "/api/v1/probe-assignments/rendered", "", &inventory.RenderError{Reason: "bad"}, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := send(t, assignmentServer(t, &fakeAssignments{err: tc.err}), "admin", tc.method, tc.path, tc.body)
			if rec.Code != tc.code || rec.Header().Get("Content-Type") != "application/problem+json" {
				t.Fatalf("%d %s (%s), want %d problem+json", rec.Code, rec.Body, rec.Header().Get("Content-Type"), tc.code)
			}
		})
	}
}

// A change passes only the body's fields, and the target cannot be named.
func TestUpdateProbeAssignmentPassesOnlyTheBodysFields(t *testing.T) {
	assignments := &fakeAssignments{assignment: waitsOnFinance}
	rec := send(t, assignmentServer(t, assignments), "operator", http.MethodPatch, "/api/v1/probe-assignments/"+waitsOnFinance.ID.String(), `{"timeout_ms":0,"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d %s", rec.Code, rec.Body)
	}
	c := assignments.change
	if c.ProbeDefinitionID != nil || c.IntervalSeconds != nil || c.MaxRows != nil || c.TimeoutMS == nil || *c.TimeoutMS != 0 ||
		c.Enabled == nil || *c.Enabled || assignments.id != waitsOnFinance.ID {
		t.Errorf("UpdateProbeAssignment(%s, %+v)", assignments.id, c)
	}
	for _, body := range []string{`{"database_target_id":"` + uuid.NewString() + `"}`, `{"state":"approved"}`} {
		assignments := &fakeAssignments{assignment: waitsOnFinance}
		if rec := send(t, assignmentServer(t, assignments), "operator", http.MethodPatch, "/api/v1/probe-assignments/"+waitsOnFinance.ID.String(), body); rec.Code != http.StatusBadRequest || len(assignments.calls) != 0 {
			t.Errorf("PATCH %s = %d, calls %v; want 400 and no call", body, rec.Code, assignments.calls)
		}
	}
}

func TestApproveAndRejectProbeAssignment(t *testing.T) {
	approver := uuid.MustParse("5c1d7a3e-2b4f-4e8a-9c6d-1f0e3b2a4d5c")
	at := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	approved := waitsOnFinance
	approved.State, approved.ApprovedBy, approved.ApprovedAt = inventory.StateApproved, &approver, &at
	assignments := &fakeAssignments{assignment: approved}
	rec := send(t, assignmentServer(t, assignments), "admin", http.MethodPost, "/api/v1/probe-assignments/"+waitsOnFinance.ID.String()+"/approve", "")
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", rec.Code, rec.Body)
	}
	if body["state"] != "approved" || body["approved_by"] != approver.String() || body["approved_at"] != "2026-11-04T10:00:00Z" ||
		body["collector_id"] != "api-"+financeProd.ID.String()+"-30s" || assignments.caller.Actor.Email != "admin@example.com" {
		t.Errorf("approve body %v, caller %+v", body, assignments.caller)
	}
	assignments = &fakeAssignments{assignment: waitsOnFinance}
	if rec := send(t, assignmentServer(t, assignments), "admin", http.MethodPost, "/api/v1/probe-assignments/"+waitsOnFinance.ID.String()+"/reject", `{"reason":"use 60s"}`); rec.Code != http.StatusOK || assignments.reason != "use 60s" {
		t.Errorf("reject = %d, reason %q", rec.Code, assignments.reason)
	}
}

// A draft shows null for what it does not set, and the list passes its
// filter on.
func TestProbeAssignmentBody(t *testing.T) {
	assignments := &fakeAssignments{assignment: waitsOnFinance}
	rec := send(t, assignmentServer(t, assignments), "viewer", http.MethodGet, "/api/v1/probe-assignments?database_target_id="+financeProd.ID.String()+"&state=draft", "")
	var body struct {
		Items      []map[string]any `json:"items"`
		NextCursor string           `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Items) != 1 || body.NextCursor != "next" {
		t.Fatalf("GET = %d %s, %v", rec.Code, rec.Body, err)
	}
	item := body.Items[0]
	for _, key := range []string{"timeout_ms", "max_rows", "approved_by", "approved_at", "pull_request_url", "created_by"} {
		if v, ok := item[key]; !ok || v != nil {
			t.Errorf("%s = %v, want null", key, v)
		}
	}
	if assignments.filter.DatabaseTargetID != financeProd.ID || assignments.filter.State != inventory.StateDraft {
		t.Errorf("filter = %+v", assignments.filter)
	}
}

func TestRenderedProbeAssignments(t *testing.T) {
	const file = "# Generated\nmanagedCollectors: []\n"
	rec := send(t, assignmentServer(t, &fakeAssignments{rendered: []byte(file)}), "operator", http.MethodGet, "/api/v1/probe-assignments/rendered", "")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/yaml" || rec.Body.String() != file {
		t.Fatalf("GET rendered = %d %q %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
}
