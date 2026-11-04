//go:build integration

package inventory_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"heartbeat/apps/api/internal/inventory"
)

// manage hands a row to the inventory file, as its reconciler will.
func (f fixture) manage(t *testing.T, table string, id uuid.UUID) {
	t.Helper()
	tag, err := f.pool.Exec(context.Background(), "update "+table+" set managed_by = 'config' where id = $1", id)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("manage %s %s: %v, %d rows", table, id, err, tag.RowsAffected())
	}
}

// One writer per row (ADR 0021 §4): the API's endpoints refuse to change
// or delete a row the inventory file manages, change nothing, and audit
// nothing; rows the API manages, beside them, still change.
func TestTheAPIDoesNotWriteRowsTheInventoryFileManages(t *testing.T) {
	f := newFixture(t)
	f.users(t)
	ctx := context.Background()
	env := f.environment(t, "managed")
	app := f.application(t, env.ID, "Managed shop")
	comp := f.mustComponent(t, app.ID, "Web", "web")
	src, err := f.svc.CreateSource(ctx, operator, outsystems(app.ID, inventory.IngestAgentless, "ManagedShop"))
	if err != nil {
		t.Fatal(err)
	}
	target := f.target(t, env.ID, "managed-db")
	assignment := f.assign(t, target.ID, f.builtin(t, "cpu"), 30)
	for table, id := range map[string]uuid.UUID{
		"environments": env.ID, "applications": app.ID, "application_components": comp.ID,
		"telemetry_sources": src.ID, "database_targets": target.ID, "probe_assignments": assignment.ID,
	} {
		f.manage(t, table, id)
	}
	const why = "managed by its environment's inventory file"
	mark := f.mark(t)
	for name, write := range map[string]func() error{
		"update the environment": func() error {
			_, err := f.svc.UpdateEnvironment(ctx, operator, env.ID, inventory.EnvironmentChange{Name: ptr("Renamed")})
			return err
		},
		"delete the environment": func() error { return f.svc.DeleteEnvironment(ctx, operator, env.ID) },
		"update the application": func() error {
			_, err := f.svc.UpdateApplication(ctx, operator, app.ID, inventory.ApplicationChange{Status: ptr(inventory.StatusDisabled)})
			return err
		},
		"delete the application": func() error { return f.svc.DeleteApplication(ctx, operator, app.ID) },
		"update the component": func() error {
			_, err := f.svc.UpdateComponent(ctx, operator, comp.ID, inventory.ComponentChange{RuntimeName: ptr("web-2")})
			return err
		},
		"delete the component": func() error { return f.svc.DeleteComponent(ctx, operator, comp.ID) },
		"update the source": func() error {
			_, err := f.svc.UpdateSource(ctx, operator, src.ID, inventory.SourceChange{Status: ptr(inventory.StatusDisabled)})
			return err
		},
		"delete the source": func() error { return f.svc.DeleteSource(ctx, operator, src.ID) },
		"update the target": func() error {
			_, err := f.svc.UpdateDatabaseTarget(ctx, operator, target.ID, inventory.DatabaseTargetChange{Host: ptr("other.db.internal")})
			return err
		},
		"delete the target": func() error { return f.svc.DeleteDatabaseTarget(ctx, operator, target.ID) },
		"assign a probe to the target": func() error {
			_, err := f.svc.CreateProbeAssignment(ctx, operator, inventory.NewProbeAssignment{DatabaseTargetID: target.ID, ProbeDefinitionID: f.builtin(t, "waits")})
			return err
		},
		"update the assignment": func() error {
			_, err := f.svc.UpdateProbeAssignment(ctx, operator, assignment.ID, inventory.ProbeAssignmentChange{IntervalSeconds: ptr(60)})
			return err
		},
		"approve the assignment": func() error {
			_, err := f.svc.ApproveProbeAssignment(ctx, admin, assignment.ID)
			return err
		},
		"reject the assignment": func() error {
			_, err := f.svc.RejectProbeAssignment(ctx, admin, assignment.ID, "no")
			return err
		},
		"delete the assignment": func() error { return f.svc.DeleteProbeAssignment(ctx, operator, assignment.ID) },
	} {
		t.Run(name, func(t *testing.T) { wantConflict(t, write(), why) })
	}
	f.none(t, mark)
	if got, err := f.svc.GetEnvironment(ctx, env.ID); err != nil || got.Name != env.Name {
		t.Errorf("environment after the refused writes = %+v, %v; want it unchanged", got, err)
	}

	// The API still writes its own rows in that environment.
	other := f.application(t, env.ID, "API shop")
	if _, err := f.svc.UpdateApplication(ctx, operator, other.ID, inventory.ApplicationChange{Status: ptr(inventory.StatusDisabled)}); err != nil {
		t.Errorf("update an API-managed application: %v", err)
	}
	if err := f.svc.DeleteApplication(ctx, operator, other.ID); err != nil {
		t.Errorf("delete an API-managed application: %v", err)
	}
}
