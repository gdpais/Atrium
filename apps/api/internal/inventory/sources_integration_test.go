//go:build integration

package inventory_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"heartbeat/apps/api/internal/audit"
	"heartbeat/apps/api/internal/collectors"
	"heartbeat/apps/api/internal/inventory"
)

// watching is an Observer whose collector extracts the environments it
// lists.
type watching map[string][]string

func (w watching) OutSystemsLogs(context.Context) (collectors.Coverage, error) {
	return collectors.Coverage{CheckedAt: time.Now(), ByEnvironment: w}, nil
}

func outsystems(app uuid.UUID, mode, name string) inventory.NewSource {
	return inventory.NewSource{ApplicationID: app, SourceType: "outsystems", IngestMode: mode, Config: map[string]any{"application_name": name}}
}

func TestSources(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	svc := inventory.New(f.pool, inventory.WithObserver(watching{"prod": {"oslogs-prod"}}))
	ctx := context.Background()
	prod, staging := f.environment(t, "prod"), f.environment(t, "staging")
	claims, billing := f.application(t, prod.ID, "Claims"), f.application(t, prod.ID, "Billing")
	stagingClaims := f.application(t, staging.ID, "Claims")

	t.Run("registered for its application, environment derived, observed, audited", func(t *testing.T) {
		mark := f.mark(t)
		src, err := svc.CreateSource(ctx, operator, outsystems(claims.ID, "agentless", "Claims"))
		if err != nil {
			t.Fatal(err)
		}
		if src.ApplicationID != claims.ID || src.EnvironmentID != prod.ID || src.Status != inventory.StatusActive || src.Config["application_name"] != "Claims" {
			t.Fatalf("created %+v", src)
		}
		if src.Observation.Status != inventory.Observed || src.Observation.Collectors[0] != "oslogs-prod" {
			t.Fatalf("observation %+v", src.Observation)
		}
		r := f.one(t, mark, audit.ActionCreate, audit.EntityTelemetrySource, src.ID)
		after := decode[map[string]any](t, r.After)
		if after["environment_id"] != prod.ID.String() || after["observation"] != nil {
			t.Fatalf("create record after %s: want the derived environment and no observation", r.After)
		}
		got, err := svc.GetSource(ctx, src.ID)
		if err != nil || got.Observation.Status != inventory.Observed || got.EnvironmentID != prod.ID {
			t.Fatalf("get = %+v, %v", got, err)
		}
	})

	t.Run("a staging source is registered, and not observed", func(t *testing.T) {
		src, err := svc.CreateSource(ctx, operator, outsystems(stagingClaims.ID, "agentless", "Claims"))
		if err != nil {
			t.Fatalf("the same name in another environment: %v", err)
		}
		if src.Observation.Status != inventory.NotObserved {
			t.Fatalf("observation %+v", src.Observation)
		}
	})

	t.Run("within an environment, a name maps to one application", func(t *testing.T) {
		mark := f.mark(t)
		_, err := svc.CreateSource(ctx, operator, outsystems(billing.ID, "otlp", "Claims"))
		wantConflict(t, err, `application_name "Claims" is application `+claims.ID.String())
		_, err = svc.CreateSource(ctx, operator, outsystems(claims.ID, "agentless", "Claims"))
		wantConflict(t, err, "already has a outsystems agentless source")
		f.none(t, mark)
		otlp, err := svc.CreateSource(ctx, operator, outsystems(claims.ID, "otlp", "Claims"))
		if err != nil || otlp.Observation.Status != inventory.NotApplicable {
			t.Fatalf("the same application's otlp source: %+v, %v", otlp, err)
		}
	})

	t.Run("config, type and mode are checked before anything is written", func(t *testing.T) {
		mark := f.mark(t)
		for _, in := range []inventory.NewSource{
			{ApplicationID: claims.ID, SourceType: "outsystems", IngestMode: "agentless", Config: map[string]any{}},
			{ApplicationID: claims.ID, SourceType: "outsystems", IngestMode: "agentless", Config: map[string]any{"application_name": "X", "password": "hunter2"}},
			{ApplicationID: claims.ID, SourceType: "outsystems", IngestMode: "file", Config: map[string]any{"application_name": "X"}},
			{ApplicationID: claims.ID, SourceType: "splunk", IngestMode: "api", Config: map[string]any{"application_name": "X"}},
		} {
			var invalid *inventory.InvalidError
			if _, err := svc.CreateSource(ctx, operator, in); !errors.As(err, &invalid) {
				t.Errorf("%+v: %v, want an InvalidError", in, err)
			}
		}
		_, err := svc.CreateSource(ctx, operator, outsystems(uuid.New(), "agentless", "Orphan"))
		wantOwner(t, err)
		f.none(t, mark)
		var n int
		if err := f.pool.QueryRow(ctx, "select count(*) from telemetry_sources where config::text like '%hunter2%'").Scan(&n); err != nil || n != 0 {
			t.Fatalf("a refused config was stored: %d, %v", n, err)
		}
	})

	t.Run("changed, refused a taken name, no-op unaudited, deleted", func(t *testing.T) {
		src, err := svc.CreateSource(ctx, operator, outsystems(billing.ID, "agentless", "Billing"))
		if err != nil {
			t.Fatal(err)
		}
		mark := f.mark(t)
		disabled := inventory.StatusDisabled
		changed, err := svc.UpdateSource(ctx, operator, src.ID, inventory.SourceChange{Status: &disabled})
		if err != nil || changed.Status != disabled || changed.Config["application_name"] != "Billing" {
			t.Fatalf("change = %+v, %v", changed, err)
		}
		r := f.one(t, mark, audit.ActionUpdate, audit.EntityTelemetrySource, src.ID)
		if decode[map[string]any](t, r.Before)["status"] != "active" {
			t.Fatalf("update record before %s", r.Before)
		}
		mark = f.mark(t)
		taken := map[string]any{"application_name": "Claims"}
		_, err = svc.UpdateSource(ctx, operator, src.ID, inventory.SourceChange{Config: &taken})
		wantConflict(t, err, "maps to one application")
		same := map[string]any{"application_name": "Billing"}
		if _, err := svc.UpdateSource(ctx, operator, src.ID, inventory.SourceChange{Config: &same, Status: &disabled}); err != nil {
			t.Fatal(err)
		}
		f.none(t, mark)
		if err := svc.DeleteSource(ctx, operator, src.ID); err != nil {
			t.Fatal(err)
		}
		f.one(t, mark, audit.ActionDelete, audit.EntityTelemetrySource, src.ID)
		if _, err := svc.GetSource(ctx, src.ID); !errors.Is(err, inventory.ErrNotFound) {
			t.Fatalf("get after delete = %v", err)
		}
	})

	t.Run("a schema tightened later does not stop a source being disabled", func(t *testing.T) {
		legacyApp := f.application(t, prod.ID, "Legacy")
		var id uuid.UUID
		// A config valid when it was written and not under today's schema,
		// as after a release that tightens it.
		if err := f.pool.QueryRow(ctx, `insert into telemetry_sources (application_id, source_type, ingest_mode, config)
			values ($1, 'outsystems', 'agentless', '{"application_name":"Legacy","retired_field":1}') returning id`, legacyApp.ID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		disabled := inventory.StatusDisabled
		got, err := svc.UpdateSource(ctx, operator, id, inventory.SourceChange{Status: &disabled})
		if err != nil || got.Status != disabled {
			t.Fatalf("disable = %+v, %v", got, err)
		}
		fixed := map[string]any{"application_name": "Legacy", "retired_field": 1}
		var invalid *inventory.InvalidError
		if _, err := svc.UpdateSource(ctx, operator, id, inventory.SourceChange{Config: &fixed}); !errors.As(err, &invalid) {
			t.Fatalf("a new config is still checked: %v", err)
		}
	})

	t.Run("lists filter by environment and page by name", func(t *testing.T) {
		page, err := svc.ListSources(ctx, inventory.SourceFilter{EnvironmentID: prod.ID}, inventory.Page{Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for {
			for _, src := range page.Items {
				if src.EnvironmentID != prod.ID {
					t.Fatalf("the prod filter returned %+v", src)
				}
				names = append(names, fmt.Sprint(src.Config["application_name"], "/", src.IngestMode, "/", src.Observation.Status))
			}
			if page.NextCursor == "" {
				break
			}
			if page, err = svc.ListSources(ctx, inventory.SourceFilter{EnvironmentID: prod.ID}, inventory.Page{Limit: 1, Cursor: page.NextCursor}); err != nil {
				t.Fatal(err)
			}
		}
		if len(names) != 3 || !strings.HasPrefix(names[0], "Claims/") || !strings.HasPrefix(names[1], "Claims/") || !strings.HasPrefix(names[2], "Legacy/") {
			t.Fatalf("prod sources %v, want Claims's agentless and otlp, then Legacy's", names)
		}
	})
}

// Two applications that register the same application_name at once in one
// environment: the environment's source lock lets one through, and the
// others find the name taken.
func TestConcurrentRegistrationsClaimANameOnce(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	svc := inventory.New(f.pool)
	prod := f.environment(t, "prod")
	const n = 8
	apps := make([]uuid.UUID, n)
	for i := range apps {
		apps[i] = f.application(t, prod.ID, fmt.Sprintf("App %d", i)).ID
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	for i := range apps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = svc.CreateSource(context.Background(), operator, outsystems(apps[i], "agentless", "Shared"))
		}()
	}
	close(start)
	wg.Wait()
	won := 0
	for _, err := range errs {
		var conflict *inventory.ConflictError
		switch {
		case err == nil:
			won++
		case !errors.As(err, &conflict):
			t.Errorf("unexpected error: %v", err)
		}
	}
	var stored int
	if err := f.pool.QueryRow(context.Background(), "select count(*) from telemetry_sources where config->>'application_name' = 'Shared'").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if won != 1 || stored != 1 {
		t.Fatalf("%d registrations succeeded and %d are stored, want 1 and 1", won, stored)
	}
}
