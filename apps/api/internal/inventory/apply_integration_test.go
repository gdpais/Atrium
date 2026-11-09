//go:build integration

package inventory_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"atrium/apps/api/internal/audit"
	"atrium/apps/api/internal/audit/audittest"
	"atrium/apps/api/internal/inventory"
)

var fileCaller = inventory.Caller{
	Actor:     audit.ConfigActor("/etc/atrium/inventory/inventory.yaml", "api-test"),
	RequestID: "inventory-test",
}

// shopFile is an environment with two applications, a source each, and a
// target with two probes, one disabled.
const shopFile = `version: 1
environment:
  slug: shop-%s
  name: Shop
  type: staging
  labels: {region: eu}
applications:
  - name: Storefront
    platform: outsystems
    owner_team: Web
    components:
      - {name: Portal, type: reactive_web_app, runtime_name: StorePortal}
    telemetry_sources:
      - type: outsystems
        ingest_mode: agentless
        config: {application_name: Storefront}
  - name: Orders
    platform: outsystems
    telemetry_sources:
      - type: outsystems
        ingest_mode: agentless
        config: {application_name: Orders}
database_targets:
  - name: shop-db
    host: SHOP-SQL.internal
    database_name: Shop
    credential_ref: env/shop-db
    probes:
      - {name: blocking, interval_seconds: 30}
      - {name: cpu, interval_seconds: 30, enabled: false}
`

func mustUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func parse(t *testing.T, text string) inventory.File {
	t.Helper()
	file, err := inventory.ParseFile([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func shop(t *testing.T, slug string) string {
	t.Helper()
	return strings.ReplaceAll(shopFile, "%s", slug)
}

func (f fixture) apply(t *testing.T, revision, text string) inventory.ApplyResult {
	t.Helper()
	result, err := f.svc.ApplyFile(context.Background(), fileCaller, revision, parse(t, text))
	if err != nil {
		t.Fatalf("apply %s: %v", revision, err)
	}
	return result
}

// fileRecord is an audit record with its config version.
type fileRecord struct {
	record
	ConfigVersion string `json:"config_version"`
}

func (f fixture) fileRecordsSince(t *testing.T, mark int64) []fileRecord {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), "select record::text from audit_outbox where seq > $1 order by seq", mark)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []fileRecord
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		audittest.Validate(t, []byte(raw))
		var r fileRecord
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// count returns how many rows of table match where, with args.
func (f fixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), "select count(*) from "+query, args...).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", query, err)
	}
	return n
}

// The first apply creates every row of the file, managed by the file and
// audited as the file, at its revision; the file's probes are approved
// with no user named. Applying it again changes and audits nothing.
func TestApplyFileCreatesTheInventoryOnceAndAuditsIt(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mark := f.mark(t)
	result := f.apply(t, "sha256:one", shop(t, "create"))
	// 1 environment, 2 applications, 1 component, 2 sources, 1 target, 2 assignments.
	if result.Changes != 9 {
		t.Fatalf("changes = %d, want 9", result.Changes)
	}
	records := f.fileRecordsSince(t, mark)
	if len(records) != 9 {
		t.Fatalf("%d records, want 9", len(records))
	}
	for _, r := range records {
		if r.Action != audit.ActionCreate || r.Actor != fileCaller.Actor || r.RequestID != fileCaller.RequestID || r.ConfigVersion != "sha256:one" {
			t.Fatalf("record %+v: want a create by the file at sha256:one", r)
		}
	}
	for _, table := range []string{"environments", "applications", "application_components", "telemetry_sources", "database_targets", "probe_assignments"} {
		if n := f.count(t, table+" where managed_by <> 'config'"); n != 0 {
			t.Errorf("%d %s not managed by the file", n, table)
		}
	}
	if n := f.count(t, "probe_assignments where state = 'approved' and approved_by is null and approved_at is not null"); n != 2 {
		t.Fatalf("%d assignments approved by the file, want 2", n)
	}
	if n := f.count(t, "database_targets where host = 'shop-sql.internal' and port = 1433"); n != 1 {
		t.Fatal("the target is not stored normalized")
	}

	// The render holds the enabled probe, as the collector will run it.
	rendered, err := f.svc.RenderProbeAssignments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered), "name: blocking") || strings.Contains(string(rendered), "name: cpu") {
		t.Fatalf("render:\n%s", rendered)
	}
	// The file renders the same bytes without a database (ADR 0021 §5).
	offline, err := inventory.RenderFile(parse(t, shop(t, "create")))
	if err != nil || string(offline) != string(rendered) {
		t.Fatalf("RenderFile() = %v\n%s\nwant the API's render\n%s", err, offline, rendered)
	}

	mark = f.mark(t)
	if again := f.apply(t, "sha256:one", shop(t, "create")); again.Changes != 0 {
		t.Fatalf("second apply changed %d rows", again.Changes)
	}
	f.none(t, mark)

	// The API refuses to change what the file manages.
	var appID string
	if err := f.pool.QueryRow(ctx, "select id::text from applications where name = 'Orders'").Scan(&appID); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.UpdateApplication(ctx, operator, mustUUID(t, appID), inventory.ApplicationChange{OwnerTeam: ptr("Someone")})
	var conflict *inventory.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("UpdateApplication = %v, want a conflict", err)
	}
}

// A changed file updates what changed and disables what it dropped: an
// application and its source, a probe, and a target and its probes. A
// target moved to another server sends its applied probes back to
// approved, for the applied check to confirm.
func TestApplyFileUpdatesAndDisables(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	text := shop(t, "change")
	f.apply(t, "sha256:a", text)
	if _, err := f.pool.Exec(ctx, "update probe_assignments set state = 'applied'"); err != nil {
		t.Fatal(err)
	}

	changed := strings.Replace(text, "owner_team: Web", "owner_team: Storefront squad", 1)
	changed = strings.Replace(changed, "host: SHOP-SQL.internal", "host: shop-sql-2.internal", 1)
	changed = strings.Replace(changed, "      - {name: cpu, interval_seconds: 30, enabled: false}\n", "", 1)
	// Orders, with its source, is dropped.
	changed = changed[:strings.Index(changed, "  - name: Orders")] + changed[strings.Index(changed, "database_targets:"):]
	changed += `  - name: archive-db
    host: archive-sql.internal
    credential_ref: env/archive-db
`
	mark := f.mark(t)
	result := f.apply(t, "sha256:b", changed)

	byAction := map[string][]string{}
	for _, r := range f.fileRecordsSince(t, mark) {
		if r.ConfigVersion != "sha256:b" {
			t.Fatalf("record %+v: want sha256:b", r)
		}
		byAction[r.Action] = append(byAction[r.Action], r.EntityType)
	}
	want := map[string][]string{
		// The application's owner, the target's host, and its two probes
		// sent back to approved; the new target.
		audit.ActionUpdate: {audit.EntityApplication, audit.EntityDatabaseTarget, audit.EntityProbeAssignment, audit.EntityProbeAssignment},
		audit.ActionCreate: {audit.EntityDatabaseTarget},
		// Orders and its source. The cpu probe was disabled already.
		audit.ActionDisable: {audit.EntityApplication, audit.EntityTelemetrySource},
	}
	if result.Changes != 7 || len(byAction) != len(want) {
		t.Fatalf("changes = %d, records by action %v; want 7, %v", result.Changes, byAction, want)
	}
	for action, types := range want {
		if strings.Join(byAction[action], ",") != strings.Join(types, ",") {
			t.Errorf("%s records %v, want %v", action, byAction[action], types)
		}
	}
	if n := f.count(t, "applications where name = 'Orders' and status = 'disabled'"); n != 1 {
		t.Error("Orders is not disabled")
	}
	if n := f.count(t, "telemetry_sources where config->>'application_name' = 'Orders' and status = 'disabled'"); n != 1 {
		t.Error("Orders' source is not disabled")
	}
	if n := f.count(t, "probe_assignments a join probe_definitions d on d.id = a.probe_definition_id where d.name = 'blocking' and a.state = 'approved'"); n != 1 {
		t.Error("the moved target's applied probe is not back to approved")
	}
	if n := f.count(t, "probe_assignments a join probe_definitions d on d.id = a.probe_definition_id where d.name = 'cpu' and not a.enabled"); n != 1 {
		t.Error("the dropped probe is not disabled")
	}

	// Dropping the target disables it and its probes.
	withoutTarget := changed[:strings.Index(changed, "  - name: shop-db")] + changed[strings.Index(changed, "  - name: archive-db"):]
	mark = f.mark(t)
	f.apply(t, "sha256:c", withoutTarget)
	if n := f.count(t, "database_targets where name = 'shop-db' and status = 'disabled'"); n != 1 {
		t.Error("the dropped target is not disabled")
	}
	if n := f.count(t, "probe_assignments where enabled"); n != 0 {
		t.Errorf("%d probes still enabled on the dropped target", n)
	}
	if n := len(f.fileRecordsSince(t, mark)); n != 2 {
		t.Errorf("%d records for the dropped target, want its disable and its probe's", n)
	}

	// Listing Orders again enables it and its source.
	mark = f.mark(t)
	f.apply(t, "sha256:d", text)
	if n := f.count(t, "applications where status = 'disabled'") + f.count(t, "telemetry_sources where status = 'disabled'"); n != 0 {
		t.Errorf("%d rows still disabled after the file lists them again", n)
	}
	// The file records a row it switches back on as an update from
	// disabled to active.
	var enabled []string
	for _, r := range f.fileRecordsSince(t, mark) {
		if r.ConfigVersion != "sha256:d" {
			t.Fatalf("record %+v: want sha256:d", r)
		}
		var before, after struct{ Status string }
		if r.Action != audit.ActionUpdate || json.Unmarshal(r.Before, &before) != nil || json.Unmarshal(r.After, &after) != nil {
			continue
		}
		if before.Status == inventory.StatusDisabled && after.Status == inventory.StatusActive {
			enabled = append(enabled, r.EntityType)
		}
	}
	if !slices.Contains(enabled, audit.EntityApplication) || !slices.Contains(enabled, audit.EntityTelemetrySource) {
		t.Errorf("records enabling %v, want Orders and its source among them", enabled)
	}
}

// A file that names a row the API manages changes nothing, not even what
// it would have changed before reaching that row, and audits nothing.
func TestApplyFileRefusesRowsTheAPIManages(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	t.Run("environment", func(t *testing.T) {
		f.environment(t, "shop-api-owned")
		mark := f.mark(t)
		_, err := f.svc.ApplyFile(ctx, fileCaller, "sha256:x", parse(t, shop(t, "api-owned")))
		var fileErr *inventory.FileError
		if !errors.As(err, &fileErr) || !strings.Contains(err.Error(), "environment.slug") {
			t.Fatalf("ApplyFile = %v, want a problem at environment.slug", err)
		}
		f.none(t, mark)
	})

	t.Run("application", func(t *testing.T) {
		text := shop(t, "mixed")
		f.apply(t, "sha256:1", text)
		var envID string
		if err := f.pool.QueryRow(ctx, "select id::text from environments where slug = 'shop-mixed'").Scan(&envID); err != nil {
			t.Fatal(err)
		}
		f.application(t, mustUUID(t, envID), "Billing")
		mark := f.mark(t)
		changed := strings.Replace(text, "name: Shop\n", "name: Shop renamed\n", 1)
		changed = strings.Replace(changed, "database_targets:", "  - name: Billing\n    platform: outsystems\ndatabase_targets:", 1)
		_, err := f.svc.ApplyFile(ctx, fileCaller, "sha256:2", parse(t, changed))
		var fileErr *inventory.FileError
		if !errors.As(err, &fileErr) || !strings.Contains(err.Error(), "applications[2].name") {
			t.Fatalf("ApplyFile = %v, want a problem at applications[2].name", err)
		}
		f.none(t, mark)
		if n := f.count(t, "environments where name = 'Shop renamed'"); n != 0 {
			t.Fatal("the refused file renamed the environment")
		}
	})

	t.Run("application_name", func(t *testing.T) {
		text := shop(t, "claimed")
		f.apply(t, "sha256:1", text)
		var envID string
		if err := f.pool.QueryRow(ctx, "select id::text from environments where slug = 'shop-claimed'").Scan(&envID); err != nil {
			t.Fatal(err)
		}
		other := f.application(t, mustUUID(t, envID), "Legacy")
		if _, err := f.svc.CreateSource(ctx, operator, outsystems(other.ID, inventory.IngestAgentless, "Catalog")); err != nil {
			t.Fatal(err)
		}
		changed := strings.Replace(text, "config: {application_name: Orders}", "config: {application_name: Catalog}", 1)
		_, err := f.svc.ApplyFile(ctx, fileCaller, "sha256:2", parse(t, changed))
		var fileErr *inventory.FileError
		if !errors.As(err, &fileErr) || !strings.Contains(err.Error(), "applications[1].telemetry_sources[0].config.application_name") {
			t.Fatalf("ApplyFile = %v, want a problem at the source's application_name", err)
		}
	})
}

// An application renamed in the file is a new application: the old one is
// disabled, and its source with it, so the new one's source can take the
// application_name.
func TestApplyFileMovesAnApplicationNameToARenamedApplication(t *testing.T) {
	f := newFixture(t)
	text := shop(t, "rename")
	f.apply(t, "sha256:1", text)
	f.apply(t, "sha256:2", strings.Replace(text, "  - name: Orders\n", "  - name: Order management\n", 1))
	if n := f.count(t, "applications where name = 'Orders' and status = 'disabled'"); n != 1 {
		t.Fatal("the old application is not disabled")
	}
	if n := f.count(t, "telemetry_sources s join applications a on a.id = s.application_id where a.name = 'Order management' and s.status = 'active'"); n != 1 {
		t.Fatal("the renamed application has no active source")
	}
}

// A target renamed in the file is refused, naming the old target: the
// disabled row would still hold its server, port and database.
func TestApplyFileRefusesARenamedTarget(t *testing.T) {
	f := newFixture(t)
	text := shop(t, "target-rename")
	f.apply(t, "sha256:1", text)
	mark := f.mark(t)
	_, err := f.svc.ApplyFile(context.Background(), fileCaller, "sha256:2", parse(t, strings.Replace(text, "  - name: shop-db\n", "  - name: shop-primary\n", 1)))
	var fileErr *inventory.FileError
	if !errors.As(err, &fileErr) || !strings.Contains(err.Error(), "database_targets[0]") || !strings.Contains(err.Error(), `list it under its name "shop-db"`) {
		t.Fatalf("ApplyFile = %v, want a problem at database_targets[0] naming shop-db", err)
	}
	f.none(t, mark)
	if n := f.count(t, "database_targets where name = 'shop-db' and status = 'active'"); n != 1 {
		t.Fatal("the refused file disabled the target")
	}
}

// Replicas applying the same file at once take turns: one makes the
// changes, the others find nothing to change.
func TestApplyFileTakesTurns(t *testing.T) {
	f := newFixture(t)
	file := parse(t, shop(t, "turns"))
	var wg sync.WaitGroup
	changes := make([]int, 4)
	errs := make([]error, 4)
	for i := range changes {
		wg.Go(func() {
			result, err := f.svc.ApplyFile(context.Background(), fileCaller, "sha256:t", file)
			changes[i], errs[i] = result.Changes, err
		})
	}
	wg.Wait()
	total := 0
	for i, n := range changes {
		if errs[i] != nil {
			t.Fatalf("apply %d: %v", i, errs[i])
		}
		total += n
	}
	if total != 9 {
		t.Fatalf("changes %v: want 9 in all, from one apply", changes)
	}
	if n := f.count(t, "environments where slug = 'shop-turns'"); n != 1 {
		t.Fatalf("%d environments", n)
	}
}

// The FileApplier applies the mounted file when it changes, counts each
// result, and leaves PostgreSQL as it was when the file is not valid.
func TestFileApplierAppliesTheFileWhenItChanges(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "inventory.yaml")
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	applier := inventory.NewFileApplier(f.svc, path, "api-test", nil)
	counts := func() map[string]float64 {
		out := map[string]float64{}
		applies := applier.Collectors()[0].(*prometheus.CounterVec)
		for _, r := range []string{inventory.ApplyChanged, inventory.ApplyUnchanged, inventory.ApplyInvalid, inventory.ApplyError} {
			out[r] = testutil.ToFloat64(applies.WithLabelValues(r))
		}
		return out
	}

	if got := applier.Check(ctx); got != inventory.ApplyError {
		t.Fatalf("missing file: %q, want error", got)
	}
	text := shop(t, "watched")
	write(text)
	if got := applier.Check(ctx); got != inventory.ApplyChanged {
		t.Fatalf("first read: %q, want changed", got)
	}
	if got := applier.Check(ctx); got != "" {
		t.Fatalf("same file: %q, want nothing applied", got)
	}
	write("version: 1\nenvironment: {slug: shop-watched}\n")
	if got := applier.Check(ctx); got != inventory.ApplyInvalid {
		t.Fatalf("invalid file: %q, want invalid", got)
	}
	if got := applier.Check(ctx); got != "" {
		t.Fatalf("same invalid file: %q, want nothing applied", got)
	}
	if n := f.count(t, "applications where status = 'active'"); n != 2 {
		t.Fatalf("%d active applications after an invalid file, want the 2 last applied", n)
	}
	write(strings.Replace(text, "owner_team: Web", "owner_team: Web team", 1))
	if got := applier.Check(ctx); got != inventory.ApplyChanged {
		t.Fatalf("changed file: %q, want changed", got)
	}
	if got, want := counts(), map[string]float64{inventory.ApplyChanged: 2, inventory.ApplyUnchanged: 0, inventory.ApplyInvalid: 1, inventory.ApplyError: 1}; !maps.Equal(got, want) {
		t.Fatalf("counts %v, want %v", got, want)
	}
	var actor string
	if err := f.pool.QueryRow(ctx, "select record->'actor'->>'file' from audit_outbox order by seq desc limit 1").Scan(&actor); err != nil || actor != path {
		t.Fatalf("last record's actor file %q, %v; want %s", actor, err, path)
	}
}
