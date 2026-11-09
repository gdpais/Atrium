package app

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	"atrium/services/db-collector/internal/collectors"
	connector "atrium/services/db-collector/internal/connectors/sqlserver"
)

// repoRoot is the repository root relative to this package's directory.
const repoRoot = "../../../.."

// collectorReferenceFiles are the files that name collector self-metrics:
// the Prometheus rules and their tests, the Grafana dashboards, the
// end-to-end checks and the docs.
var collectorReferenceFiles = []string{
	"infra/helm/atrium/files/prometheus/rules/*.yml",
	"infra/helm/atrium/files/prometheus/rules/tests/*.yml",
	"infra/helm/atrium/files/grafana/dashboards/*.json",
	"scripts/*.sh",
	"docs/reference/*.md",
	"docs/architecture/*.md",
	"docs/guides/*.md",
	"services/db-collector/README.md",
}

// collectorMetricRef matches a collector self-metric name. A name preceded
// by ^ (a grep pattern) or followed by * (a glob) is a pattern, not a name.
var collectorMetricRef = regexp.MustCompile(`(\^)?\batrium_collector_[a-z0-9_]+(\*)?`)

// collectorMetrics returns every collector self-metric name, with _bucket,
// _sum and _count for the histogram.
func collectorMetrics() map[string]bool {
	names := map[string]bool{}
	for _, name := range []string{
		collectors.MetricTargetUp, collectors.MetricTargetLastSuccess, collectors.MetricTargetConsecutiveFailures,
		collectors.MetricCycleDuration, collectors.MetricTargetLoginSysadmin, collectors.MetricProbeErrors, collectors.MetricProbeUp,
		MetricConfigLastReloadSuccessful, MetricConfigLastReloadSuccessTimestamp, MetricConfigReloads, MetricConfigInfo,
		collectors.MetricCycles,
		connector.MetricConnections, connector.MetricConnectionWaits, connector.MetricConnectionWaitSeconds,
		MetricHTTPRequests,
	} {
		names[name] = true
	}
	for _, suffix := range []string{"", "_bucket", "_sum", "_count"} {
		for _, histogram := range []string{collectors.MetricProbeDuration, collectors.MetricCycleSeconds, collectors.MetricCycleStartDelay, MetricHTTPRequestDuration} {
			names[histogram+suffix] = true
		}
	}
	return names
}

// Rules, alerts, dashboards, the end-to-end checks and the docs name
// collector self-metrics, so a renamed or misspelled one leaves an alert
// that never fires or a doc that sends operators to nothing. Every
// atrium_collector_* name they reference must be one the collector
// exports.
func TestReferencesNameCollectorMetrics(t *testing.T) {
	known := collectorMetrics()
	for _, pattern := range collectorReferenceFiles {
		files, err := filepath.Glob(filepath.Join(repoRoot, pattern))
		if err != nil {
			t.Fatal(err)
		}
		if len(files) == 0 {
			t.Fatalf("no files match %s; update the patterns if they moved", pattern)
		}
		for _, file := range files {
			content, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var unknown []string
			for _, match := range collectorMetricRef.FindAllStringSubmatch(string(content), -1) {
				if match[1] != "" || match[2] != "" {
					continue
				}
				if name := match[0]; !known[name] && !slices.Contains(unknown, name) {
					unknown = append(unknown, name)
				}
			}
			if len(unknown) > 0 {
				rel, _ := filepath.Rel(repoRoot, file)
				t.Errorf("%s references collector metrics the collector does not export: %v", rel, unknown)
			}
		}
	}
}
