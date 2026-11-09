package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRuntimeConfigFiltersEnabledSQLServerCollectors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "integrations.yaml")
	content := []byte(`dashboards:
  base_url: http://grafana:3000
  dashboard_templates:
    logs: /explore
logs:
  base_url: http://loki:3100
alerting:
  base_url: http://alertmanager:9093
collectors:
  - id: sql-prod
    kind: sqlserver
    enabled: true
    credential_ref: kv/sql-prod
    config:
      environment: prod
      scrape_interval: 30s
      target_names: [core-db]
      probes:
        - name: waits
        - name: sessions
      targets:
        - name: core-db
          host: sql.prod.local
          port: 1433
          database_name: Atrium
          credential_ref: kv/core-db
  - id: otel-sidecar
    kind: otel
    enabled: true
    config: {}
  - id: sql-disabled
    kind: sqlserver
    enabled: false
    credential_ref: kv/sql-disabled
    config:
      environment: prod
`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadRuntimeConfig(path)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig returned error: %v", err)
	}

	collectors := cfg.EnabledCollectors("sqlserver")
	if len(collectors) != 1 {
		t.Fatalf("expected 1 enabled sqlserver collector, got %d", len(collectors))
	}
	if collectors[0].ID != "sql-prod" {
		t.Fatalf("unexpected collector id: %s", collectors[0].ID)
	}
	if collectors[0].Environment != "prod" {
		t.Fatalf("unexpected environment filter: %s", collectors[0].Environment)
	}
	if collectors[0].ScrapeInterval.String() != "30s" {
		t.Fatalf("unexpected scrape interval: %s", collectors[0].ScrapeInterval)
	}
	if len(collectors[0].Targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(collectors[0].Targets))
	}
	target := collectors[0].Targets[0]
	if target.Name != "core-db" {
		t.Fatalf("unexpected target name: %s", target.Name)
	}
	if target.EnvironmentSlug != "prod" {
		t.Fatalf("unexpected target environment: %s", target.EnvironmentSlug)
	}
	if len(target.Probes) != 2 {
		t.Fatalf("expected target to inherit 2 probes, got %d", len(target.Probes))
	}
}

func TestConfigVersionStableForEquivalentContent(t *testing.T) {
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.yaml")
	pathB := filepath.Join(dir, "b.yaml")
	content := []byte(`dashboards:
  base_url: http://grafana:3000
  dashboard_templates: {}
logs:
  base_url: http://loki:3100
alerting:
  base_url: http://alertmanager:9093
collectors:
  - id: sql-prod
    kind: sqlserver
    enabled: true
    credential_ref: kv/sql-prod
    config:
      environment: prod
      scrape_interval: 30s
      probes:
        - name: waits
`)
	if err := os.WriteFile(pathA, content, 0o600); err != nil {
		t.Fatalf("write config a: %v", err)
	}
	if err := os.WriteFile(pathB, content, 0o600); err != nil {
		t.Fatalf("write config b: %v", err)
	}

	cfgA, err := LoadRuntimeConfig(pathA)
	if err != nil {
		t.Fatalf("load config a: %v", err)
	}
	cfgB, err := LoadRuntimeConfig(pathB)
	if err != nil {
		t.Fatalf("load config b: %v", err)
	}
	if cfgA.Version != cfgB.Version {
		t.Fatalf("expected equal config versions, got %q and %q", cfgA.Version, cfgB.Version)
	}
}
