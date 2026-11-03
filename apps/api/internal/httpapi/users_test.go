package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"heartbeat/apps/api/internal/audit"
	"heartbeat/apps/api/internal/auth"
	"heartbeat/apps/api/internal/users"
)

// fakeUsers is a Users that records its calls and answers with one user.
type fakeUsers struct {
	user users.User
	// err, when set, is what every method returns.
	err error

	calls []usersCall
}

type usersCall struct {
	method string
	caller users.Caller
	id     uuid.UUID
	arg    any
}

const userID = "8d2b8c1e-5a1f-4c39-9a3e-1f2d3c4b5a69"

func newFakeUsers() *fakeUsers {
	login := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	return &fakeUsers{user: users.User{
		ID: userID, Email: "alice@example.com", DisplayName: "Alice", Status: users.StatusActive, Roles: []string{"operator"},
		LastLoginAt: &login, CreatedAt: login.Add(-time.Hour), UpdatedAt: login,
	}}
}

func (f *fakeUsers) call(method string, caller users.Caller, id uuid.UUID, arg any) {
	f.calls = append(f.calls, usersCall{method, caller, id, arg})
}

func (f *fakeUsers) List(_ context.Context, status string) ([]users.User, error) {
	f.call("List", users.Caller{}, uuid.Nil, status)
	return []users.User{f.user}, f.err
}

func (f *fakeUsers) Get(_ context.Context, id uuid.UUID) (users.User, error) {
	f.call("Get", users.Caller{}, id, nil)
	return f.user, f.err
}

func (f *fakeUsers) Create(_ context.Context, caller users.Caller, in users.NewUser) (users.User, error) {
	f.call("Create", caller, uuid.Nil, in)
	return f.user, f.err
}

func (f *fakeUsers) Update(_ context.Context, caller users.Caller, id uuid.UUID, change users.Change) (users.User, error) {
	f.call("Update", caller, id, change)
	return f.user, f.err
}

func (f *fakeUsers) SetPassword(_ context.Context, caller users.Caller, id uuid.UUID, secret string) error {
	f.call("SetPassword", caller, id, secret)
	return f.err
}

func (f *fakeUsers) Grant(_ context.Context, caller users.Caller, id uuid.UUID, role string) (users.User, error) {
	f.call("Grant", caller, id, role)
	return f.user, f.err
}

func (f *fakeUsers) Revoke(_ context.Context, caller users.Caller, id uuid.UUID, role string) (users.User, error) {
	f.call("Revoke", caller, id, role)
	return f.user, f.err
}

// userOperations are the admins' user operations, each with a request an
// admin could send.
var userOperations = []struct{ method, path, body string }{
	{http.MethodGet, "/api/v1/users", ""},
	{http.MethodPost, "/api/v1/users", `{"email":"alice@example.com","password":"correct horse battery staple","roles":["operator"]}`},
	{http.MethodGet, "/api/v1/users/" + userID, ""},
	{http.MethodPatch, "/api/v1/users/" + userID, `{"display_name":"Alice"}`},
	{http.MethodPut, "/api/v1/users/" + userID + "/password", `{"password":"correct horse battery staple"}`},
	{http.MethodPut, "/api/v1/users/" + userID + "/roles/viewer", ""},
	{http.MethodDelete, "/api/v1/users/" + userID + "/roles/viewer", ""},
}

// Only admins manage users: an operator or a viewer is refused before the
// operation runs, an admin is served.
func TestUserOperationsAreForAdmins(t *testing.T) {
	for _, op := range userOperations {
		for _, tc := range []struct {
			roles  []string
			status int
		}{
			{[]string{"viewer"}, http.StatusForbidden},
			{[]string{"operator", "viewer"}, http.StatusForbidden},
			{[]string{"admin"}, 0},
		} {
			fake, admin := newFakeUsers(), newFakeAuth(tc.roles...)
			rec := serveRequest(newServer(t, Options{Auth: admin, Users: fake, Site: site}), browserRequest(op.method, op.path, op.body, admin.token))
			if tc.status != 0 {
				problemOf(t, rec, tc.status)
				if len(fake.calls) != 0 {
					t.Fatalf("%s %s by %v ran %s", op.method, op.path, tc.roles, fake.calls[0].method)
				}
				continue
			}
			if rec.Code >= 300 || len(fake.calls) != 1 {
				t.Fatalf("%s %s by an admin = %d, %d calls: %s", op.method, op.path, rec.Code, len(fake.calls), rec.Body)
			}
			// Changes carry the admin and the request, for the audit record.
			if c := fake.calls[0]; c.method != "List" && c.method != "Get" &&
				(c.caller.Actor != audit.UserActor(admin.principal.User.ID, admin.principal.User.Email) || c.caller.RequestID != rec.Header().Get(requestIDHeader)) {
				t.Fatalf("%s: caller %+v", c.method, c.caller)
			}
		}
	}
}

// A request without a session is answered 401 before its path parameters
// are read: a malformed ID tells an anonymous client nothing.
func TestUserOperationsNeedASessionBeforeTheirParameters(t *testing.T) {
	server := newServer(t, Options{Auth: newFakeAuth("admin"), Users: newFakeUsers(), Site: site})
	for _, req := range []*http.Request{
		browserRequest(http.MethodGet, "/api/v1/users/not-a-uuid", "", ""),
		browserRequest(http.MethodPut, "/api/v1/users/not-a-uuid/roles/viewer", "", ""),
	} {
		problemOf(t, serveRequest(server, req), http.StatusUnauthorized)
	}
	problemOf(t, serveRequest(server, browserRequest(http.MethodGet, "/api/v1/users/not-a-uuid", "", "dG9rZW4tb2YtdGhlLXdhbGxib2FyZC1zZXNzaW9uLTAx")), http.StatusBadRequest)
}

func TestCreateUser(t *testing.T) {
	fake, admin := newFakeUsers(), newFakeAuth("admin")
	body := `{"email":"Alice@Example.com","display_name":"Alice","password":"correct horse battery staple","roles":["operator","viewer"]}`
	rec := serveRequest(newServer(t, Options{Auth: admin, Users: fake, Site: site}), browserRequest(http.MethodPost, "/api/v1/users", body, admin.token))
	if rec.Code != http.StatusCreated || rec.Header().Get("Location") != "/api/v1/users/"+userID {
		t.Fatalf("status %d, Location %q: %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	in := fake.calls[0].arg.(users.NewUser)
	if in.Email != "Alice@Example.com" || in.DisplayName != "Alice" || in.Password != "correct horse battery staple" || strings.Join(in.Roles, ",") != "operator,viewer" {
		t.Fatalf("Create(%+v)", in)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["id"] != userID || got["email"] != "alice@example.com" || got["status"] != "active" || got["last_login_at"] != "2026-11-03T08:00:00Z" {
		t.Fatalf("body %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), "correct horse") || strings.Contains(rec.Body.String(), "password") {
		t.Fatalf("the body shows the password: %s", rec.Body)
	}
}

// Each users error is the problem its operation declares.
func TestUserOperationsProblems(t *testing.T) {
	for _, tc := range []struct {
		err          error
		method, path string
		body         string
		status       int
		detail       string
	}{
		{&users.InvalidError{Field: "email", Reason: "want an address"}, http.MethodPost, "/api/v1/users", userOperations[1].body, http.StatusBadRequest, "email: want an address"},
		{users.ErrEmailTaken, http.MethodPost, "/api/v1/users", userOperations[1].body, http.StatusConflict, "already exists"},
		{users.ErrUnknownRole, http.MethodPost, "/api/v1/users", userOperations[1].body, http.StatusBadRequest, "no such role"},
		{users.ErrNotFound, http.MethodGet, "/api/v1/users/" + userID, "", http.StatusNotFound, "no user has this ID"},
		{users.ErrNotFound, http.MethodPatch, "/api/v1/users/" + userID, `{"status":"disabled"}`, http.StatusNotFound, "no user"},
		{users.ErrLastAdmin, http.MethodPatch, "/api/v1/users/" + userID, `{"status":"disabled"}`, http.StatusConflict, "last active admin"},
		{users.ErrEmailTaken, http.MethodPatch, "/api/v1/users/" + userID, `{"email":"bob@example.com"}`, http.StatusConflict, "already exists"},
		{&users.InvalidError{Field: "password", Reason: "shorter than 15 characters"}, http.MethodPut, "/api/v1/users/" + userID + "/password", `{"password":"short"}`, http.StatusBadRequest, "password"},
		{users.ErrNotFound, http.MethodPut, "/api/v1/users/" + userID + "/password", userOperations[4].body, http.StatusNotFound, "no user"},
		{users.ErrUnknownRole, http.MethodPut, "/api/v1/users/" + userID + "/roles/owner", "", http.StatusNotFound, "no such role"},
		{users.ErrNotFound, http.MethodPut, "/api/v1/users/" + userID + "/roles/viewer", "", http.StatusNotFound, "no user"},
		{users.ErrLastAdmin, http.MethodDelete, "/api/v1/users/" + userID + "/roles/admin", "", http.StatusConflict, "last active admin"},
		{users.ErrLastRole, http.MethodDelete, "/api/v1/users/" + userID + "/roles/viewer", "", http.StatusConflict, "at least one role"},
		{users.ErrUnknownRole, http.MethodDelete, "/api/v1/users/" + userID + "/roles/owner", "", http.StatusNotFound, "no such role"},
		{errors.New("dial tcp 10.0.0.9:5432: connection refused"), http.MethodGet, "/api/v1/users", "", http.StatusInternalServerError, requestIDHeader},
	} {
		fake, admin := newFakeUsers(), newFakeAuth("admin")
		fake.err = tc.err
		rec := serveRequest(newServer(t, Options{Auth: admin, Users: fake, Site: site}), browserRequest(tc.method, tc.path, tc.body, admin.token))
		if detail := problemOf(t, rec, tc.status); !strings.Contains(detail, tc.detail) {
			t.Fatalf("%s %s with %v: detail %q, want %q in it", tc.method, tc.path, tc.err, detail, tc.detail)
		}
		if strings.Contains(rec.Body.String(), "10.0.0.9") {
			t.Fatalf("the problem shows the internal error: %s", rec.Body)
		}
	}
}

func TestUserOperationsPassTheRequest(t *testing.T) {
	fake, admin := newFakeUsers(), newFakeAuth("admin")
	server := newServer(t, Options{Auth: admin, Users: fake, Site: site})
	for _, req := range []*http.Request{
		browserRequest(http.MethodGet, "/api/v1/users?status=disabled", "", admin.token),
		browserRequest(http.MethodPatch, "/api/v1/users/"+userID, `{"email":"a@example.com","display_name":"A","status":"disabled"}`, admin.token),
		browserRequest(http.MethodPut, "/api/v1/users/"+userID+"/password", `{"password":"correct horse battery staple"}`, admin.token),
		browserRequest(http.MethodPut, "/api/v1/users/"+userID+"/roles/operator", "", admin.token),
		browserRequest(http.MethodDelete, "/api/v1/users/"+userID+"/roles/admin", "", admin.token),
	} {
		if rec := serveRequest(server, req); rec.Code >= 300 {
			t.Fatalf("%s %s = %d: %s", req.Method, req.URL, rec.Code, rec.Body)
		}
	}
	id := uuid.MustParse(userID)
	change := fake.calls[1].arg.(users.Change)
	if fake.calls[0].arg != "disabled" ||
		fake.calls[1].id != id || *change.Email != "a@example.com" || *change.DisplayName != "A" || *change.Status != "disabled" ||
		fake.calls[2].id != id || fake.calls[2].arg != "correct horse battery staple" ||
		fake.calls[3].method != "Grant" || fake.calls[3].arg != "operator" ||
		fake.calls[4].method != "Revoke" || fake.calls[4].arg != "admin" {
		t.Fatalf("calls %+v", fake.calls)
	}
	problemOf(t, serveRequest(server, browserRequest(http.MethodGet, "/api/v1/users?status=gone", "", admin.token)), http.StatusBadRequest)
}

// Any logged-in user changes their own password; every session of theirs
// ends, so the cookie is cleared.
func TestChangeOwnPassword(t *testing.T) {
	const body = `{"current_password":"correct horse battery staple","new_password":"a new horse battery staple"}`
	viewer := newFakeAuth("viewer")
	server := newServer(t, Options{Auth: viewer, Site: site})
	rec := serveRequest(server, browserRequest(http.MethodPut, "/api/v1/account/password", body, viewer.token))
	if rec.Code != http.StatusNoContent || !cleared(t, rec) {
		t.Fatalf("status %d, cookie cleared %t: %s", rec.Code, cleared(t, rec), rec.Body)
	}
	c := viewer.changes[0]
	if c.principal.User.ID != viewer.principal.User.ID || c.current != "correct horse battery staple" || c.next != "a new horse battery staple" ||
		c.client.RequestID != rec.Header().Get(requestIDHeader) {
		t.Fatalf("ChangePassword(%+v)", c)
	}

	for _, tc := range []struct {
		err        error
		status     int
		detail     string
		retryAfter string
	}{
		{auth.ErrWrongPassword, http.StatusForbidden, "the current password is wrong", ""},
		{&auth.RateLimitedError{RetryAfter: 90500 * time.Millisecond}, http.StatusTooManyRequests, "try again later", "91"},
		{&auth.InvalidError{}, http.StatusBadRequest, "", ""},
	} {
		viewer.changeErr = tc.err
		rec := serveRequest(server, browserRequest(http.MethodPut, "/api/v1/account/password", body, viewer.token))
		if detail := problemOf(t, rec, tc.status); !strings.Contains(detail, tc.detail) {
			t.Fatalf("%v: detail %q", tc.err, detail)
		}
		if rec.Header().Get("Retry-After") != tc.retryAfter || cleared(t, rec) {
			t.Fatalf("%v: Retry-After %q, cookie cleared %t", tc.err, rec.Header().Get("Retry-After"), cleared(t, rec))
		}
	}

	req := browserRequest(http.MethodPut, "/api/v1/account/password", body, viewer.token)
	req.Header.Del("Origin")
	problemOf(t, serveRequest(server, req), http.StatusForbidden)
	problemOf(t, serveRequest(server, browserRequest(http.MethodPut, "/api/v1/account/password", body, "")), http.StatusUnauthorized)
}
