package outsystems

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	collectorconfig "heartbeat/internal/config"
	collectormetadata "heartbeat/services/db-collector/internal/metadata"
)

// repoRoot is the repository root relative to this package's directory.
const repoRoot = "../../../../.."

// referenceFilePatterns are the files that name OutSystems metrics: the
// Prometheus rules and their tests, the Grafana dashboards, the end-to-end
// checks and the docs.
var referenceFilePatterns = []string{
	"infra/helm/heartbeat/files/prometheus/rules/*.yml",
	"infra/helm/heartbeat/files/prometheus/rules/tests/*.yml",
	"infra/helm/heartbeat/files/grafana/dashboards/*.json",
	"scripts/*.sh",
	"docs/reference/*.md",
	"docs/architecture/*.md",
	"docs/guides/*.md",
}

var outsystemsMetricRef = regexp.MustCompile(`\bheartbeat_outsystems_[a-z0-9_]+`)

// registeredMetrics returns the name of every series family Metrics exports,
// with _bucket, _sum and _count for histograms, after touching each metric.
func registeredMetrics(t *testing.T) map[string]bool {
	t.Helper()
	registry := prometheus.NewRegistry()
	metrics, err := NewMetrics(registry)
	if err != nil {
		t.Fatal(err)
	}
	metrics.initCollector(collectorconfig.CollectorRuntimeConfig{ID: "c", Enabled: true, Targets: []collectorconfig.TargetRuntimeConfig{
		{Name: "t", Probes: []collectorconfig.ProbeRuntimeConfig{{Name: "screen"}}},
	}})
	item := collectormetadata.ScheduledProbe{CollectorID: "c", Definition: collectormetadata.ProbeDefinition{Name: "screen"}}
	metrics.addRows(item, 1)
	metrics.setLag(item, time.Second)
	metrics.exportFailed(item)
	metrics.batchStuck(item)
	metrics.setLogDelay(item, time.Second)
	metrics.setExportBlocked(item, true)
	metrics.recounted(item, 1, 1)
	metrics.recountRan(item)
	metrics.recountFailed(item)
	metrics.observeStage(item, stageRead, time.Now())
	observe := func(logType string, columns []string, row ...any) {
		batch := Batch{Rows: [][]any{row}}
		for _, name := range columns {
			batch.Columns = append(batch.Columns, Column{Name: name})
		}
		item := item
		item.Definition.Name = logType
		metrics.observeRows(item, batch, batch.Rows, []time.Time{t0})
	}
	observe("screen", []string{"Duration", "Server", "Session_Bytes", "Viewstate_Bytes"}, int64(1), "FE-01", int64(1), int64(1))
	observe("integration", []string{"Duration", "Is_Expose"}, int64(1), false)
	observe("cyclic_job", []string{"Should_Have_Run_At"}, t0)
	observe("error", nil)
	observe("general", []string{"Module_Name"}, "SLOWSQL")

	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, family := range families {
		names[family.GetName()] = true
		if family.GetType() == dto.MetricType_HISTOGRAM {
			for _, suffix := range []string{"_bucket", "_sum", "_count"} {
				names[family.GetName()+suffix] = true
			}
		}
	}
	return names
}

// Every metric the package declares is registered and reachable.
func TestEveryDeclaredMetricIsExported(t *testing.T) {
	names := registeredMetrics(t)
	for _, name := range []string{
		MetricRows, MetricLag, MetricLagSetting, MetricExportFailures, MetricStuckBatches, MetricLogDelay, MetricExportBlocked,
		MetricOperationDuration, MetricOperations, MetricOperationSeconds, MetricIntegrationDuration,
		MetricErrors, MetricSlowEvents, MetricTimerDelay, MetricScreenState, MetricFrontEndOperations,
		MetricDerivedSeries, MetricDerivedSeriesOverflow, MetricStageDuration, MetricHeldBytes,
	} {
		if !names[name] {
			t.Errorf("%s is declared but not exported", name)
		}
	}
}

// Rules, dashboards, the end-to-end checks and the docs name OutSystems
// metrics, so a renamed or unbuilt metric leaves them silently empty.  Every
// OutSystems metric they reference must be exported.
func TestReferencesNameExportedMetrics(t *testing.T) {
	exported := registeredMetrics(t)
	var files []string
	for _, pattern := range referenceFilePatterns {
		matches, err := filepath.Glob(filepath.Join(repoRoot, pattern))
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) == 0 {
			t.Fatalf("no files match %s; update the patterns if they moved", pattern)
		}
		files = append(files, matches...)
	}
	for _, file := range files {
		content, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var unknown []string
		for _, name := range outsystemsMetricRef.FindAllString(string(content), -1) {
			if !exported[name] && !slices.Contains(unknown, name) {
				unknown = append(unknown, name)
			}
		}
		if len(unknown) > 0 {
			sort.Strings(unknown)
			rel, _ := filepath.Rel(repoRoot, file)
			t.Errorf("%s references OutSystems metrics the collector does not export: %s", rel, strings.Join(unknown, ", "))
		}
	}
}
