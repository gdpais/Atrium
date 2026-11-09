package tests

import (
	"path/filepath"
	"testing"

	"atrium/internal/config"
)

// The schema's timeout_ms maximum must match the limit the services enforce,
// or Helm would accept a probe timeout the collector rejects (or the reverse).
func TestIntegrationsSchemaProbeTimeoutMatchesConfigLimit(t *testing.T) {
	schema := mustReadJSONSchema(t, filepath.Join(repoRoot(t), "packages/config-schema/src/integrations.schema.json"))
	defs, _ := schema["$defs"].(map[string]any)
	probe, _ := defs["probe"].(map[string]any)
	properties, _ := probe["properties"].(map[string]any)
	timeout, ok := properties["timeout_ms"].(map[string]any)
	if !ok {
		t.Fatal("integrations schema has no $defs.probe.properties.timeout_ms")
	}
	maximum, ok := timeout["maximum"].(float64)
	if !ok {
		t.Fatal("integrations schema timeout_ms has no maximum")
	}
	if want := config.MaxProbeTimeout.Milliseconds(); int64(maximum) != want {
		t.Fatalf("schema timeout_ms maximum = %v, want %d (config.MaxProbeTimeout)", maximum, want)
	}
}

// The schema's batch_limit and max_derived_series maximums must match the
// limits the collector enforces, for the same reason.
func TestIntegrationsSchemaCollectorLimitsMatchConfigLimits(t *testing.T) {
	schema := mustReadJSONSchema(t, filepath.Join(repoRoot(t), "packages/config-schema/src/integrations.schema.json"))
	collectors, _ := schema["properties"].(map[string]any)["collectors"].(map[string]any)
	items, _ := collectors["items"].(map[string]any)
	collectorConfig, _ := items["properties"].(map[string]any)["config"].(map[string]any)
	for _, limit := range []struct {
		name string
		max  int
	}{
		{"batch_limit", config.MaxLogBatchLimit},
		{"max_derived_series", config.MaxLogDerivedSeries},
	} {
		setting, ok := collectorConfig["properties"].(map[string]any)[limit.name].(map[string]any)
		if !ok {
			t.Fatalf("integrations schema has no collector config %s", limit.name)
		}
		if maximum, _ := setting["maximum"].(float64); int(maximum) != limit.max {
			t.Fatalf("schema %s maximum = %v, want %d", limit.name, setting["maximum"], limit.max)
		}
	}
}

// The schema's max_rows maximum must match the limit the collector enforces,
// for the same reason.
func TestIntegrationsSchemaProbeMaxRowsMatchesConfigLimit(t *testing.T) {
	schema := mustReadJSONSchema(t, filepath.Join(repoRoot(t), "packages/config-schema/src/integrations.schema.json"))
	defs, _ := schema["$defs"].(map[string]any)
	probe, _ := defs["probe"].(map[string]any)
	properties, _ := probe["properties"].(map[string]any)
	maxRows, ok := properties["max_rows"].(map[string]any)
	if !ok {
		t.Fatal("integrations schema has no $defs.probe.properties.max_rows")
	}
	if maximum, _ := maxRows["maximum"].(float64); int(maximum) != config.MaxProbeMaxRows {
		t.Fatalf("schema max_rows maximum = %v, want %d (config.MaxProbeMaxRows)", maxRows["maximum"], config.MaxProbeMaxRows)
	}
}
