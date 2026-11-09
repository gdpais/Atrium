//go:build integration

package probedefs_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"atrium/apps/api/internal/audit"
	"atrium/apps/api/internal/audit/audittest"
	"atrium/apps/api/internal/probedefs"
	"atrium/apps/api/internal/testenv"
	"atrium/services/db-collector/probecatalog"
)

var admin = audit.UserActor("6f1c0e8a-3c55-4f0b-9d2b-4a7e8c1d2e3f", "admin@example.com")

const leanWaits = "SELECT wait_type, wait_time_ms FROM sys.dm_os_wait_stats WHERE wait_time_ms > 0"

// The seeded built-in definitions are the collector's catalog, no more and
// no less: a probe added to the catalog without a migration that seeds it,
// or a seeded row the catalog does not have, fails here.
func TestSeedIsTheCollectorCatalog(t *testing.T) {
	t.Parallel()
	_, _, pool := testenv.MigratedPostgres(t)
	ctx := context.Background()
	rows, err := pool.Query(ctx, `
select engine, name, category, version, query_template is null, overrides is null, timeout_ms is null, enabled
from probe_definitions where builtin order by engine, name`)
	if err != nil {
		t.Fatal(err)
	}
	var seeded []probecatalog.Probe
	for rows.Next() {
		var p probecatalog.Probe
		var version int
		var noQuery, noOverrides, noTimeout, enabled bool
		if err := rows.Scan(&p.Engine, &p.Name, &p.Category, &version, &noQuery, &noOverrides, &noTimeout, &enabled); err != nil {
			t.Fatal(err)
		}
		if version != 1 || !noQuery || !noOverrides || !noTimeout || !enabled {
			t.Errorf("built-in %s: version %d, no query %t, no overrides %t, no timeout %t, enabled %t; want version 1, enabled, and no query, overrides or timeout",
				p.Name, version, noQuery, noOverrides, noTimeout, enabled)
		}
		seeded = append(seeded, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	catalog := probecatalog.Builtins()
	for _, p := range catalog {
		if !slices.Contains(seeded, p) {
			t.Errorf("the catalog has %+v, which no migration seeds: add a migration that inserts it", p)
		}
	}
	for _, p := range seeded {
		if !slices.Contains(catalog, p) {
			t.Errorf("a migration seeds %+v, which the catalog does not have: disable it in a migration, or fix its category", p)
		}
	}
}

// newService returns a service on a fresh, migrated database, and its pool.
func newService(t *testing.T) (*probedefs.Service, *pgxpool.Pool) {
	t.Helper()
	_, _, pool := testenv.MigratedPostgres(t)
	return probedefs.NewService(pool), pool
}

// auditRecords returns the records in the outbox, the oldest first.
func auditRecords(t *testing.T, pool *pgxpool.Pool) []audit.Record {
	t.Helper()
	rows, err := pool.Query(context.Background(), "select record from audit_outbox order by seq")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var records []audit.Record
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		audittest.Validate(t, raw)
		var r audit.Record
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return records
}

func builtin(t *testing.T, s *probedefs.Service, name string) probedefs.Definition {
	t.Helper()
	defs, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range defs {
		if d.Builtin && d.Name == name {
			return d
		}
	}
	t.Fatalf("no built-in %s among %d definitions", name, len(defs))
	return probedefs.Definition{}
}

func create(t *testing.T, s *probedefs.Service, name string) probedefs.Definition {
	t.Helper()
	def, err := s.Create(context.Background(), probedefs.NewDefinition{
		Engine: "sqlserver", Name: name, Overrides: "waits",
		Content: probedefs.Content{QueryTemplate: leanWaits, Description: "fewer waits"},
	}, admin, "req-create")
	if err != nil {
		t.Fatal(err)
	}
	return def
}

func TestCreate(t *testing.T) {
	t.Parallel()
	s, pool := newService(t)
	ctx := context.Background()
	def := create(t, s, "custom_waits_lean")
	want := probedefs.Definition{
		ID: def.ID, Engine: "sqlserver", Name: "custom_waits_lean", Version: 1, Overrides: "waits", Category: "waits",
		Description: "fewer waits", DefaultIntervalSeconds: probedefs.DefaultIntervalSeconds, QueryTemplate: leanWaits,
		Enabled: true, Latest: true, CreatedAt: def.CreatedAt, UpdatedAt: def.UpdatedAt,
	}
	if def != want || def.CreatedAt.IsZero() {
		t.Fatalf("Create() = %+v, want %+v", def, want)
	}
	if got, err := s.Get(ctx, def.ID); err != nil || got != def {
		t.Fatalf("Get() = %+v, %v; want what Create returned", got, err)
	}

	records := auditRecords(t, pool)
	if len(records) != 1 {
		t.Fatalf("%d audit records, want 1", len(records))
	}
	r := records[0]
	if r.Action != audit.ActionCreate || r.EntityType != audit.EntityProbeDefinition || r.EntityID != def.ID.String() ||
		r.RequestID != "req-create" || r.Actor != admin || string(r.Before) != "null" {
		t.Errorf("record = %+v", r)
	}
	var after probedefs.Definition
	if err := json.Unmarshal(r.After, &after); err != nil {
		t.Fatal(err)
	}
	if after.QueryTemplate != leanWaits || after.Version != 1 || after.Overrides != "waits" {
		t.Errorf("record's after = %s, want the version with its query", r.After)
	}
}

func TestCreateRefuses(t *testing.T) {
	t.Parallel()
	s, pool := newService(t)
	ctx := context.Background()
	create(t, s, "custom_waits_lean")
	content := probedefs.Content{QueryTemplate: leanWaits}
	for _, tc := range []struct {
		name string
		def  probedefs.NewDefinition
		want error
	}{
		{"a custom definition's name", probedefs.NewDefinition{Engine: "sqlserver", Name: "custom_waits_lean", Overrides: "waits", Content: content}, probedefs.ErrNameTaken},
	} {
		if _, err := s.Create(ctx, tc.def, admin, "req"); !errors.Is(err, tc.want) {
			t.Errorf("%s: Create() = %v, want %v", tc.name, err, tc.want)
		}
	}
	for _, tc := range []struct {
		name string
		def  probedefs.NewDefinition
		want string
	}{
		{"another engine", probedefs.NewDefinition{Engine: "postgres", Name: "custom_x", Overrides: "waits", Content: content}, "engine"},
		{"a bad name", probedefs.NewDefinition{Engine: "sqlserver", Name: "custom_Lean Waits", Overrides: "waits", Content: content}, "name"},
		// A built-in's name, or any other without the prefix: a probe the
		// collector adds later could have it.
		{"a built-in's name", probedefs.NewDefinition{Engine: "sqlserver", Name: "waits", Overrides: "waits", Content: content}, `name "waits": want custom_`},
		{"no prefix", probedefs.NewDefinition{Engine: "sqlserver", Name: "waits_lean", Overrides: "waits", Content: content}, `name "waits_lean": want custom_`},
		{"no overrides", probedefs.NewDefinition{Engine: "sqlserver", Name: "custom_x", Content: content}, `overrides ""`},
		{"overrides not a probe name", probedefs.NewDefinition{Engine: "sqlserver", Name: "custom_x", Overrides: "Waits", Content: content}, `overrides "Waits"`},
		{"an unknown built-in", probedefs.NewDefinition{Engine: "sqlserver", Name: "custom_x", Overrides: "deadlocks", Content: content}, `overrides "deadlocks"`},
		{"a custom definition as the built-in", probedefs.NewDefinition{Engine: "sqlserver", Name: "custom_x", Overrides: "custom_waits_lean", Content: content}, `overrides "custom_waits_lean"`},
		{"a write", probedefs.NewDefinition{Engine: "sqlserver", Name: "custom_x", Overrides: "waits", Content: probedefs.Content{QueryTemplate: "DELETE FROM t"}}, "query_template must be one read-only SELECT"},
	} {
		_, err := s.Create(ctx, tc.def, admin, "req")
		var invalid *probedefs.InvalidError
		if !errors.As(err, &invalid) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Create() = %v, want an *InvalidError about %s", tc.name, err, tc.want)
		}
	}
	if records := auditRecords(t, pool); len(records) != 1 {
		t.Errorf("%d audit records, want only the first creation's", len(records))
	}
}

// A change and its audit record commit together: when the record cannot be
// written, the definition is not stored either.
func TestChangesRollBackWithoutTheirRecord(t *testing.T) {
	t.Parallel()
	s, pool := newService(t)
	ctx := context.Background()
	noActor := audit.Actor{}
	if _, err := s.Create(ctx, probedefs.NewDefinition{Engine: "sqlserver", Name: "custom_waits_lean", Overrides: "waits",
		Content: probedefs.Content{QueryTemplate: leanWaits}}, noActor, "req"); err == nil {
		t.Fatal("Create() with an actor the record refuses succeeded")
	}
	var n int
	if err := pool.QueryRow(ctx, "select count(*) from probe_definitions where not builtin").Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d custom definitions (%v), want none", n, err)
	}

	def := create(t, s, "custom_waits_lean")
	if _, _, err := s.AddVersion(ctx, def.ID, probedefs.Content{QueryTemplate: "SELECT 1"}, noActor, "req"); err == nil {
		t.Fatal("AddVersion() with an actor the record refuses succeeded")
	}
	if _, err := s.SetEnabled(ctx, def.ID, false, noActor, "req"); err == nil {
		t.Fatal("SetEnabled() with an actor the record refuses succeeded")
	}
	versions, err := s.Versions(ctx, def.ID)
	if err != nil || len(versions) != 1 || !versions[0].Enabled {
		t.Fatalf("Versions() = %+v, %v; want version 1 alone, enabled", versions, err)
	}
	if records := auditRecords(t, pool); len(records) != 1 {
		t.Errorf("%d audit records, want only the creation's", len(records))
	}
}

func TestAddVersion(t *testing.T) {
	t.Parallel()
	s, pool := newService(t)
	ctx := context.Background()
	v1 := create(t, s, "custom_waits_lean")
	changed := probedefs.Content{QueryTemplate: leanWaits + " AND wait_type <> N'CXPACKET'", DefaultIntervalSeconds: 60, TimeoutMS: 5000}
	v2, added, err := s.AddVersion(ctx, v1.ID, changed, admin, "req-v2")
	if err != nil || !added {
		t.Fatalf("AddVersion() = %+v, %t, %v; want a new version", v2, added, err)
	}
	if v2.ID == v1.ID || v2.Version != 2 || v2.Name != v1.Name || v2.Overrides != "waits" || v2.Category != "waits" || v2.QueryTemplate != changed.QueryTemplate ||
		v2.DefaultIntervalSeconds != 60 || v2.TimeoutMS != 5000 || v2.Description != "" || !v2.Latest {
		t.Fatalf("AddVersion() = %+v", v2)
	}

	// Version 1 stays as it was, for the assignments that pin it.
	old, err := s.Get(ctx, v1.ID)
	if err != nil {
		t.Fatal(err)
	}
	v1.Latest = false
	if old != v1 {
		t.Errorf("version 1 after the change = %+v, want %+v", old, v1)
	}
	list, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(list, func(d probedefs.Definition) bool { return d.Name == "custom_waits_lean" }); i < 0 || list[i].ID != v2.ID {
		t.Errorf("List() = %+v, want version 2 of custom_waits_lean", list)
	}
	versions, err := s.Versions(ctx, v1.ID)
	if err != nil || len(versions) != 2 || versions[0].ID != v2.ID || versions[1].ID != v1.ID || !versions[0].Latest || versions[1].Latest {
		t.Errorf("Versions() = %+v, %v; want version 2, then 1", versions, err)
	}

	// The same content again adds nothing.
	same, added, err := s.AddVersion(ctx, v2.ID, changed, admin, "req-same")
	if err != nil || added || same.ID != v2.ID {
		t.Errorf("AddVersion() of the same content = %+v, %t, %v; want version 2 and nothing added", same, added, err)
	}

	// A change from an older version would lose version 2's.
	_, _, err = s.AddVersion(ctx, v1.ID, probedefs.Content{QueryTemplate: "SELECT 1"}, admin, "req-stale")
	var conflict *probedefs.ConflictError
	if !errors.As(err, &conflict) || !strings.Contains(err.Error(), "not the latest") {
		t.Errorf("AddVersion() from version 1 = %v, want a conflict: not the latest", err)
	}

	records := auditRecords(t, pool)
	if len(records) != 2 {
		t.Fatalf("%d audit records, want the creation's and version 2's", len(records))
	}
	r := records[1]
	var before, after probedefs.Definition
	if err := errors.Join(json.Unmarshal(r.Before, &before), json.Unmarshal(r.After, &after)); err != nil {
		t.Fatal(err)
	}
	if r.Action != audit.ActionUpdate || r.EntityID != v2.ID.String() || before.ID != v1.ID || before.Version != 1 || after.Version != 2 || after.QueryTemplate != changed.QueryTemplate {
		t.Errorf("record = %+v, before %+v, after %+v; want an update from version 1 to 2", r, before, after)
	}
}

func TestAddVersionRefuses(t *testing.T) {
	t.Parallel()
	s, _ := newService(t)
	ctx := context.Background()
	content := probedefs.Content{QueryTemplate: "SELECT 1"}
	if _, _, err := s.AddVersion(ctx, builtin(t, s, "waits").ID, content, admin, "req"); !errors.Is(err, probedefs.ErrBuiltin) {
		t.Errorf("AddVersion() of a built-in = %v, want %v", err, probedefs.ErrBuiltin)
	}
	if _, _, err := s.AddVersion(ctx, uuid.New(), content, admin, "req"); !errors.Is(err, probedefs.ErrNotFound) {
		t.Errorf("AddVersion() of an unknown ID = %v, want %v", err, probedefs.ErrNotFound)
	}
	def := create(t, s, "custom_waits_lean")
	if _, err := s.SetEnabled(ctx, def.ID, false, admin, "req"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddVersion(ctx, def.ID, content, admin, "req"); !errors.Is(err, probedefs.ErrDisabled) {
		t.Errorf("AddVersion() of a disabled definition = %v, want %v", err, probedefs.ErrDisabled)
	}
}

// Two changes from the same latest version take turns: one adds version 2,
// the other is refused, and no version is written twice.
func TestConcurrentVersionsTakeTurns(t *testing.T) {
	t.Parallel()
	s, pool := newService(t)
	ctx := context.Background()
	v1 := create(t, s, "custom_waits_lean")
	const writers = 8
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := range writers {
		wg.Go(func() {
			_, _, errs[i] = s.AddVersion(ctx, v1.ID, probedefs.Content{QueryTemplate: fmt.Sprintf("SELECT %d AS wait_time_ms", i)}, admin, "req")
		})
	}
	wg.Wait()
	succeeded := 0
	for _, err := range errs {
		var conflict *probedefs.ConflictError
		switch {
		case err == nil:
			succeeded++
		case !errors.As(err, &conflict):
			t.Errorf("AddVersion() = %v, want success or a conflict", err)
		}
	}
	versions, err := s.Versions(ctx, v1.ID)
	if succeeded != 1 || err != nil || len(versions) != 2 {
		t.Errorf("%d succeeded, versions %d (%v); want 1 and 2", succeeded, len(versions), err)
	}
	if records := auditRecords(t, pool); len(records) != 2 {
		t.Errorf("%d audit records, want 2", len(records))
	}
}

// A version added while the definition is being disabled ends up disabled
// too, or is refused: the two take turns, so no version is left enabled
// beside disabled ones.
func TestDisableAndAddVersionTakeTurns(t *testing.T) {
	t.Parallel()
	s, _ := newService(t)
	ctx := context.Background()
	for i := range 30 {
		v1 := create(t, s, fmt.Sprintf("custom_waits_race_%d", i))
		var wg sync.WaitGroup
		var addErr, disableErr error
		wg.Go(func() {
			_, _, addErr = s.AddVersion(ctx, v1.ID, probedefs.Content{QueryTemplate: "SELECT 1 AS wait_time_ms"}, admin, "req")
		})
		wg.Go(func() { _, disableErr = s.SetEnabled(ctx, v1.ID, false, admin, "req") })
		wg.Wait()
		if disableErr != nil || (addErr != nil && !errors.Is(addErr, probedefs.ErrDisabled)) {
			t.Fatalf("round %d: AddVersion() = %v, SetEnabled() = %v; want success or ErrDisabled, and success", i, addErr, disableErr)
		}
		versions, err := s.Versions(ctx, v1.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range versions {
			if v.Enabled {
				t.Fatalf("round %d: version %d of %d is enabled after the definition was disabled", i, v.Version, len(versions))
			}
		}
	}
}

func TestSetEnabled(t *testing.T) {
	t.Parallel()
	s, pool := newService(t)
	ctx := context.Background()
	v1 := create(t, s, "custom_waits_lean")
	v2, _, err := s.AddVersion(ctx, v1.ID, probedefs.Content{QueryTemplate: "SELECT 1 AS wait_time_ms"}, admin, "req")
	if err != nil {
		t.Fatal(err)
	}

	// Disabling through an older version disables the definition: every
	// version.
	disabled, err := s.SetEnabled(ctx, v1.ID, false, admin, "req-disable")
	if err != nil || disabled.Enabled || disabled.ID != v1.ID {
		t.Fatalf("SetEnabled(false) = %+v, %v; want version 1, disabled", disabled, err)
	}
	versions, err := s.Versions(ctx, v2.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range versions {
		if v.Enabled {
			t.Errorf("version %d enabled after the definition was disabled", v.Version)
		}
		if v.QueryTemplate == "" {
			t.Errorf("version %d lost its query", v.Version)
		}
	}
	// Disabling it again writes nothing.
	if _, err := s.SetEnabled(ctx, v2.ID, false, admin, "req-again"); err != nil {
		t.Fatal(err)
	}
	enabled, err := s.SetEnabled(ctx, v2.ID, true, admin, "req-enable")
	if err != nil || !enabled.Enabled {
		t.Fatalf("SetEnabled(true) = %+v, %v; want enabled", enabled, err)
	}

	if _, err := s.SetEnabled(ctx, builtin(t, s, "waits").ID, false, admin, "req"); !errors.Is(err, probedefs.ErrBuiltin) {
		t.Errorf("SetEnabled() of a built-in = %v, want %v", err, probedefs.ErrBuiltin)
	}
	if _, err := s.SetEnabled(ctx, uuid.New(), false, admin, "req"); !errors.Is(err, probedefs.ErrNotFound) {
		t.Errorf("SetEnabled() of an unknown ID = %v, want %v", err, probedefs.ErrNotFound)
	}

	var actions []string
	for _, r := range auditRecords(t, pool) {
		actions = append(actions, r.Action+" "+r.RequestID)
	}
	want := []string{"create req-create", "update req", "disable req-disable", "enable req-enable"}
	if !slices.Equal(actions, want) {
		t.Errorf("audit records %v, want %v", actions, want)
	}
}

func TestGetAndVersionsOfAnUnknownID(t *testing.T) {
	t.Parallel()
	s, _ := newService(t)
	ctx := context.Background()
	if _, err := s.Get(ctx, uuid.New()); !errors.Is(err, probedefs.ErrNotFound) {
		t.Errorf("Get() = %v, want %v", err, probedefs.ErrNotFound)
	}
	if _, err := s.Versions(ctx, uuid.New()); !errors.Is(err, probedefs.ErrNotFound) {
		t.Errorf("Versions() = %v, want %v", err, probedefs.ErrNotFound)
	}
}
