//go:build integration

package inventory_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"heartbeat/apps/api/internal/audit"
	"heartbeat/apps/api/internal/inventory"
)

func (f fixture) target(t *testing.T, env uuid.UUID, name string) inventory.DatabaseTarget {
	t.Helper()
	tg, err := f.svc.CreateDatabaseTarget(context.Background(), operator, inventory.NewDatabaseTarget{
		EnvironmentID: env, Name: name, Host: name + ".db.internal", CredentialRef: "env/" + name,
	})
	if err != nil {
		t.Fatal(err)
	}
	return tg
}

func TestDatabaseTargets(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	t.Run("create, read and audit", func(t *testing.T) {
		e := f.environment(t, "targets-create")
		mark := f.mark(t)
		tg, err := f.svc.CreateDatabaseTarget(ctx, operator, inventory.NewDatabaseTarget{
			EnvironmentID: e.ID, Name: "finance-prod", Host: "SQL01.Finance.Internal", DatabaseName: "Finance",
			CredentialRef: "env/finance-prod", Labels: map[string]string{"tier": "gold"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if tg.Engine != "sqlserver" || tg.Port != 1433 || tg.Host != "sql01.finance.internal" || tg.Status != inventory.StatusActive || tg.EnvironmentID != e.ID {
			t.Fatalf("CreateDatabaseTarget() = %+v", tg)
		}
		r := f.one(t, mark, audit.ActionCreate, audit.EntityDatabaseTarget, tg.ID)
		if !null(r.Before) || decode[inventory.DatabaseTarget](t, r.After).CredentialRef != "env/finance-prod" {
			t.Fatalf("create record before %s after %s", r.Before, r.After)
		}
		got, err := f.svc.GetDatabaseTarget(ctx, tg.ID)
		if err != nil || got.Name != tg.Name || got.DatabaseName != "Finance" || got.Labels["tier"] != "gold" || !got.CreatedAt.Equal(tg.CreatedAt) {
			t.Fatalf("GetDatabaseTarget() = %+v, %v; want what was created", got, err)
		}
		if _, err := f.svc.GetDatabaseTarget(ctx, uuid.New()); !errors.Is(err, inventory.ErrNotFound) {
			t.Fatalf("GetDatabaseTarget(unknown) = %v, want ErrNotFound", err)
		}
	})

	t.Run("names and databases are unique in an environment", func(t *testing.T) {
		e := f.environment(t, "targets-unique")
		other := f.environment(t, "targets-unique-other")
		f.target(t, e.ID, "orders")
		mark := f.mark(t)
		_, err := f.svc.CreateDatabaseTarget(ctx, operator, inventory.NewDatabaseTarget{EnvironmentID: e.ID, Name: "orders", Host: "elsewhere", CredentialRef: "env/x"})
		wantConflict(t, err, `a database target named "orders"`)
		_, err = f.svc.CreateDatabaseTarget(ctx, operator, inventory.NewDatabaseTarget{EnvironmentID: e.ID, Name: "orders-again", Host: "ORDERS.db.internal", CredentialRef: "env/x"})
		wantConflict(t, err, "a sqlserver target for orders.db.internal, port 1433")
		f.none(t, mark)
		// Another environment may have both.
		if _, err := f.svc.CreateDatabaseTarget(ctx, operator, inventory.NewDatabaseTarget{EnvironmentID: other.ID, Name: "orders", Host: "orders.db.internal", CredentialRef: "env/orders"}); err != nil {
			t.Fatalf("the same name and database in another environment: %v", err)
		}
		// Another database on the same server is another target.
		if _, err := f.svc.CreateDatabaseTarget(ctx, operator, inventory.NewDatabaseTarget{EnvironmentID: e.ID, Name: "orders-archive", Host: "orders.db.internal", DatabaseName: "Archive", CredentialRef: "env/orders"}); err != nil {
			t.Fatalf("another database on the same server: %v", err)
		}
	})

	t.Run("the environment must exist", func(t *testing.T) {
		mark := f.mark(t)
		_, err := f.svc.CreateDatabaseTarget(ctx, operator, inventory.NewDatabaseTarget{EnvironmentID: uuid.New(), Name: "nowhere", Host: "nowhere", CredentialRef: "env/x"})
		wantOwner(t, err)
		f.none(t, mark)
	})

	t.Run("a credential is never stored", func(t *testing.T) {
		e := f.environment(t, "targets-credentials")
		mark := f.mark(t)
		for _, ref := range []string{"sa:Passw0rd!", "env/sa:Passw0rd!", "https://sa:Passw0rd!@vault"} {
			_, err := f.svc.CreateDatabaseTarget(ctx, operator, inventory.NewDatabaseTarget{EnvironmentID: e.ID, Name: "creds", Host: "creds", CredentialRef: ref})
			var invalid *inventory.InvalidError
			if !errors.As(err, &invalid) || invalid.Field != "credential_ref" {
				t.Errorf("credential_ref %q: %v, want an InvalidError", ref, err)
			}
		}
		f.none(t, mark)
		var n int
		if err := f.pool.QueryRow(ctx, "select count(*) from database_targets where environment_id = $1", e.ID).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%d targets stored (%v), want none", n, err)
		}
	})

	t.Run("update", func(t *testing.T) {
		e := f.environment(t, "targets-update")
		tg := f.target(t, e.ID, "claims")
		mark := f.mark(t)
		after, err := f.svc.UpdateDatabaseTarget(ctx, operator, tg.ID, inventory.DatabaseTargetChange{
			Host: ptr("claims-02.db.internal"), Port: ptr(14330), CredentialRef: ptr("kv/claims"), Status: ptr(inventory.StatusDisabled),
		})
		if err != nil {
			t.Fatal(err)
		}
		if after.Host != "claims-02.db.internal" || after.Port != 14330 || after.CredentialRef != "kv/claims" || after.Status != inventory.StatusDisabled || after.Name != "claims" || !after.UpdatedAt.After(tg.UpdatedAt) {
			t.Fatalf("UpdateDatabaseTarget() = %+v", after)
		}
		r := f.one(t, mark, audit.ActionUpdate, audit.EntityDatabaseTarget, tg.ID)
		if decode[inventory.DatabaseTarget](t, r.Before).Host != tg.Host || decode[inventory.DatabaseTarget](t, r.After).Host != after.Host {
			t.Fatalf("update record before %s after %s", r.Before, r.After)
		}

		// A change that changes nothing writes and records nothing.
		mark = f.mark(t)
		same, err := f.svc.UpdateDatabaseTarget(ctx, operator, tg.ID, inventory.DatabaseTargetChange{Host: ptr("CLAIMS-02.db.internal"), Port: ptr(14330)})
		if err != nil || !same.UpdatedAt.Equal(after.UpdatedAt) {
			t.Fatalf("a change to the same values: %+v, %v; want updated_at kept", same, err)
		}
		f.none(t, mark)

		// Moving it onto another target's database is refused.
		f.target(t, e.ID, "ledger")
		_, err = f.svc.UpdateDatabaseTarget(ctx, operator, tg.ID, inventory.DatabaseTargetChange{Host: ptr("ledger.db.internal"), Port: ptr(1433)})
		wantConflict(t, err, "a sqlserver target for ledger.db.internal")
		if _, err := f.svc.UpdateDatabaseTarget(ctx, operator, uuid.New(), inventory.DatabaseTargetChange{Status: ptr(inventory.StatusActive)}); !errors.Is(err, inventory.ErrNotFound) {
			t.Fatalf("UpdateDatabaseTarget(unknown) = %v, want ErrNotFound", err)
		}
	})

	t.Run("delete, and refused while it has assignments", func(t *testing.T) {
		e := f.environment(t, "targets-delete")
		assigned := f.target(t, e.ID, "assigned")
		var definition uuid.UUID
		if err := f.pool.QueryRow(ctx, "select id from probe_definitions where builtin and name = 'waits'").Scan(&definition); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(ctx, "insert into probe_assignments (database_target_id, probe_definition_id, interval_seconds) values ($1, $2, 30)", assigned.ID, definition); err != nil {
			t.Fatal(err)
		}
		mark := f.mark(t)
		wantConflict(t, f.svc.DeleteDatabaseTarget(ctx, operator, assigned.ID), "has 1 probe assignments")
		f.none(t, mark)
		var n int
		if err := f.pool.QueryRow(ctx, "select count(*) from probe_assignments where database_target_id = $1", assigned.ID).Scan(&n); err != nil || n != 1 {
			t.Fatalf("%d assignments left (%v), want the one", n, err)
		}

		free := f.target(t, e.ID, "free")
		mark = f.mark(t)
		if err := f.svc.DeleteDatabaseTarget(ctx, operator, free.ID); err != nil {
			t.Fatal(err)
		}
		r := f.one(t, mark, audit.ActionDelete, audit.EntityDatabaseTarget, free.ID)
		if !null(r.After) || decode[inventory.DatabaseTarget](t, r.Before).Name != "free" {
			t.Fatalf("delete record before %s after %s", r.Before, r.After)
		}
		if err := f.svc.DeleteDatabaseTarget(ctx, operator, free.ID); !errors.Is(err, inventory.ErrNotFound) {
			t.Fatalf("a second delete = %v, want ErrNotFound", err)
		}
	})

	t.Run("a change rolls back without its record", func(t *testing.T) {
		e := f.environment(t, "targets-rollback")
		noActor := inventory.Caller{RequestID: "req"}
		if _, err := f.svc.CreateDatabaseTarget(ctx, noActor, inventory.NewDatabaseTarget{EnvironmentID: e.ID, Name: "rollback", Host: "rollback", CredentialRef: "env/x"}); err == nil {
			t.Fatal("CreateDatabaseTarget() with an actor the record refuses succeeded")
		}
		var n int
		if err := f.pool.QueryRow(ctx, "select count(*) from database_targets where environment_id = $1", e.ID).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%d targets stored (%v), want none", n, err)
		}
	})

	t.Run("pages and filters", func(t *testing.T) {
		e := f.environment(t, "targets-pages")
		for i := range 5 {
			f.target(t, e.ID, fmt.Sprintf("page-%d", i))
		}
		if _, err := f.svc.UpdateDatabaseTarget(ctx, operator, f.target(t, e.ID, "page-off").ID, inventory.DatabaseTargetChange{Status: ptr(inventory.StatusDisabled)}); err != nil {
			t.Fatal(err)
		}
		var names []string
		cursor := ""
		for range 10 {
			page, err := f.svc.ListDatabaseTargets(ctx, inventory.DatabaseTargetFilter{EnvironmentID: e.ID, Status: inventory.StatusActive}, inventory.Page{Limit: 2, Cursor: cursor})
			if err != nil {
				t.Fatal(err)
			}
			for _, tg := range page.Items {
				names = append(names, tg.Name)
			}
			if cursor = page.NextCursor; cursor == "" {
				break
			}
		}
		if fmt.Sprint(names) != "[page-0 page-1 page-2 page-3 page-4]" {
			t.Fatalf("active targets, two a page: %v", names)
		}
		if _, err := f.svc.ListDatabaseTargets(ctx, inventory.DatabaseTargetFilter{Status: "paused"}, inventory.Page{}); err == nil {
			t.Fatal("a list with an unknown status succeeded")
		}
	})
}
