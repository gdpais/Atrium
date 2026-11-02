package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// These are safety boundaries: a future fixture edit must not give disposable
// tests access to persistent developer data or globally named Docker resources.
func TestPostgresFixtureIsDisposable(t *testing.T) {
	content, err := os.ReadFile(filepath.Join(repoRoot(t), "infra/docker-compose.test.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Name     string         `yaml:"name"`
		Volumes  map[string]any `yaml:"volumes"`
		Networks map[string]any `yaml:"networks"`
		Services map[string]struct {
			ContainerName string   `yaml:"container_name"`
			Ports         []any    `yaml:"ports"`
			NetworkMode   string   `yaml:"network_mode"`
			Tmpfs         []string `yaml:"tmpfs"`
			Volumes       []string `yaml:"volumes"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(content, &fixture); err != nil {
		t.Fatal(err)
	}
	pg, ok := fixture.Services["postgres"]
	if !ok || len(fixture.Services) != 1 || fixture.Name != "" || len(fixture.Volumes) != 0 || len(fixture.Networks) != 0 {
		t.Fatal("test fixture must contain only PostgreSQL and no shared resources")
	}
	if pg.ContainerName != "" || pg.NetworkMode != "" || len(pg.Ports) != 0 {
		t.Fatal("test PostgreSQL must not use globally named containers or host networking/ports")
	}
	if len(pg.Tmpfs) != 1 || pg.Tmpfs[0] != "/var/lib/postgresql/data" {
		t.Fatal("test database storage must be disposable tmpfs")
	}
	if len(pg.Volumes) != 1 || pg.Volumes[0] != "../db/migrations:/migrations:ro" {
		t.Fatal("the only permitted test bind mount is read-only migrations")
	}
}

func TestRequiredFoundationFilesExist(t *testing.T) {
	root := repoRoot(t)
	required := []string{
		"docs/README.md",
		"docs/product/overview.md",
		"docs/product/roadmap.md",
		"docs/architecture/overview.md",
		"docs/architecture/data-model.md",
		"docs/architecture/database-observability.md",
		"docs/architecture/workflows.md",
		"docs/guides/local-development.md",
		"docs/reference/configuration.md",
		"infra/helm/heartbeat/Chart.yaml",
		"infra/helm/heartbeat/Chart.lock",
		"infra/helm/heartbeat/values.yaml",
		"infra/helm/heartbeat/values.schema.json",
		"infra/helm/values/kind.yaml",
		"infra/helm/values/production.example.yaml",
		"infra/kind/cluster.yaml",
		"packages/config-schema/src/integrations.schema.json",
		"packages/telemetry-contracts/src/application_event.schema.json",
		"packages/telemetry-contracts/src/session_investigation.schema.json",
		"packages/telemetry-contracts/src/alert_evidence.schema.json",
		"packages/telemetry-contracts/src/report_payload.schema.json",
		"packages/telemetry-contracts/src/audit_record.schema.json",
		"db/migrations/0001_foundations.up.sql",
		"db/migrations/0001_foundations.down.sql",
		"db/migrations/0002_audit_outbox.up.sql",
		"db/migrations/0002_audit_outbox.down.sql",
	}

	for _, rel := range required {
		t.Run(rel, func(t *testing.T) {
			mustExist(t, filepath.Join(root, rel))
		})
	}
}

func TestMonorepoScaffoldingExists(t *testing.T) {
	root := repoRoot(t)
	required := []string{
		"apps/api/cmd/api",
		"apps/api/internal",
		"apps/web",
		"services/db-collector/cmd/db-collector",
		"services/db-collector/internal",
		"services/session-analyzer/cmd/session-analyzer",
		"services/session-analyzer/internal",
		"services/reporting/cmd/reporting",
		"services/reporting/internal",
		"infra/helm/heartbeat/templates",
		"infra/helm/heartbeat/files/prometheus/rules",
		"infra/helm/heartbeat/files/grafana/dashboards",
		"infra/helm/values",
		"infra/kind",
		"tests/integration",
		"tests/e2e",
	}

	for _, rel := range required {
		t.Run(rel, func(t *testing.T) {
			mustExist(t, filepath.Join(root, rel))
		})
	}
}

func TestSchemaContractsAreValidJSON(t *testing.T) {
	root := repoRoot(t)
	schemas := []string{
		"packages/config-schema/src/integrations.schema.json",
		"packages/telemetry-contracts/src/application_event.schema.json",
		"packages/telemetry-contracts/src/session_investigation.schema.json",
		"packages/telemetry-contracts/src/alert_evidence.schema.json",
		"packages/telemetry-contracts/src/report_payload.schema.json",
		"packages/telemetry-contracts/src/audit_record.schema.json",
	}

	for _, rel := range schemas {
		t.Run(rel, func(t *testing.T) {
			schema := mustReadJSONSchema(t, filepath.Join(root, rel))
			if schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
				t.Fatalf("unexpected $schema in %s: %#v", rel, schema["$schema"])
			}
			if _, ok := schema["title"]; !ok {
				t.Fatalf("missing title in %s", rel)
			}
			if _, ok := schema["type"]; !ok {
				t.Fatalf("missing type in %s", rel)
			}
		})
	}
}

func TestMigrationContainsCoreTablesAndExcludesNonGoals(t *testing.T) {
	sql := migrationSQL(t)
	expected := []string{
		"create table users",
		"create table roles",
		"create table user_roles",
		"create table environments",
		"create table applications",
		"create table application_components",
		"create table telemetry_sources",
		"create table normalization_rules",
		"create table database_targets",
		"create table probe_definitions",
		"create table probe_assignments",
		"create table investigations",
		"create table investigation_jobs",
		"create table investigation_results",
		"create table evidence_links",
		"create table alert_policies",
		"create table adaptive_baselines",
		"create table notification_routes",
		"create table alert_events",
		"create table report_templates",
		"create table report_schedules",
		"create table report_runs",
	}
	for _, fragment := range expected {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("migration missing fragment %q", fragment)
		}
	}

	forbidden := []string{
		"create table outsystems_sources",
		"create table session_identity_mappings",
		"create table integration_connections",
		"create table grafana_links",
		"create table audit_events",
		"create table collector_instances",
		"create table collector_assignments",
		"create table assets",
		"create table asset_relationships",
	}
	for _, fragment := range forbidden {
		if strings.Contains(sql, fragment) {
			t.Fatalf("migration unexpectedly contains fragment %q", fragment)
		}
	}
}

func TestMigrationHasExpectedUniquesAndIndexes(t *testing.T) {
	sql := migrationSQL(t)
	expected := []string{
		"unique (user_id, role_id)",
		"unique (environment_id, name)",
		"unique (application_id, name, component_type)",
		"unique (source_type, version)",
		"create unique index database_targets_identity_uq on database_targets (environment_id, engine, host, port, coalesce(database_name, ''))",
		"unique (engine, name, version)",
		"unique (database_target_id, probe_definition_id)",
		"unique (application_id, name)",
		"unique (application_id, signal_key, \"window\", method)",
		"unique (application_id, fingerprint, started_at)",
		"unique (application_id, report_template_id, cron_expression, timezone)",
		"create unique index environments_slug_uq",
		"create index investigations_environment_created_at_idx",
		"create index investigations_subject_lookup_idx",
		"create index alert_events_fingerprint_started_at_idx",
	}
	for _, fragment := range expected {
		if !strings.Contains(sql, fragment) {
			t.Fatalf("migration missing fragment %q", fragment)
		}
	}
}

func TestMigrationFilesUseSQLExtension(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{
		"db/migrations/0001_foundations.up.sql",
		"db/migrations/0001_foundations.down.sql",
		"db/migrations/0002_audit_outbox.up.sql",
		"db/migrations/0002_audit_outbox.down.sql",
	} {
		if filepath.Ext(rel) != ".sql" {
			t.Fatalf("expected .sql file extension for %s", rel)
		}
		mustExist(t, filepath.Join(root, rel))
	}
}
