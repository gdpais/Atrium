//go:build integration

package inventory_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"atrium/apps/api/internal/audit"
	"atrium/apps/api/internal/collectors"
	"atrium/apps/api/internal/inventory"
	"atrium/apps/api/internal/probedefs"
	"atrium/apps/api/internal/testenv"
)

var admin = inventory.Caller{
	Actor:     audit.UserActor("5c1d7a3e-2b4f-4e8a-9c6d-1f0e3b2a4d5c", "admin@example.com"),
	RequestID: "req-assignments-test",
}

// users makes the operator and the admin the tests act as: an assignment
// records who created and approved it.
func (f fixture) users(t *testing.T) {
	t.Helper()
	for _, c := range []inventory.Caller{operator, admin} {
		if _, err := f.pool.Exec(context.Background(),
			"insert into users (id, email, display_name) values ($1, $2, $2) on conflict do nothing", c.Actor.UserID, c.Actor.Email); err != nil {
			t.Fatal(err)
		}
	}
}

// builtin is the ID of the seeded built-in definition name.
func (f fixture) builtin(t *testing.T, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(context.Background(), "select id from probe_definitions where builtin and name = $1", name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// custom creates a custom definition that overrides waits.
func (f fixture) custom(t *testing.T, name string, timeoutMS int) probedefs.Definition {
	t.Helper()
	def, err := probedefs.NewService(f.pool).Create(context.Background(), probedefs.NewDefinition{
		Engine: "sqlserver", Name: name, Overrides: "waits",
		Content: probedefs.Content{QueryTemplate: "SELECT wait_type, wait_time_ms FROM sys.dm_os_wait_stats", TimeoutMS: timeoutMS},
	}, admin.Actor, "req-custom")
	if err != nil {
		t.Fatal(err)
	}
	return def
}

func (f fixture) assign(t *testing.T, target, def uuid.UUID, interval int) inventory.ProbeAssignment {
	t.Helper()
	a, err := f.svc.CreateProbeAssignment(context.Background(), operator, inventory.NewProbeAssignment{
		DatabaseTargetID: target, ProbeDefinitionID: def, IntervalSeconds: interval,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func wantInvalid(t *testing.T, err error, field string) {
	t.Helper()
	var invalid *inventory.InvalidError
	if !errors.As(err, &invalid) || invalid.Field != field {
		t.Fatalf("err = %v, want an InvalidError for %s", err, field)
	}
}

func TestProbeAssignments(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.users(t)
	ctx := context.Background()
	waits := f.builtin(t, "waits")
	blocking := f.builtin(t, "blocking")

	t.Run("create a draft, read and audit it", func(t *testing.T) {
		e := f.environment(t, "assign-create")
		tg := f.target(t, e.ID, "orders")
		mark := f.mark(t)
		a, err := f.svc.CreateProbeAssignment(ctx, operator, inventory.NewProbeAssignment{DatabaseTargetID: tg.ID, ProbeDefinitionID: waits})
		if err != nil {
			t.Fatal(err)
		}
		// The interval defaults to the definition's.
		if a.State != inventory.StateDraft || a.IntervalSeconds != 30 || !a.Enabled || a.ApprovedBy != nil ||
			a.CreatedBy == nil || a.CreatedBy.String() != operator.Actor.UserID || a.CollectorID != "api-"+tg.ID.String()+"-30s" {
			t.Fatalf("CreateProbeAssignment() = %+v", a)
		}
		r := f.one(t, mark, audit.ActionCreate, audit.EntityProbeAssignment, a.ID)
		if !null(r.Before) || decode[inventory.ProbeAssignment](t, r.After).State != inventory.StateDraft {
			t.Fatalf("create record before %s after %s", r.Before, r.After)
		}
		got, err := f.svc.GetProbeAssignment(ctx, a.ID)
		if err != nil || got.ID != a.ID || got.ProbeDefinitionID != waits {
			t.Fatalf("GetProbeAssignment() = %+v, %v", got, err)
		}
		page, err := f.svc.ListProbeAssignments(ctx, inventory.ProbeAssignmentFilter{DatabaseTargetID: tg.ID, State: inventory.StateDraft}, inventory.Page{})
		if err != nil || len(page.Items) != 1 || page.Items[0].ID != a.ID {
			t.Fatalf("ListProbeAssignments() = %+v, %v", page, err)
		}
		if _, err := f.svc.GetProbeAssignment(ctx, uuid.New()); !errors.Is(err, inventory.ErrNotFound) {
			t.Fatalf("GetProbeAssignment(unknown) = %v, want ErrNotFound", err)
		}
		// The target cannot be deleted while it has assignments.
		wantConflict(t, f.svc.DeleteDatabaseTarget(ctx, operator, tg.ID), "probe assignments")
	})

	t.Run("pages", func(t *testing.T) {
		e := f.environment(t, "assign-pages")
		tg := f.target(t, e.ID, "paged")
		var want []uuid.UUID
		for _, name := range []string{"waits", "blocking", "sessions"} {
			want = append(want, f.assign(t, tg.ID, f.builtin(t, name), 30).ID)
		}
		var got []uuid.UUID
		p := inventory.Page{Limit: 2}
		for {
			page, err := f.svc.ListProbeAssignments(ctx, inventory.ProbeAssignmentFilter{DatabaseTargetID: tg.ID}, p)
			if err != nil {
				t.Fatal(err)
			}
			for _, a := range page.Items {
				got = append(got, a.ID)
			}
			if page.NextCursor == "" {
				break
			}
			p.Cursor = page.NextCursor
		}
		if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Fatalf("pages = %v, want %v", got, want)
		}
		if _, err := f.svc.ListProbeAssignments(ctx, inventory.ProbeAssignmentFilter{State: "pending"}, inventory.Page{}); err == nil {
			t.Fatal("an unknown state filter passed")
		}
	})

	t.Run("probe policy", func(t *testing.T) {
		e := f.environment(t, "assign-policy")
		tg := f.target(t, e.ID, "policy")
		slow := f.custom(t, "custom_waits_slow", 20000)
		mark := f.mark(t)
		for _, tc := range []struct {
			in    inventory.NewProbeAssignment
			field string
		}{
			{inventory.NewProbeAssignment{ProbeDefinitionID: waits, IntervalSeconds: 4}, "interval_seconds"},
			{inventory.NewProbeAssignment{ProbeDefinitionID: waits, IntervalSeconds: 3601}, "interval_seconds"},
			{inventory.NewProbeAssignment{ProbeDefinitionID: waits, TimeoutMS: 25001}, "timeout_ms"},
			{inventory.NewProbeAssignment{ProbeDefinitionID: waits, IntervalSeconds: 10, TimeoutMS: 10001}, "timeout_ms"},
			{inventory.NewProbeAssignment{ProbeDefinitionID: waits, MaxRows: 100001}, "max_rows"},
			{inventory.NewProbeAssignment{ProbeDefinitionID: waits, MaxRows: -1}, "max_rows"},
			// The definition's timeout counts when the assignment sets none.
			{inventory.NewProbeAssignment{ProbeDefinitionID: slow.ID, IntervalSeconds: 15}, "timeout_ms"},
		} {
			tc.in.DatabaseTargetID = tg.ID
			_, err := f.svc.CreateProbeAssignment(ctx, operator, tc.in)
			wantInvalid(t, err, tc.field)
		}
		f.none(t, mark)
		a, err := f.svc.CreateProbeAssignment(ctx, operator, inventory.NewProbeAssignment{
			DatabaseTargetID: tg.ID, ProbeDefinitionID: slow.ID, IntervalSeconds: 60, MaxRows: 500,
		})
		if err != nil || a.MaxRows != 500 || a.TimeoutMS != 0 {
			t.Fatalf("a slow definition at 60s = %+v, %v", a, err)
		}
	})

	t.Run("a target runs each probe once", func(t *testing.T) {
		e := f.environment(t, "assign-once")
		tg := f.target(t, e.ID, "once")
		first := f.assign(t, tg.ID, waits, 30)
		lean := f.custom(t, "custom_waits_lean", 0)
		mark := f.mark(t)
		_, err := f.svc.CreateProbeAssignment(ctx, operator, inventory.NewProbeAssignment{DatabaseTargetID: tg.ID, ProbeDefinitionID: waits})
		wantConflict(t, err, "an assignment of waits already, "+first.ID.String())
		_, err = f.svc.CreateProbeAssignment(ctx, operator, inventory.NewProbeAssignment{DatabaseTargetID: tg.ID, ProbeDefinitionID: lean.ID})
		wantConflict(t, err, "runs probe waits already")
		f.none(t, mark)
		// Another target may.
		other := f.target(t, e.ID, "once-other")
		f.assign(t, other.ID, lean.ID, 30)
	})

	t.Run("owners and definitions", func(t *testing.T) {
		e := f.environment(t, "assign-owners")
		tg := f.target(t, e.ID, "owners")
		_, err := f.svc.CreateProbeAssignment(ctx, operator, inventory.NewProbeAssignment{DatabaseTargetID: uuid.New(), ProbeDefinitionID: waits})
		wantOwner(t, err)
		_, err = f.svc.CreateProbeAssignment(ctx, operator, inventory.NewProbeAssignment{DatabaseTargetID: tg.ID, ProbeDefinitionID: uuid.New()})
		wantOwner(t, err)
		off := f.custom(t, "custom_waits_off", 0)
		if _, err := probedefs.NewService(f.pool).SetEnabled(ctx, off.ID, false, admin.Actor, "req-off"); err != nil {
			t.Fatal(err)
		}
		_, err = f.svc.CreateProbeAssignment(ctx, operator, inventory.NewProbeAssignment{DatabaseTargetID: tg.ID, ProbeDefinitionID: off.ID})
		wantConflict(t, err, "is disabled")
		// A definition disabled after the draft was made is not approved:
		// enabling it again would run it on an approval given while it was off.
		later := f.custom(t, "custom_waits_later", 0)
		draft := f.assign(t, tg.ID, later.ID, 30)
		if _, err := probedefs.NewService(f.pool).SetEnabled(ctx, later.ID, false, admin.Actor, "req-later"); err != nil {
			t.Fatal(err)
		}
		_, err = f.svc.ApproveProbeAssignment(ctx, admin, draft.ID)
		wantConflict(t, err, "is disabled")
		if got, _ := f.svc.GetProbeAssignment(ctx, draft.ID); got.State != inventory.StateDraft {
			t.Fatalf("after a refused approval: %+v", got)
		}
	})

	t.Run("a change goes back to draft", func(t *testing.T) {
		e := f.environment(t, "assign-change")
		tg := f.target(t, e.ID, "change")
		def := f.custom(t, "custom_waits_change", 0)
		a := f.assign(t, tg.ID, def.ID, 30)
		if a, err := f.svc.ApproveProbeAssignment(ctx, admin, a.ID); err != nil || a.State != inventory.StateApproved {
			t.Fatalf("ApproveProbeAssignment() = %+v, %v", a, err)
		}
		mark := f.mark(t)
		same, err := f.svc.UpdateProbeAssignment(ctx, operator, a.ID, inventory.ProbeAssignmentChange{IntervalSeconds: ptr(30)})
		if err != nil || same.State != inventory.StateApproved {
			t.Fatalf("a change that changes nothing = %+v, %v", same, err)
		}
		f.none(t, mark)
		v2, _, err := probedefs.NewService(f.pool).AddVersion(ctx, def.ID, probedefs.Content{QueryTemplate: "SELECT wait_type, wait_time_ms FROM sys.dm_os_wait_stats WHERE wait_time_ms > 0"}, admin.Actor, "req-v2")
		if err != nil {
			t.Fatal(err)
		}
		mark = f.mark(t)
		changed, err := f.svc.UpdateProbeAssignment(ctx, operator, a.ID, inventory.ProbeAssignmentChange{ProbeDefinitionID: &v2.ID, TimeoutMS: ptr(5000)})
		if err != nil || changed.State != inventory.StateDraft || changed.ApprovedBy != nil || changed.ApprovedAt != nil ||
			changed.ProbeDefinitionID != v2.ID || changed.TimeoutMS != 5000 {
			t.Fatalf("UpdateProbeAssignment() = %+v, %v", changed, err)
		}
		r := f.one(t, mark, audit.ActionUpdate, audit.EntityProbeAssignment, a.ID)
		if decode[inventory.ProbeAssignment](t, r.Before).State != inventory.StateApproved {
			t.Fatalf("update record before %s", r.Before)
		}
		// Only a version of the same definition.
		_, err = f.svc.UpdateProbeAssignment(ctx, operator, a.ID, inventory.ProbeAssignmentChange{ProbeDefinitionID: &blocking})
		wantConflict(t, err, "is not a version of custom_waits_change")
		_, err = f.svc.UpdateProbeAssignment(ctx, operator, a.ID, inventory.ProbeAssignmentChange{IntervalSeconds: ptr(2)})
		wantInvalid(t, err, "interval_seconds")
		_, err = f.svc.UpdateProbeAssignment(ctx, operator, uuid.New(), inventory.ProbeAssignmentChange{Enabled: ptr(false)})
		if !errors.Is(err, inventory.ErrNotFound) {
			t.Fatalf("UpdateProbeAssignment(unknown) = %v, want ErrNotFound", err)
		}
	})

	t.Run("a target's new connection needs a new approval", func(t *testing.T) {
		e := f.environment(t, "assign-reconnect")
		tg := f.target(t, e.ID, "reconnect")
		a := f.assign(t, tg.ID, waits, 30)
		if _, err := f.svc.ApproveProbeAssignment(ctx, admin, a.ID); err != nil {
			t.Fatal(err)
		}
		// Its status and labels do not move it: disabled, it is not
		// rendered; enabled again, it runs as approved.
		if _, err := f.svc.UpdateDatabaseTarget(ctx, operator, tg.ID, inventory.DatabaseTargetChange{Labels: &map[string]string{"tier": "gold"}}); err != nil {
			t.Fatal(err)
		}
		if got, _ := f.svc.GetProbeAssignment(ctx, a.ID); got.State != inventory.StateApproved {
			t.Fatalf("after a label change: %s, want approved", got.State)
		}
		mark := f.mark(t)
		if _, err := f.svc.UpdateDatabaseTarget(ctx, operator, tg.ID, inventory.DatabaseTargetChange{Host: ptr("elsewhere.db.internal")}); err != nil {
			t.Fatal(err)
		}
		got, err := f.svc.GetProbeAssignment(ctx, a.ID)
		if err != nil || got.State != inventory.StateDraft || got.ApprovedBy != nil {
			t.Fatalf("after a host change: %+v, %v; want a draft", got, err)
		}
		records := f.recordsSince(t, mark)
		if len(records) != 2 || records[0].EntityType != audit.EntityDatabaseTarget || records[1].EntityType != audit.EntityProbeAssignment ||
			records[1].Action != audit.ActionUpdate || records[1].Actor != operator.Actor ||
			decode[inventory.ProbeAssignment](t, records[1].Before).State != inventory.StateApproved {
			t.Fatalf("records = %+v, want the target's update and the assignment's reset", records)
		}
	})

	t.Run("approve, reject and delete", func(t *testing.T) {
		e := f.environment(t, "assign-approve")
		tg := f.target(t, e.ID, "approve")
		a := f.assign(t, tg.ID, waits, 60)
		mark := f.mark(t)
		approved, err := f.svc.ApproveProbeAssignment(ctx, admin, a.ID)
		if err != nil || approved.State != inventory.StateApproved || approved.ApprovedBy == nil || approved.ApprovedBy.String() != admin.Actor.UserID || approved.ApprovedAt == nil {
			t.Fatalf("ApproveProbeAssignment() = %+v, %v", approved, err)
		}
		f.oneBy(t, mark, admin.Actor, admin.RequestID, audit.ActionApprove, audit.EntityProbeAssignment, a.ID)
		_, err = f.svc.ApproveProbeAssignment(ctx, admin, a.ID)
		wantConflict(t, err, "is approved, not a draft")
		_, err = f.svc.RejectProbeAssignment(ctx, admin, a.ID, "no")
		wantConflict(t, err, "not a draft")

		// A target runs at one interval.
		b := f.assign(t, tg.ID, blocking, 30)
		_, err = f.svc.ApproveProbeAssignment(ctx, admin, b.ID)
		wantConflict(t, err, "run every 60s")
		mark = f.mark(t)
		rejected, err := f.svc.RejectProbeAssignment(ctx, admin, b.ID, "  run it every 60s like the rest  ")
		if err != nil || rejected.State != inventory.StateRejected || rejected.StateDetail != "run it every 60s like the rest" {
			t.Fatalf("RejectProbeAssignment() = %+v, %v", rejected, err)
		}
		f.oneBy(t, mark, admin.Actor, admin.RequestID, audit.ActionReject, audit.EntityProbeAssignment, b.ID)
		_, err = f.svc.RejectProbeAssignment(ctx, admin, b.ID, "bad\x00reason")
		wantInvalid(t, err, "reason")
		// Changed to the target's interval, it can be approved.
		if _, err := f.svc.UpdateProbeAssignment(ctx, operator, b.ID, inventory.ProbeAssignmentChange{IntervalSeconds: ptr(60)}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.svc.ApproveProbeAssignment(ctx, admin, b.ID); err != nil {
			t.Fatal(err)
		}
		// A disabled assignment is not rendered, so its interval does not
		// bind the target.
		if _, err := f.svc.UpdateProbeAssignment(ctx, operator, a.ID, inventory.ProbeAssignmentChange{Enabled: ptr(false)}); err != nil {
			t.Fatal(err)
		}

		mark = f.mark(t)
		if err := f.svc.DeleteProbeAssignment(ctx, operator, b.ID); err != nil {
			t.Fatal(err)
		}
		f.one(t, mark, audit.ActionDelete, audit.EntityProbeAssignment, b.ID)
		if err := f.svc.DeleteProbeAssignment(ctx, operator, b.ID); !errors.Is(err, inventory.ErrNotFound) {
			t.Fatalf("DeleteProbeAssignment(deleted) = %v, want ErrNotFound", err)
		}
	})
}

// TestRenderProbeAssignments renders into a database of its own, so that
// the whole file is known.
func TestRenderProbeAssignments(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.users(t)
	ctx := context.Background()

	empty, err := f.svc.RenderProbeAssignments(ctx)
	if err != nil || string(empty) != inventory.RenderHeader+"managedCollectors: []\n" {
		t.Fatalf("RenderProbeAssignments() with no approvals = %q, %v", empty, err)
	}

	e := f.environment(t, "render")
	orders, err := f.svc.CreateDatabaseTarget(ctx, operator, inventory.NewDatabaseTarget{
		EnvironmentID: e.ID, Name: "orders", Host: "orders.db.internal", DatabaseName: "Orders", CredentialRef: "env/orders",
	})
	if err != nil {
		t.Fatal(err)
	}
	lean := f.custom(t, "custom_waits_lean", 4000)
	for _, a := range []inventory.ProbeAssignment{
		f.assign(t, orders.ID, f.builtin(t, "sessions"), 15),
		f.assign(t, orders.ID, lean.ID, 15),
	} {
		if _, err := f.svc.ApproveProbeAssignment(ctx, admin, a.ID); err != nil {
			t.Fatal(err)
		}
	}
	// Neither a draft nor an approved assignment of a disabled target is
	// rendered.
	f.assign(t, orders.ID, f.builtin(t, "blocking"), 15)
	idle := f.target(t, e.ID, "idle")
	if _, err := f.svc.ApproveProbeAssignment(ctx, admin, f.assign(t, idle.ID, f.builtin(t, "cpu"), 30).ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.UpdateDatabaseTarget(ctx, operator, idle.ID, inventory.DatabaseTargetChange{Status: ptr(inventory.StatusDisabled)}); err != nil {
		t.Fatal(err)
	}

	got, err := f.svc.RenderProbeAssignments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := inventory.RenderHeader + `managedCollectors:
  - id: api-` + orders.ID.String() + `-15s
    kind: sqlserver
    enabled: true
    config:
      environment: render
      scrape_interval: 15s
      targets:
        - name: orders
          environment: render
          host: orders.db.internal
          port: 1433
          database_name: Orders
          credential_ref: env/orders
          probes:
            - name: sessions
            - name: waits
              query_template: SELECT wait_type, wait_time_ms FROM sys.dm_os_wait_stats
              timeout_ms: 4000
`
	if string(got) != want {
		t.Fatalf("RenderProbeAssignments() =\n%s\nwant\n%s", got, want)
	}
	again, err := f.svc.RenderProbeAssignments(ctx)
	if err != nil || string(again) != string(got) {
		t.Fatalf("a second render differs: %v\n%s", err, again)
	}
}

// running is a Runner that reports what it holds.
type running struct {
	collectors collectors.SQLServerCollectors
	err        error
}

func (r running) SQLServer(context.Context) (collectors.SQLServerCollectors, error) {
	return r.collectors, r.err
}

// runningThen runs then when the collector is asked, after the applied
// check listed the approved assignments: a change that races the check.
type runningThen struct {
	running
	then func()
}

func (r runningThen) SQLServer(ctx context.Context) (collectors.SQLServerCollectors, error) {
	r.then()
	return r.running.SQLServer(ctx)
}

func TestMarkAppliedProbeAssignments(t *testing.T) {
	t.Parallel()
	_, _, pool := testenv.MigratedPostgres(t)
	plain := fixture{pool: pool, svc: inventory.New(pool)}
	plain.users(t)
	ctx := context.Background()
	e := plain.environment(t, "applied")
	tg := plain.target(t, e.ID, "applied")
	a := plain.assign(t, tg.ID, plain.builtin(t, "waits"), 30)
	if _, err := plain.svc.ApproveProbeAssignment(ctx, admin, a.ID); err != nil {
		t.Fatal(err)
	}
	system := audit.SystemActor(inventory.AppliedCheckTask, "api-0")
	target := collectors.Target{Name: "applied", EnvironmentSlug: "applied", Host: "applied.db.internal", Port: 1433}
	runs := func(c collectors.SQLServerCollector) running {
		return running{collectors: collectors.SQLServerCollectors{CheckedAt: time.Now(), ByID: map[string]collectors.SQLServerCollector{a.CollectorID: c}}}
	}
	withProbes := func(probes ...collectors.Probe) collectors.Target {
		t := target
		t.Probes = probes
		return t
	}

	for name, r := range map[string]running{
		"unreachable":          {err: errors.New("the collector did not answer")},
		"no such collector":    {collectors: collectors.SQLServerCollectors{ByID: map[string]collectors.SQLServerCollector{}}},
		"disabled":             runs(collectors.SQLServerCollector{ScrapeInterval: 30 * time.Second, Targets: []collectors.Target{withProbes(collectors.Probe{Name: "waits"})}}),
		"another interval":     runs(collectors.SQLServerCollector{Enabled: true, ScrapeInterval: time.Minute, Targets: []collectors.Target{withProbes(collectors.Probe{Name: "waits"})}}),
		"without the probe":    runs(collectors.SQLServerCollector{Enabled: true, ScrapeInterval: 30 * time.Second, Targets: []collectors.Target{withProbes(collectors.Probe{Name: "cpu"})}}),
		"another row limit":    runs(collectors.SQLServerCollector{Enabled: true, ScrapeInterval: 30 * time.Second, Targets: []collectors.Target{withProbes(collectors.Probe{Name: "waits", MaxRows: 5})}}),
		"the target elsewhere": runs(collectors.SQLServerCollector{Enabled: true, ScrapeInterval: 30 * time.Second, Targets: []collectors.Target{{Name: "applied", EnvironmentSlug: "applied", Host: "other", Port: 1433, Probes: []collectors.Probe{{Name: "waits"}}}}}),
	} {
		svc := inventory.New(pool, inventory.WithRunner(r))
		n, err := svc.MarkAppliedProbeAssignments(ctx, system)
		if n != 0 || (err != nil) != (r.err != nil) {
			t.Fatalf("%s: MarkAppliedProbeAssignments() = %d, %v", name, n, err)
		}
	}
	if got, _ := plain.svc.GetProbeAssignment(ctx, a.ID); got.State != inventory.StateApproved {
		t.Fatalf("state = %s, want still approved", got.State)
	}

	svc := inventory.New(pool, inventory.WithRunner(runs(collectors.SQLServerCollector{
		Enabled: true, ScrapeInterval: 30 * time.Second, Targets: []collectors.Target{withProbes(collectors.Probe{Name: "cpu"}, collectors.Probe{Name: "waits"})},
	})))
	mark := plain.mark(t)
	if n, err := svc.MarkAppliedProbeAssignments(ctx, system); n != 1 || err != nil {
		t.Fatalf("MarkAppliedProbeAssignments() = %d, %v; want 1", n, err)
	}
	r := plain.oneBy(t, mark, system, "", audit.ActionApply, audit.EntityProbeAssignment, a.ID)
	if decode[inventory.ProbeAssignment](t, r.After).State != inventory.StateApplied {
		t.Fatalf("apply record actor %+v after %s", r.Actor, r.After)
	}
	if got, _ := plain.svc.GetProbeAssignment(ctx, a.ID); got.State != inventory.StateApplied || got.ApprovedBy == nil || *got.ApprovedBy != uuid.MustParse(admin.Actor.UserID) {
		t.Fatalf("after the check: %+v", got)
	}
	// Applied stays applied, and is rendered still.
	if n, err := svc.MarkAppliedProbeAssignments(ctx, system); n != 0 || err != nil {
		t.Fatalf("a second check = %d, %v", n, err)
	}
	if file, err := plain.svc.RenderProbeAssignments(ctx); err != nil || !strings.Contains(string(file), a.CollectorID) {
		t.Fatalf("render after applied: %v\n%s", err, file)
	}

	// An assignment changed to another version and approved again while the
	// check asked the collector is not marked: the collector was found
	// running the version listed, not the one approved now.
	re := plain.environment(t, "raced")
	rt := plain.target(t, re.ID, "raced")
	v1 := plain.custom(t, "custom_waits_raced", 0)
	raced := plain.assign(t, rt.ID, v1.ID, 30)
	if _, err := plain.svc.ApproveProbeAssignment(ctx, admin, raced.ID); err != nil {
		t.Fatal(err)
	}
	racer := runningThen{
		running: running{collectors: collectors.SQLServerCollectors{CheckedAt: time.Now(), ByID: map[string]collectors.SQLServerCollector{raced.CollectorID: {
			Enabled: true, ScrapeInterval: 30 * time.Second, Targets: []collectors.Target{{
				Name: "raced", EnvironmentSlug: "raced", Host: "raced.db.internal", Port: 1433,
				Probes: []collectors.Probe{{Name: "waits", QueryTemplate: "SELECT wait_type, wait_time_ms FROM sys.dm_os_wait_stats"}},
			}},
		}}}},
		then: func() {
			v2, _, err := probedefs.NewService(pool).AddVersion(ctx, v1.ID, probedefs.Content{QueryTemplate: "SELECT wait_type, wait_time_ms FROM sys.dm_os_wait_stats WHERE wait_time_ms > 0"}, admin.Actor, "req-raced")
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := plain.svc.UpdateProbeAssignment(ctx, operator, raced.ID, inventory.ProbeAssignmentChange{ProbeDefinitionID: &v2.ID}); err != nil {
				t.Error(err)
			}
			if _, err := plain.svc.ApproveProbeAssignment(ctx, admin, raced.ID); err != nil {
				t.Error(err)
			}
		},
	}
	if n, err := inventory.New(pool, inventory.WithRunner(racer)).MarkAppliedProbeAssignments(ctx, system); n != 0 || err != nil {
		t.Fatalf("a check raced by a change and a new approval = %d, %v; want 0", n, err)
	}
	if got, _ := plain.svc.GetProbeAssignment(ctx, raced.ID); got.State != inventory.StateApproved {
		t.Fatalf("after the raced check: %+v", got)
	}
}
