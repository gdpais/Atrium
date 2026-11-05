package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	api "heartbeat/apps/api"
	"heartbeat/apps/api/internal/audit"
	"heartbeat/apps/api/internal/auth"
	"heartbeat/apps/api/internal/inventory"
)

// fakeInventory is an Inventory that records what it is asked and answers
// with fixed entities, or with err when it is set.
type fakeInventory struct {
	err   error
	calls []string

	caller    inventory.Caller
	id        uuid.UUID
	status    string
	appFilter inventory.ApplicationFilter
	cmpFilter inventory.ComponentFilter
	page      inventory.Page
	newEnv    inventory.NewEnvironment
	envChange inventory.EnvironmentChange
	newApp    inventory.NewApplication
	appChange inventory.ApplicationChange
	newCmp    inventory.NewComponent
	cmpChange inventory.ComponentChange
	srcFilter inventory.SourceFilter
	newSrc    inventory.NewSource
	srcChange inventory.SourceChange
	next      string
}

var (
	envID = uuid.MustParse("5d0c8a3e-1b2f-4c6d-8e9f-0a1b2c3d4e5f")
	appID = uuid.MustParse("7e2d9b4f-3c1a-4e5d-9f8a-1b2c3d4e5f60")
	cmpID = uuid.MustParse("9f3e0c5a-4d2b-4f6e-8a9b-2c3d4e5f6071")
	when  = time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)

	prod = inventory.Environment{
		ID: envID, Name: "Production", Slug: "prod", Type: "production", Status: "active",
		Labels: map[string]string{"region": "eu"}, CreatedAt: when, UpdatedAt: when,
	}
	claims = inventory.Application{
		ID: appID, EnvironmentID: envID, Name: "Claims", Platform: "outsystems_traditional",
		Criticality: "high", Status: "active", CreatedAt: when, UpdatedAt: when,
	}
	srcID      = uuid.MustParse("1a4f2b6c-5e3d-4a7b-9c8d-3e4f5a6b7c82")
	claimsLogs = inventory.Source{
		ID: srcID, ApplicationID: appID, EnvironmentID: envID, SourceType: "outsystems", IngestMode: "agentless",
		Status: "active", Config: map[string]any{"application_name": "Claims"}, CreatedAt: when, UpdatedAt: when,
		Observation: inventory.Observation{Status: inventory.Observed, Collectors: []string{"oslogs-prod"}, CheckedAt: when},
	}
	claimsWeb = inventory.Component{
		ID: cmpID, ApplicationID: appID, EnvironmentID: envID, Name: "ClaimsWeb", ComponentType: "module",
		Labels: map[string]string{}, CreatedAt: when, UpdatedAt: when,
	}
)

func (f *fakeInventory) called(name string) { f.calls = append(f.calls, name) }

func (f *fakeInventory) ListEnvironments(_ context.Context, status string) ([]inventory.Environment, error) {
	f.called("ListEnvironments")
	f.status = status
	return []inventory.Environment{prod}, f.err
}

func (f *fakeInventory) GetEnvironment(_ context.Context, id uuid.UUID) (inventory.Environment, error) {
	f.called("GetEnvironment")
	f.id = id
	return prod, f.err
}

func (f *fakeInventory) CreateEnvironment(_ context.Context, c inventory.Caller, in inventory.NewEnvironment) (inventory.Environment, error) {
	f.called("CreateEnvironment")
	f.caller, f.newEnv = c, in
	return prod, f.err
}

func (f *fakeInventory) UpdateEnvironment(_ context.Context, c inventory.Caller, id uuid.UUID, ch inventory.EnvironmentChange) (inventory.Environment, error) {
	f.called("UpdateEnvironment")
	f.caller, f.id, f.envChange = c, id, ch
	return prod, f.err
}

func (f *fakeInventory) DeleteEnvironment(_ context.Context, c inventory.Caller, id uuid.UUID) error {
	f.called("DeleteEnvironment")
	f.caller, f.id = c, id
	return f.err
}

func (f *fakeInventory) ListApplications(_ context.Context, filter inventory.ApplicationFilter, p inventory.Page) (inventory.ApplicationPage, error) {
	f.called("ListApplications")
	f.appFilter, f.page = filter, p
	return inventory.ApplicationPage{Items: []inventory.Application{claims}, NextCursor: f.next}, f.err
}

func (f *fakeInventory) GetApplication(_ context.Context, id uuid.UUID) (inventory.Application, error) {
	f.called("GetApplication")
	f.id = id
	return claims, f.err
}

func (f *fakeInventory) CreateApplication(_ context.Context, c inventory.Caller, in inventory.NewApplication) (inventory.Application, error) {
	f.called("CreateApplication")
	f.caller, f.newApp = c, in
	return claims, f.err
}

func (f *fakeInventory) UpdateApplication(_ context.Context, c inventory.Caller, id uuid.UUID, ch inventory.ApplicationChange) (inventory.Application, error) {
	f.called("UpdateApplication")
	f.caller, f.id, f.appChange = c, id, ch
	return claims, f.err
}

func (f *fakeInventory) DeleteApplication(_ context.Context, c inventory.Caller, id uuid.UUID) error {
	f.called("DeleteApplication")
	f.caller, f.id = c, id
	return f.err
}

func (f *fakeInventory) ListComponents(_ context.Context, filter inventory.ComponentFilter, p inventory.Page) (inventory.ComponentPage, error) {
	f.called("ListComponents")
	f.cmpFilter, f.page = filter, p
	return inventory.ComponentPage{Items: []inventory.Component{claimsWeb}, NextCursor: f.next}, f.err
}

func (f *fakeInventory) GetComponent(_ context.Context, id uuid.UUID) (inventory.Component, error) {
	f.called("GetComponent")
	f.id = id
	return claimsWeb, f.err
}

func (f *fakeInventory) CreateComponent(_ context.Context, c inventory.Caller, in inventory.NewComponent) (inventory.Component, error) {
	f.called("CreateComponent")
	f.caller, f.newCmp = c, in
	return claimsWeb, f.err
}

func (f *fakeInventory) UpdateComponent(_ context.Context, c inventory.Caller, id uuid.UUID, ch inventory.ComponentChange) (inventory.Component, error) {
	f.called("UpdateComponent")
	f.caller, f.id, f.cmpChange = c, id, ch
	return claimsWeb, f.err
}

func (f *fakeInventory) DeleteComponent(_ context.Context, c inventory.Caller, id uuid.UUID) error {
	f.called("DeleteComponent")
	f.caller, f.id = c, id
	return f.err
}

func (f *fakeInventory) ListSources(_ context.Context, filter inventory.SourceFilter, p inventory.Page) (inventory.SourcePage, error) {
	f.called("ListSources")
	f.srcFilter, f.page = filter, p
	return inventory.SourcePage{Items: []inventory.Source{claimsLogs}, NextCursor: f.next}, f.err
}

func (f *fakeInventory) GetSource(_ context.Context, id uuid.UUID) (inventory.Source, error) {
	f.called("GetSource")
	f.id = id
	return claimsLogs, f.err
}

func (f *fakeInventory) CreateSource(_ context.Context, c inventory.Caller, in inventory.NewSource) (inventory.Source, error) {
	f.called("CreateSource")
	f.caller, f.newSrc = c, in
	return claimsLogs, f.err
}

func (f *fakeInventory) UpdateSource(_ context.Context, c inventory.Caller, id uuid.UUID, ch inventory.SourceChange) (inventory.Source, error) {
	f.called("UpdateSource")
	f.caller, f.id, f.srcChange = c, id, ch
	return claimsLogs, f.err
}

func (f *fakeInventory) DeleteSource(_ context.Context, c inventory.Caller, id uuid.UUID) error {
	f.called("DeleteSource")
	f.caller, f.id = c, id
	return f.err
}

var _ Inventory = (*fakeInventory)(nil)

// inventoryServer is a Server whose sessions have roles.
func inventoryServer(t *testing.T, store *fakeInventory, roles ...string) (*Server, *fakeAuth) {
	t.Helper()
	fake := newFakeAuth(roles...)
	return newServer(t, Options{Auth: fake, Site: site, Inventory: store}), fake
}

// inventoryOperations is one call of each inventory operation, with a body
// it accepts.
var inventoryOperations = []struct {
	method, path, body string
	// call is the Inventory method it reaches.
	call string
}{
	{http.MethodGet, "/api/v1/environments", "", "ListEnvironments"},
	{http.MethodPost, "/api/v1/environments", `{"name":"Production","slug":"prod","type":"production"}`, "CreateEnvironment"},
	{http.MethodGet, "/api/v1/environments/" + envID.String(), "", "GetEnvironment"},
	{http.MethodPatch, "/api/v1/environments/" + envID.String(), `{"status":"disabled"}`, "UpdateEnvironment"},
	{http.MethodDelete, "/api/v1/environments/" + envID.String(), "", "DeleteEnvironment"},
	{http.MethodGet, "/api/v1/applications", "", "ListApplications"},
	{http.MethodPost, "/api/v1/applications", `{"environment_id":"` + envID.String() + `","name":"Claims","platform":"outsystems_traditional"}`, "CreateApplication"},
	{http.MethodGet, "/api/v1/applications/" + appID.String(), "", "GetApplication"},
	{http.MethodPatch, "/api/v1/applications/" + appID.String(), `{"criticality":"critical"}`, "UpdateApplication"},
	{http.MethodDelete, "/api/v1/applications/" + appID.String(), "", "DeleteApplication"},
	{http.MethodGet, "/api/v1/components", "", "ListComponents"},
	{http.MethodPost, "/api/v1/components", `{"application_id":"` + appID.String() + `","name":"ClaimsWeb","component_type":"module"}`, "CreateComponent"},
	{http.MethodGet, "/api/v1/components/" + cmpID.String(), "", "GetComponent"},
	{http.MethodPatch, "/api/v1/components/" + cmpID.String(), `{"runtime_name":"ClaimsWeb"}`, "UpdateComponent"},
	{http.MethodDelete, "/api/v1/components/" + cmpID.String(), "", "DeleteComponent"},
	{http.MethodGet, "/api/v1/telemetry-sources", "", "ListSources"},
	{http.MethodPost, "/api/v1/telemetry-sources", `{"application_id":"` + appID.String() + `","source_type":"outsystems","ingest_mode":"agentless","config":{"application_name":"Claims"}}`, "CreateSource"},
	{http.MethodGet, "/api/v1/telemetry-sources/" + srcID.String(), "", "GetSource"},
	{http.MethodPatch, "/api/v1/telemetry-sources/" + srcID.String(), `{"status":"disabled"}`, "UpdateSource"},
	{http.MethodDelete, "/api/v1/telemetry-sources/" + srcID.String(), "", "DeleteSource"},
}

// Reads need the viewer role and every change the operator role, as the
// spec declares: a viewer is refused each change before it reaches the
// inventory, a user without a role is refused everything, and nobody is
// served without a session.
func TestInventoryRoles(t *testing.T) {
	rules, err := accessRules(api.Spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range inventoryOperations {
		pattern := op.method + " " + patternOf(op.path)
		want := auth.Operator
		if op.method == http.MethodGet {
			want = auth.Viewer
		}
		if got := rules[pattern]; got != (rule{role: want}) {
			t.Errorf("%s: rule %+v, want role %s", pattern, got, want)
		}

		for _, tc := range []struct {
			name    string
			roles   []string
			session bool
			allowed bool
		}{
			{"no session", []string{"admin"}, false, false},
			{"no role", nil, true, false},
			{"viewer", []string{"viewer"}, true, want == auth.Viewer},
			{"operator", []string{"operator"}, true, true},
			{"admin", []string{"admin"}, true, true},
		} {
			store := &fakeInventory{}
			server, fake := inventoryServer(t, store, tc.roles...)
			token := ""
			if tc.session {
				token = fake.token
			}
			rec := serveRequest(server, browserRequest(op.method, op.path, op.body, token))
			switch {
			case !tc.session:
				problemOf(t, rec, http.StatusUnauthorized)
			case !tc.allowed:
				if detail := problemOf(t, rec, http.StatusForbidden); !strings.Contains(detail, string(want)) {
					t.Errorf("%s %s as %s: detail %q does not name the %s role", op.method, op.path, tc.name, detail, want)
				}
			case rec.Code >= 300:
				t.Errorf("%s %s as %s = %d: %s", op.method, op.path, tc.name, rec.Code, rec.Body)
			}
			if reached := len(store.calls) > 0; reached != (tc.session && tc.allowed) {
				t.Errorf("%s %s as %s: inventory reached %t (calls %v)", op.method, op.path, tc.name, reached, store.calls)
			} else if reached && store.calls[0] != op.call {
				t.Errorf("%s %s reached %v, want %s", op.method, op.path, store.calls, op.call)
			}
		}
	}
}

// patternOf turns a request path into the spec's path, with {id}.
func patternOf(path string) string {
	for _, id := range []uuid.UUID{envID, appID, cmpID, srcID} {
		path = strings.Replace(path, id.String(), "{id}", 1)
	}
	return path
}

// The service's errors become the problems the operation declares; one it
// does not declare is a server error, never an undeclared status.
func TestInventoryErrors(t *testing.T) {
	notFound, invalid := inventory.ErrNotFound, &inventory.InvalidError{Field: "slug", Reason: `"Prod": want lower-case letters`}
	conflict := &inventory.ConflictError{Detail: "the environment still has applications, so it cannot be deleted: disable it instead"}
	owner := &inventory.OwnerError{Detail: "environment 5d0c8a3e-1b2f-4c6d-8e9f-0a1b2c3d4e5f does not exist"}
	managed := &inventory.ConflictError{Detail: "the environment is managed by its environment's inventory file (ADR 0021): change it there, by pull request"}
	for _, tc := range []struct {
		op     int // index in inventoryOperations
		err    error
		status int
		detail string
	}{
		{0, invalid, http.StatusBadRequest, "slug: "},
		{1, invalid, http.StatusBadRequest, "slug: "},
		{1, conflict, http.StatusConflict, "still has applications"},
		{1, owner, http.StatusInternalServerError, ""}, // createEnvironment has no owner
		{2, notFound, http.StatusNotFound, "no environment has this ID"},
		{3, notFound, http.StatusNotFound, "no environment"},
		{3, managed, http.StatusConflict, "inventory file"},
		{4, conflict, http.StatusConflict, "disable it instead"},
		{4, notFound, http.StatusNotFound, "no environment"},
		{5, invalid, http.StatusBadRequest, "slug: "},
		{6, owner, http.StatusUnprocessableEntity, "does not exist"},
		{6, conflict, http.StatusConflict, "applications"},
		{7, notFound, http.StatusNotFound, "no application has this ID"},
		{8, conflict, http.StatusConflict, "applications"},
		{9, conflict, http.StatusConflict, "applications"},
		{10, invalid, http.StatusBadRequest, "slug: "},
		{11, owner, http.StatusUnprocessableEntity, "does not exist"},
		{12, notFound, http.StatusNotFound, "no component has this ID"},
		{13, conflict, http.StatusConflict, "applications"},
		{14, notFound, http.StatusNotFound, "no component"},
		{14, conflict, http.StatusConflict, "applications"},
		{14, owner, http.StatusInternalServerError, ""}, // deleteComponent has no owner to name
		{2, fmt.Errorf("read: %w", context.DeadlineExceeded), http.StatusInternalServerError, ""},
		{15, invalid, http.StatusBadRequest, "slug: "},
		{16, invalid, http.StatusBadRequest, "slug: "},
		{16, conflict, http.StatusConflict, "applications"},
		{16, owner, http.StatusUnprocessableEntity, "does not exist"},
		{17, notFound, http.StatusNotFound, "no telemetry source has this ID"},
		{18, conflict, http.StatusConflict, "applications"},
		{18, owner, http.StatusInternalServerError, ""}, // updateTelemetrySource has no owner to name
		{19, notFound, http.StatusNotFound, "no telemetry source"},
		{19, conflict, http.StatusConflict, "applications"},
	} {
		op := inventoryOperations[tc.op]
		server, fake := inventoryServer(t, &fakeInventory{err: tc.err}, "operator")
		rec := serveRequest(server, browserRequest(op.method, op.path, op.body, fake.token))
		detail := problemOf(t, rec, tc.status)
		if tc.status == http.StatusInternalServerError {
			if strings.Contains(detail, tc.err.Error()) {
				t.Errorf("%s %s: a server error shows its cause: %q", op.method, op.path, detail)
			}
			continue
		}
		if !strings.Contains(detail, tc.detail) {
			t.Errorf("%s %s with %v: detail %q, want it to hold %q", op.method, op.path, tc.err, detail, tc.detail)
		}
	}
}

// The ownership path cannot be changed through a body: a PATCH that names
// the owner, or the slug, is refused whole, and so is any member the
// schema does not list; the inventory is never asked.
func TestBodiesAreClosed(t *testing.T) {
	for _, tc := range []struct {
		method, path, body, member string
	}{
		{http.MethodPatch, "/api/v1/applications/" + appID.String(), `{"environment_id":"` + uuid.NewString() + `"}`, "environment_id"},
		{http.MethodPatch, "/api/v1/applications/" + appID.String(), `{"name":"Claims","environment_id":"` + uuid.NewString() + `"}`, "environment_id"},
		{http.MethodPatch, "/api/v1/components/" + cmpID.String(), `{"application_id":"` + uuid.NewString() + `"}`, "application_id"},
		{http.MethodPatch, "/api/v1/components/" + cmpID.String(), `{"environment_id":"` + uuid.NewString() + `"}`, "environment_id"},
		{http.MethodPatch, "/api/v1/environments/" + envID.String(), `{"slug":"production"}`, "slug"},
		{http.MethodPost, "/api/v1/components", `{"application_id":"` + appID.String() + `","environment_id":"` + envID.String() + `","name":"ClaimsWeb","component_type":"module"}`, "environment_id"},
		{http.MethodPost, "/api/v1/environments", `{"name":"Production","slug":"prod","type":"production","id":"` + envID.String() + `"}`, "id"},
		{http.MethodPost, "/api/v1/environments", `{"name":"Production","slug":"prod","type":"production","Name":"x"}`, "Name"},
		{http.MethodPatch, "/api/v1/telemetry-sources/" + srcID.String(), `{"application_id":"` + uuid.NewString() + `"}`, "application_id"},
		{http.MethodPatch, "/api/v1/telemetry-sources/" + srcID.String(), `{"source_type":"outsystems"}`, "source_type"},
		{http.MethodPatch, "/api/v1/telemetry-sources/" + srcID.String(), `{"environment_id":"` + envID.String() + `"}`, "environment_id"},
		{http.MethodPost, "/api/v1/telemetry-sources", `{"application_id":"` + appID.String() + `","environment_id":"` + envID.String() + `","source_type":"outsystems","ingest_mode":"agentless","config":{"application_name":"Claims"}}`, "environment_id"},
	} {
		store := &fakeInventory{}
		server, fake := inventoryServer(t, store, "operator")
		detail := problemOf(t, serveRequest(server, browserRequest(tc.method, tc.path, tc.body, fake.token)), http.StatusBadRequest)
		if !strings.Contains(detail, `unknown field "`+tc.member+`"`) {
			t.Errorf("%s %s %s: detail %q does not name %s", tc.method, tc.path, tc.body, detail, tc.member)
		}
		if len(store.calls) != 0 {
			t.Errorf("%s %s %s reached the inventory: %v", tc.method, tc.path, tc.body, store.calls)
		}
	}

	t.Run("more than one value", func(t *testing.T) {
		store := &fakeInventory{}
		server, fake := inventoryServer(t, store, "operator")
		problemOf(t, serveRequest(server, browserRequest(http.MethodPatch, "/api/v1/environments/"+envID.String(), `{"status":"disabled"} {"status":"active"}`, fake.token)), http.StatusBadRequest)
		if len(store.calls) != 0 {
			t.Fatal("reached the inventory")
		}
	})

	t.Run("the login body is closed too", func(t *testing.T) {
		server, fake := inventoryServer(t, &fakeInventory{}, "viewer")
		problemOf(t, serveRequest(server, browserRequest(http.MethodPost, "/api/v1/session", `{"email":"wall@example.com","password":"correct horse battery staple","remember":true}`, "")), http.StatusBadRequest)
		if len(fake.logins) != 0 {
			t.Fatal("logged in with an unknown member")
		}
	})
}

// A creation answers 201 with the entity and its Location; the change is
// the logged-in user's, in the request whose ID the response carries.
func TestInventoryCreatesAndAttributes(t *testing.T) {
	for _, op := range []int{1, 6, 11, 16} {
		o := inventoryOperations[op]
		store := &fakeInventory{}
		server, fake := inventoryServer(t, store, "operator")
		rec := serveRequest(server, browserRequest(o.method, o.path, o.body, fake.token))
		if rec.Code != http.StatusCreated {
			t.Fatalf("%s %s = %d: %s", o.method, o.path, rec.Code, rec.Body)
		}
		var body struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if want := o.path + "/" + body.ID; rec.Header().Get("Location") != want {
			t.Errorf("%s: Location %q, want %q", o.path, rec.Header().Get("Location"), want)
		}
		user := fake.principal.User
		want := inventory.Caller{Actor: audit.UserActor(user.ID, user.Email), RequestID: rec.Header().Get(requestIDHeader)}
		if store.caller != want || want.RequestID == "" {
			t.Errorf("%s: caller %+v, want %+v", o.path, store.caller, want)
		}
	}
}

// What the bodies and parameters say reaches the inventory as they say
// it: absent fields stay nil, so that a PATCH keeps them.
func TestInventoryPassesTheRequestOn(t *testing.T) {
	t.Run("create with defaults left to the service", func(t *testing.T) {
		store := &fakeInventory{}
		server, fake := inventoryServer(t, store, "operator")
		serveRequest(server, browserRequest(http.MethodPost, "/api/v1/environments",
			`{"name":"Production","slug":"prod","type":"production","labels":{"region":"eu"}}`, fake.token))
		want := inventory.NewEnvironment{Name: "Production", Slug: "prod", Type: "production", Labels: map[string]string{"region": "eu"}}
		if fmt.Sprint(store.newEnv) != fmt.Sprint(want) {
			t.Fatalf("new environment %+v, want %+v", store.newEnv, want)
		}
	})

	t.Run("a PATCH passes only what it holds", func(t *testing.T) {
		store := &fakeInventory{}
		server, fake := inventoryServer(t, store, "operator")
		serveRequest(server, browserRequest(http.MethodPatch, "/api/v1/environments/"+envID.String(), `{"labels":{}}`, fake.token))
		c := store.envChange
		if store.id != envID || c.Name != nil || c.Type != nil || c.Description != nil || c.Status != nil || c.Labels == nil || len(*c.Labels) != 0 {
			t.Fatalf("change %+v of %s: want only labels, emptied", c, store.id)
		}

		serveRequest(server, browserRequest(http.MethodPatch, "/api/v1/applications/"+appID.String(), `{"status":"disabled"}`, fake.token))
		a := store.appChange
		if a.Status == nil || *a.Status != "disabled" || a.Name != nil || a.Platform != nil || a.OwnerTeam != nil || a.Criticality != nil {
			t.Fatalf("change %+v: want only the status", a)
		}
	})

	t.Run("list parameters", func(t *testing.T) {
		store := &fakeInventory{next: "bmV4dA"}
		server, fake := inventoryServer(t, store, "viewer")
		rec := serveRequest(server, browserRequest(http.MethodGet, "/api/v1/applications?environment_id="+envID.String()+"&status=disabled&limit=2&cursor=abc", "", fake.token))
		if rec.Code != http.StatusOK {
			t.Fatalf("list = %d: %s", rec.Code, rec.Body)
		}
		if store.appFilter != (inventory.ApplicationFilter{EnvironmentID: envID, Status: "disabled"}) || store.page != (inventory.Page{Limit: 2, Cursor: "abc"}) {
			t.Fatalf("filter %+v, page %+v", store.appFilter, store.page)
		}
		var page struct {
			Items      []json.RawMessage `json:"items"`
			NextCursor *string           `json:"next_cursor"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 || page.NextCursor == nil || *page.NextCursor != "bmV4dA" {
			t.Fatalf("page %s: want one item and the next cursor", rec.Body)
		}

		serveRequest(server, browserRequest(http.MethodGet, "/api/v1/components?application_id="+appID.String()+"&limit=0", "", fake.token))
		if store.cmpFilter.ApplicationID != appID || store.page.Limit >= 1 {
			t.Fatalf("filter %+v, page %+v: want the application, and limit=0 passed on as out of range", store.cmpFilter, store.page)
		}

		store.next = ""
		rec = serveRequest(server, browserRequest(http.MethodGet, "/api/v1/components", "", fake.token))
		if strings.Contains(rec.Body.String(), "next_cursor") {
			t.Fatalf("last page %s has a next_cursor", rec.Body)
		}
	})

	t.Run("a malformed ID or filter is refused once the session is checked", func(t *testing.T) {
		store := &fakeInventory{}
		server, fake := inventoryServer(t, store, "viewer")
		for _, path := range []string{"/api/v1/environments/prod", "/api/v1/applications?environment_id=prod", "/api/v1/applications?limit=many"} {
			problemOf(t, serveRequest(server, browserRequest(http.MethodGet, path, "", fake.token)), http.StatusBadRequest)
			problemOf(t, serveRequest(server, browserRequest(http.MethodGet, path, "", "")), http.StatusUnauthorized)
		}
		if len(store.calls) != 0 {
			t.Fatalf("reached the inventory: %v", store.calls)
		}
	})
}

// A component shows the environment it derives through its application.
func TestComponentShowsItsDerivedEnvironment(t *testing.T) {
	server, fake := inventoryServer(t, &fakeInventory{}, "viewer")
	rec := serveRequest(server, browserRequest(http.MethodGet, "/api/v1/components/"+cmpID.String(), "", fake.token))
	var c struct {
		ApplicationID string `json:"application_id"`
		EnvironmentID string `json:"environment_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	if c.ApplicationID != appID.String() || c.EnvironmentID != envID.String() {
		t.Fatalf("component %s: want its application and that application's environment", rec.Body)
	}
}

func TestInventoryWithoutAService(t *testing.T) {
	fake := newFakeAuth("operator")
	server := newServer(t, Options{Auth: fake, Site: site})
	for _, op := range inventoryOperations {
		problemOf(t, serveRequest(server, browserRequest(op.method, op.path, op.body, fake.token)), http.StatusInternalServerError)
	}
}

// A source shows its derived environment and its observation; what a
// request says reaches the inventory as it says it.
func TestTelemetrySourceHandlers(t *testing.T) {
	t.Run("the observation and the derived environment", func(t *testing.T) {
		server, fake := inventoryServer(t, &fakeInventory{}, "viewer")
		rec := serveRequest(server, browserRequest(http.MethodGet, "/api/v1/telemetry-sources/"+srcID.String(), "", fake.token))
		var got struct {
			EnvironmentID string         `json:"environment_id"`
			Config        map[string]any `json:"config"`
			Observation   struct {
				Status     string   `json:"status"`
				Collectors []string `json:"collectors"`
				CheckedAt  string   `json:"checked_at"`
				Detail     *string  `json:"detail"`
			} `json:"observation"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		o := got.Observation
		if got.EnvironmentID != envID.String() || got.Config["application_name"] != "Claims" ||
			o.Status != "observed" || len(o.Collectors) != 1 || o.Collectors[0] != "oslogs-prod" || o.CheckedAt == "" || o.Detail != nil {
			t.Fatalf("source %s", rec.Body)
		}
	})

	t.Run("an unknown observation has no checked_at and says why", func(t *testing.T) {
		unknown := claimsLogs
		unknown.Observation = inventory.Observation{Status: inventory.ObservationUnknown, Detail: "the collector did not answer"}
		out := sourceOut(unknown)
		if out.Observation.CheckedAt != nil || out.Observation.Detail == nil || *out.Observation.Detail != "the collector did not answer" || out.Observation.Collectors == nil {
			t.Fatalf("observation %+v", out.Observation)
		}
	})

	t.Run("create, change and list pass the request on", func(t *testing.T) {
		store := &fakeInventory{}
		server, fake := inventoryServer(t, store, "operator")
		serveRequest(server, browserRequest(http.MethodPost, "/api/v1/telemetry-sources",
			`{"application_id":"`+appID.String()+`","source_type":"outsystems","ingest_mode":"otlp","config":{"application_name":"Claims"}}`, fake.token))
		if store.newSrc.ApplicationID != appID || store.newSrc.SourceType != "outsystems" || store.newSrc.IngestMode != "otlp" ||
			store.newSrc.Status != "" || store.newSrc.Config["application_name"] != "Claims" {
			t.Fatalf("new source %+v", store.newSrc)
		}
		serveRequest(server, browserRequest(http.MethodPatch, "/api/v1/telemetry-sources/"+srcID.String(), `{"config":{"application_name":"Claims2"}}`, fake.token))
		c := store.srcChange
		if c.IngestMode != nil || c.Status != nil || c.Config == nil || (*c.Config)["application_name"] != "Claims2" {
			t.Fatalf("change %+v", c)
		}
		serveRequest(server, browserRequest(http.MethodGet, "/api/v1/telemetry-sources?environment_id="+envID.String()+"&source_type=outsystems&status=active&limit=5", "", fake.token))
		if store.srcFilter != (inventory.SourceFilter{EnvironmentID: envID, SourceType: "outsystems", Status: "active"}) || store.page.Limit != 5 {
			t.Fatalf("filter %+v page %+v", store.srcFilter, store.page)
		}
	})
}
