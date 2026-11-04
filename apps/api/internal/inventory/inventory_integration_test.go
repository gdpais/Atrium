//go:build integration

package inventory_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"heartbeat/apps/api/internal/audit"
	"heartbeat/apps/api/internal/audit/audittest"
	"heartbeat/apps/api/internal/inventory"
	"heartbeat/apps/api/internal/testenv"
)

var operator = inventory.Caller{
	Actor:     audit.UserActor("0b8f4c1e-6f0e-4d7a-9a51-3c0d2f1b7e10", "operator@example.com"),
	RequestID: "req-inventory-test",
}

// fixture is one PostgreSQL database, migrated, shared by a test's
// subtests: each makes its own entities, with names no other uses.
type fixture struct {
	pool *pgxpool.Pool
	svc  *inventory.Service
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	_, _, pool := testenv.MigratedPostgres(t)
	return fixture{pool: pool, svc: inventory.New(pool)}
}

// record is an audit record, as the outbox holds it.
type record struct {
	Action     string          `json:"action"`
	EntityType string          `json:"entity_type"`
	EntityID   string          `json:"entity_id"`
	RequestID  string          `json:"request_id"`
	Actor      audit.Actor     `json:"actor"`
	Before     json.RawMessage `json:"before"`
	After      json.RawMessage `json:"after"`
}

// mark returns the outbox's newest seq, so recordsSince sees only what
// follows.
func (f fixture) mark(t *testing.T) int64 {
	t.Helper()
	var seq int64
	if err := f.pool.QueryRow(context.Background(), "select coalesce(max(seq), 0) from audit_outbox").Scan(&seq); err != nil {
		t.Fatal(err)
	}
	return seq
}

// recordsSince returns the records written after mark, each checked
// against the schema.
func (f fixture) recordsSince(t *testing.T, mark int64) []record {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), "select record::text from audit_outbox where seq > $1 order by seq", mark)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []record
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		audittest.Validate(t, []byte(raw))
		var r record
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// one checks that exactly one record followed mark, about entityType id
// with action, by the operator, and returns it.
func (f fixture) one(t *testing.T, mark int64, action, entityType string, id uuid.UUID) record {
	t.Helper()
	return f.oneBy(t, mark, operator.Actor, operator.RequestID, action, entityType, id)
}

// oneBy is one, by actor in requestID; "" accepts any request.
func (f fixture) oneBy(t *testing.T, mark int64, actor audit.Actor, requestID, action, entityType string, id uuid.UUID) record {
	t.Helper()
	records := f.recordsSince(t, mark)
	if len(records) != 1 {
		t.Fatalf("%d audit records, want 1: %+v", len(records), records)
	}
	r := records[0]
	if r.Action != action || r.EntityType != entityType || r.EntityID != id.String() {
		t.Fatalf("record %s %s %s, want %s %s %s", r.Action, r.EntityType, r.EntityID, action, entityType, id)
	}
	if r.Actor != actor || (requestID != "" && r.RequestID != requestID) {
		t.Fatalf("record by %+v in %q, want %+v in %q", r.Actor, r.RequestID, actor, requestID)
	}
	return r
}

func (f fixture) none(t *testing.T, mark int64) {
	t.Helper()
	if records := f.recordsSince(t, mark); len(records) != 0 {
		t.Fatalf("%d audit records, want none: %+v", len(records), records)
	}
}

func decode[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return v
}

func ptr[T any](v T) *T { return &v }

// null reports a record's before or after that holds no entity.
func null(raw json.RawMessage) bool { return len(raw) == 0 || string(raw) == "null" }

func (f fixture) environment(t *testing.T, slug string) inventory.Environment {
	t.Helper()
	e, err := f.svc.CreateEnvironment(context.Background(), operator, inventory.NewEnvironment{Name: "Env " + slug, Slug: slug, Type: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (f fixture) application(t *testing.T, env uuid.UUID, name string) inventory.Application {
	t.Helper()
	a, err := f.svc.CreateApplication(context.Background(), operator, inventory.NewApplication{EnvironmentID: env, Name: name, Platform: "outsystems_traditional"})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func wantConflict(t *testing.T, err error, contains string) {
	t.Helper()
	var conflict *inventory.ConflictError
	if !errors.As(err, &conflict) || !strings.Contains(conflict.Detail, contains) {
		t.Fatalf("err = %v, want a ConflictError about %q", err, contains)
	}
}

func wantOwner(t *testing.T, err error) {
	t.Helper()
	var owner *inventory.OwnerError
	if !errors.As(err, &owner) {
		t.Fatalf("err = %v, want an OwnerError", err)
	}
}

func TestEnvironments(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	t.Run("create, read, change and delete, each audited", func(t *testing.T) {
		mark := f.mark(t)
		created, err := f.svc.CreateEnvironment(ctx, operator, inventory.NewEnvironment{
			Name: "  Production  ", Slug: "prod", Type: "production",
			Description: "The live estate.\nEU only.", Labels: map[string]string{"region": "eu"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if created.Name != "Production" || created.Status != inventory.StatusActive || created.Labels["region"] != "eu" {
			t.Fatalf("created %+v: want the name trimmed, active, with its labels", created)
		}
		r := f.one(t, mark, audit.ActionCreate, audit.EntityEnvironment, created.ID)
		if !null(r.Before) || decode[inventory.Environment](t, r.After).Slug != "prod" {
			t.Fatalf("create record before %s after %s: want no before and the environment after", r.Before, r.After)
		}

		got, err := f.svc.GetEnvironment(ctx, created.ID)
		if err != nil || got.ID != created.ID || got.Description != created.Description {
			t.Fatalf("get = %+v, %v; want %+v", got, err, created)
		}

		mark = f.mark(t)
		changed, err := f.svc.UpdateEnvironment(ctx, operator, created.ID, inventory.EnvironmentChange{
			Status: ptr(inventory.StatusDisabled), Labels: &map[string]string{},
		})
		if err != nil {
			t.Fatal(err)
		}
		if changed.Status != inventory.StatusDisabled || len(changed.Labels) != 0 || changed.Name != "Production" || changed.Slug != "prod" {
			t.Fatalf("changed %+v: want disabled, no labels, the rest kept", changed)
		}
		if !changed.UpdatedAt.After(created.UpdatedAt) {
			t.Fatalf("updated_at %s, want after %s", changed.UpdatedAt, created.UpdatedAt)
		}
		r = f.one(t, mark, audit.ActionUpdate, audit.EntityEnvironment, created.ID)
		if decode[inventory.Environment](t, r.Before).Status != inventory.StatusActive || decode[inventory.Environment](t, r.After).Status != inventory.StatusDisabled {
			t.Fatalf("update record before %s after %s: want active, then disabled", r.Before, r.After)
		}

		mark = f.mark(t)
		if err := f.svc.DeleteEnvironment(ctx, operator, created.ID); err != nil {
			t.Fatal(err)
		}
		r = f.one(t, mark, audit.ActionDelete, audit.EntityEnvironment, created.ID)
		if !null(r.After) || decode[inventory.Environment](t, r.Before).ID != created.ID {
			t.Fatalf("delete record before %s after %s: want the environment before and no after", r.Before, r.After)
		}
		if _, err := f.svc.GetEnvironment(ctx, created.ID); !errors.Is(err, inventory.ErrNotFound) {
			t.Fatalf("get after delete = %v, want ErrNotFound", err)
		}
	})

	t.Run("a change that changes nothing writes and audits nothing", func(t *testing.T) {
		e := f.environment(t, "noop")
		mark := f.mark(t)
		got, err := f.svc.UpdateEnvironment(ctx, operator, e.ID, inventory.EnvironmentChange{
			Name: ptr(" Env noop "), Type: ptr("test"), Labels: &map[string]string{},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !got.UpdatedAt.Equal(e.UpdatedAt) {
			t.Fatalf("updated_at moved from %s to %s", e.UpdatedAt, got.UpdatedAt)
		}
		f.none(t, mark)
	})

	t.Run("a slug is unique", func(t *testing.T) {
		f.environment(t, "dup")
		mark := f.mark(t)
		_, err := f.svc.CreateEnvironment(ctx, operator, inventory.NewEnvironment{Name: "Another", Slug: "dup", Type: "staging"})
		wantConflict(t, err, `slug "dup"`)
		f.none(t, mark)
	})

	t.Run("an invalid environment is refused before it is written", func(t *testing.T) {
		mark := f.mark(t)
		for _, in := range []inventory.NewEnvironment{
			{Name: "Bad slug", Slug: "Prod", Type: "production"},
			{Name: "Bad type", Slug: "bad-type", Type: "prod"},
			{Name: "Bad status", Slug: "bad-status", Type: "test", Status: "archived"},
			{Name: "", Slug: "no-name", Type: "test"},
		} {
			var invalid *inventory.InvalidError
			if _, err := f.svc.CreateEnvironment(ctx, operator, in); !errors.As(err, &invalid) {
				t.Errorf("create %+v = %v, want an InvalidError", in, err)
			}
		}
		f.none(t, mark)
	})

	t.Run("unknown IDs", func(t *testing.T) {
		id := uuid.New()
		if _, err := f.svc.GetEnvironment(ctx, id); !errors.Is(err, inventory.ErrNotFound) {
			t.Errorf("get = %v", err)
		}
		if _, err := f.svc.UpdateEnvironment(ctx, operator, id, inventory.EnvironmentChange{Name: ptr("x")}); !errors.Is(err, inventory.ErrNotFound) {
			t.Errorf("update = %v", err)
		}
		if err := f.svc.DeleteEnvironment(ctx, operator, id); !errors.Is(err, inventory.ErrNotFound) {
			t.Errorf("delete = %v", err)
		}
	})

	t.Run("the list is by name and filters by status", func(t *testing.T) {
		f.environment(t, "list-b")
		a := f.environment(t, "list-a")
		if _, err := f.svc.UpdateEnvironment(ctx, operator, a.ID, inventory.EnvironmentChange{Status: ptr(inventory.StatusDisabled)}); err != nil {
			t.Fatal(err)
		}
		all, err := f.svc.ListEnvironments(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		for i := 1; i < len(all); i++ {
			if all[i-1].Name > all[i].Name {
				t.Fatalf("list not by name: %q before %q", all[i-1].Name, all[i].Name)
			}
		}
		disabled, err := f.svc.ListEnvironments(ctx, inventory.StatusDisabled)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range disabled {
			if e.Status != inventory.StatusDisabled {
				t.Fatalf("disabled list holds %+v", e)
			}
		}
		if !containsSlug(disabled, "list-a") || containsSlug(disabled, "list-b") {
			t.Fatalf("disabled list %+v: want list-a and not list-b", disabled)
		}
		var invalid *inventory.InvalidError
		if _, err := f.svc.ListEnvironments(ctx, "archived"); !errors.As(err, &invalid) {
			t.Fatalf("list by an unknown status = %v, want an InvalidError", err)
		}
	})

	t.Run("a change whose audit record cannot be written is rolled back", func(t *testing.T) {
		// No request ID: audit.Write refuses the record, so the change
		// must not commit without it (ADR 0016).
		_, err := f.svc.CreateEnvironment(ctx, inventory.Caller{Actor: operator.Actor}, inventory.NewEnvironment{Name: "Unaudited", Slug: "unaudited", Type: "test"})
		if err == nil {
			t.Fatal("created without an audit record")
		}
		var n int
		if err := f.pool.QueryRow(ctx, "select count(*) from environments where slug = 'unaudited'").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatal("the environment was written though its audit record was not")
		}
	})
}

func containsSlug(envs []inventory.Environment, slug string) bool {
	for _, e := range envs {
		if e.Slug == slug {
			return true
		}
	}
	return false
}

// Deletes follow the foreign keys: refused, with a 409's detail naming what
// still refers to the entity, while anything does; nothing is deleted or
// audited then.
func TestDeleteIsRefusedWhileReferenced(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	t.Run("an environment with applications", func(t *testing.T) {
		e := f.environment(t, "has-apps")
		f.application(t, e.ID, "Claims")
		mark := f.mark(t)
		wantConflict(t, f.svc.DeleteEnvironment(ctx, operator, e.ID), "still has applications")
		f.none(t, mark)
		if _, err := f.svc.GetEnvironment(ctx, e.ID); err != nil {
			t.Fatalf("the environment is gone: %v", err)
		}
	})

	t.Run("an environment with database targets", func(t *testing.T) {
		e := f.environment(t, "has-targets")
		if _, err := f.pool.Exec(ctx, `insert into database_targets (environment_id, engine, name, host, port, credential_ref)
			values ($1, 'sqlserver', 'sql-prod-01', 'sql-prod-01', 1433, 'env/sql')`, e.ID); err != nil {
			t.Fatal(err)
		}
		wantConflict(t, f.svc.DeleteEnvironment(ctx, operator, e.ID), "still has database targets")
	})

	t.Run("an application with components", func(t *testing.T) {
		e := f.environment(t, "app-has-components")
		a := f.application(t, e.ID, "Claims")
		if _, err := f.svc.CreateComponent(ctx, operator, inventory.NewComponent{ApplicationID: a.ID, Name: "ClaimsWeb", ComponentType: "module"}); err != nil {
			t.Fatal(err)
		}
		mark := f.mark(t)
		wantConflict(t, f.svc.DeleteApplication(ctx, operator, a.ID), "still has components")
		f.none(t, mark)
	})

	t.Run("an application with telemetry sources", func(t *testing.T) {
		e := f.environment(t, "app-has-sources")
		a := f.application(t, e.ID, "Claims")
		if _, err := f.pool.Exec(ctx, `insert into telemetry_sources (application_id, source_type, ingest_mode, config)
			values ($1, 'outsystems', 'agentless', '{"application_name":"Claims"}')`, a.ID); err != nil {
			t.Fatal(err)
		}
		wantConflict(t, f.svc.DeleteApplication(ctx, operator, a.ID), "still has telemetry sources")
	})

	t.Run("an unreferenced application, then its environment", func(t *testing.T) {
		e := f.environment(t, "deletable")
		a := f.application(t, e.ID, "Claims")
		mark := f.mark(t)
		if err := f.svc.DeleteApplication(ctx, operator, a.ID); err != nil {
			t.Fatal(err)
		}
		r := f.one(t, mark, audit.ActionDelete, audit.EntityApplication, a.ID)
		if decode[inventory.Application](t, r.Before).EnvironmentID != e.ID {
			t.Fatalf("delete record before %s: want the application with its environment", r.Before)
		}
		if err := f.svc.DeleteEnvironment(ctx, operator, e.ID); err != nil {
			t.Fatal(err)
		}
	})
}

func TestApplications(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	prod := f.environment(t, "prod")
	staging := f.environment(t, "staging")

	t.Run("created in its environment, with defaults, and audited", func(t *testing.T) {
		mark := f.mark(t)
		a, err := f.svc.CreateApplication(ctx, operator, inventory.NewApplication{
			EnvironmentID: prod.ID, Name: "Claims Portal", Platform: "outsystems_traditional", OwnerTeam: "Claims",
		})
		if err != nil {
			t.Fatal(err)
		}
		if a.EnvironmentID != prod.ID || a.Criticality != "medium" || a.Status != inventory.StatusActive {
			t.Fatalf("created %+v: want prod, medium, active", a)
		}
		r := f.one(t, mark, audit.ActionCreate, audit.EntityApplication, a.ID)
		if decode[inventory.Application](t, r.After).EnvironmentID != prod.ID {
			t.Fatalf("create record after %s: want the environment", r.After)
		}
	})

	t.Run("a name is unique within an environment, not across them", func(t *testing.T) {
		f.application(t, prod.ID, "Billing")
		mark := f.mark(t)
		_, err := f.svc.CreateApplication(ctx, operator, inventory.NewApplication{EnvironmentID: prod.ID, Name: "Billing", Platform: "dotnet"})
		wantConflict(t, err, `application named "Billing"`)
		f.none(t, mark)
		if other := f.application(t, staging.ID, "Billing"); other.EnvironmentID != staging.ID {
			t.Fatalf("the staging Billing is in %s", other.EnvironmentID)
		}
	})

	t.Run("an environment that does not exist", func(t *testing.T) {
		mark := f.mark(t)
		_, err := f.svc.CreateApplication(ctx, operator, inventory.NewApplication{EnvironmentID: uuid.New(), Name: "Orphan", Platform: "dotnet"})
		wantOwner(t, err)
		f.none(t, mark)
	})

	t.Run("a change keeps the environment and refuses a taken name", func(t *testing.T) {
		a := f.application(t, prod.ID, "Renamed")
		f.application(t, prod.ID, "Taken")
		mark := f.mark(t)
		changed, err := f.svc.UpdateApplication(ctx, operator, a.ID, inventory.ApplicationChange{Criticality: ptr("critical"), OwnerTeam: ptr("SRE")})
		if err != nil {
			t.Fatal(err)
		}
		if changed.EnvironmentID != prod.ID || changed.Criticality != "critical" || changed.OwnerTeam != "SRE" || changed.Name != "Renamed" {
			t.Fatalf("changed %+v", changed)
		}
		r := f.one(t, mark, audit.ActionUpdate, audit.EntityApplication, a.ID)
		if decode[inventory.Application](t, r.Before).Criticality != "medium" {
			t.Fatalf("update record before %s: want medium", r.Before)
		}
		mark = f.mark(t)
		_, err = f.svc.UpdateApplication(ctx, operator, a.ID, inventory.ApplicationChange{Name: ptr("Taken")})
		wantConflict(t, err, `application named "Taken"`)
		f.none(t, mark)
	})

	t.Run("a change that changes nothing audits nothing", func(t *testing.T) {
		a := f.application(t, prod.ID, "Unchanged")
		mark := f.mark(t)
		if _, err := f.svc.UpdateApplication(ctx, operator, a.ID, inventory.ApplicationChange{Name: ptr("Unchanged"), Status: ptr("active")}); err != nil {
			t.Fatal(err)
		}
		f.none(t, mark)
	})
}

// Pages are by name, then ID, with a keyset cursor: every application
// comes exactly once, in order, and one added behind the cursor while a
// client pages does not shift the pages after it.
func TestApplicationPages(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	prod := f.environment(t, "prod")
	other := f.environment(t, "other")
	var want []string
	for i := range 7 {
		name := fmt.Sprintf("App %02d", i)
		f.application(t, prod.ID, name)
		want = append(want, name)
	}
	f.application(t, other.ID, "App 00") // same name, another environment

	filter := inventory.ApplicationFilter{EnvironmentID: prod.ID}
	var got []string
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("more pages than 7 applications in pages of 3 make")
		}
		p, err := f.svc.ListApplications(ctx, filter, inventory.Page{Limit: 3, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range p.Items {
			if a.EnvironmentID != prod.ID {
				t.Fatalf("the prod filter returned %+v", a)
			}
			got = append(got, a.Name)
		}
		if pages == 0 {
			// Added before the cursor: it must not push App 03 onto a
			// later page or repeat App 02.
			f.application(t, prod.ID, "App 00a")
		}
		if p.NextCursor == "" {
			if len(p.Items) == 0 {
				t.Fatal("an empty last page: the page before should have had no cursor")
			}
			break
		}
		cursor = p.NextCursor
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("pages = %v, want %v", got, want)
	}

	all, err := f.svc.ListApplications(ctx, inventory.ApplicationFilter{}, inventory.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Items) != 9 || all.NextCursor != "" {
		t.Fatalf("unfiltered list = %d items, cursor %q; want all 9 on one default page", len(all.Items), all.NextCursor)
	}
	disabled, err := f.svc.ListApplications(ctx, inventory.ApplicationFilter{Status: inventory.StatusDisabled}, inventory.Page{})
	if err != nil || len(disabled.Items) != 0 {
		t.Fatalf("disabled = %+v, %v; want none", disabled, err)
	}
}

func TestComponents(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	prod := f.environment(t, "prod")
	claims := f.application(t, prod.ID, "Claims")

	t.Run("its environment is its application's", func(t *testing.T) {
		mark := f.mark(t)
		c, err := f.svc.CreateComponent(ctx, operator, inventory.NewComponent{
			ApplicationID: claims.ID, Name: "ClaimsWeb", ComponentType: "module", RuntimeName: "ClaimsWeb",
			Labels: map[string]string{"layer": "ui"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if c.ApplicationID != claims.ID || c.EnvironmentID != prod.ID {
			t.Fatalf("created %+v: want Claims in prod", c)
		}
		r := f.one(t, mark, audit.ActionCreate, audit.EntityComponent, c.ID)
		if decode[inventory.Component](t, r.After).EnvironmentID != prod.ID {
			t.Fatalf("create record after %s: want the derived environment", r.After)
		}
		got, err := f.svc.GetComponent(ctx, c.ID)
		if err != nil || got.EnvironmentID != prod.ID || got.Labels["layer"] != "ui" {
			t.Fatalf("get = %+v, %v", got, err)
		}
		// The table has no environment column: the only path is the
		// application's.
		var columns int
		if err := f.pool.QueryRow(ctx, `select count(*) from information_schema.columns
			where table_name = 'application_components' and column_name = 'environment_id'`).Scan(&columns); err != nil {
			t.Fatal(err)
		}
		if columns != 0 {
			t.Fatal("application_components has an environment_id column: a second ownership path")
		}
	})

	t.Run("name and type are unique within an application", func(t *testing.T) {
		f.mustComponent(t, claims.ID, "Login", "screen_flow")
		_, err := f.svc.CreateComponent(ctx, operator, inventory.NewComponent{ApplicationID: claims.ID, Name: "Login", ComponentType: "screen_flow"})
		wantConflict(t, err, `screen_flow component named "Login"`)
		f.mustComponent(t, claims.ID, "Login", "module")
	})

	t.Run("an application that does not exist", func(t *testing.T) {
		mark := f.mark(t)
		_, err := f.svc.CreateComponent(ctx, operator, inventory.NewComponent{ApplicationID: uuid.New(), Name: "Orphan", ComponentType: "module"})
		wantOwner(t, err)
		f.none(t, mark)
	})

	t.Run("change, page and delete", func(t *testing.T) {
		billing := f.application(t, prod.ID, "Billing")
		c := f.mustComponent(t, billing.ID, "Worker", "worker")
		f.mustComponent(t, billing.ID, "Api", "module")
		f.mustComponent(t, billing.ID, "Batch", "worker")

		mark := f.mark(t)
		changed, err := f.svc.UpdateComponent(ctx, operator, c.ID, inventory.ComponentChange{RuntimeName: ptr("billing-worker"), Labels: &map[string]string{"queue": "jobs"}})
		if err != nil {
			t.Fatal(err)
		}
		if changed.RuntimeName != "billing-worker" || changed.ApplicationID != billing.ID || changed.EnvironmentID != prod.ID {
			t.Fatalf("changed %+v", changed)
		}
		f.one(t, mark, audit.ActionUpdate, audit.EntityComponent, c.ID)

		_, err = f.svc.UpdateComponent(ctx, operator, c.ID, inventory.ComponentChange{Name: ptr("Batch")})
		wantConflict(t, err, `worker component named "Batch"`)

		first, err := f.svc.ListComponents(ctx, inventory.ComponentFilter{ApplicationID: billing.ID}, inventory.Page{Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		rest, err := f.svc.ListComponents(ctx, inventory.ComponentFilter{ApplicationID: billing.ID}, inventory.Page{Limit: 2, Cursor: first.NextCursor})
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, item := range append(first.Items, rest.Items...) {
			if item.EnvironmentID != prod.ID {
				t.Fatalf("listed %+v without its environment", item)
			}
			names = append(names, item.Name)
		}
		if strings.Join(names, ",") != "Api,Batch,Worker" || rest.NextCursor != "" {
			t.Fatalf("pages = %v (next %q), want Api,Batch,Worker", names, rest.NextCursor)
		}

		mark = f.mark(t)
		if err := f.svc.DeleteComponent(ctx, operator, c.ID); err != nil {
			t.Fatal(err)
		}
		r := f.one(t, mark, audit.ActionDelete, audit.EntityComponent, c.ID)
		if decode[inventory.Component](t, r.Before).RuntimeName != "billing-worker" {
			t.Fatalf("delete record before %s", r.Before)
		}
		if err := f.svc.DeleteComponent(ctx, operator, c.ID); !errors.Is(err, inventory.ErrNotFound) {
			t.Fatalf("second delete = %v, want ErrNotFound", err)
		}
	})
}

func (f fixture) mustComponent(t *testing.T, app uuid.UUID, name, kind string) inventory.Component {
	t.Helper()
	c, err := f.svc.CreateComponent(context.Background(), operator, inventory.NewComponent{ApplicationID: app, Name: name, ComponentType: kind})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
