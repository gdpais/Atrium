package outsystems

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var (
	// le="3" or le=~"3(\\.0)?" on a latency histogram's buckets.
	latencyBucketRef = regexp.MustCompile(`atrium_outsystems_(?:operation|integration)_duration_seconds_bucket\{[^}]*\ble=~?"([0-9.]+)`)
	// A Loki filter on the rows' Duration, in milliseconds.
	durationFilterRef = regexp.MustCompile(`\bDuration\s*(?:>|>=|<|<=)\s*([0-9]+)\b`)
)

// The dashboards' latency thresholds (Apdex's 3 s and 10 s, the 2 s SLO)
// are read from the histogram's buckets, and the panels that list the slow
// requests from Loki use the same ones, so the two agree. A threshold that
// is not a bucket edge is only an estimate on the metric panels; change the
// buckets and the dashboards together.
func TestDashboardLatencyThresholdsAreBucketEdges(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(repoRoot, "infra/helm/atrium/files/grafana/dashboards/*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no dashboards: %v", err)
	}
	isEdge := func(seconds float64) bool { return slices.Contains(latencyBuckets, seconds) }
	checked := 0
	for _, file := range files {
		content, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var exprs []string
		collectExprs(t, content, &exprs)
		name := filepath.Base(file)
		for _, expr := range exprs {
			for _, m := range latencyBucketRef.FindAllStringSubmatch(expr, -1) {
				v, err := strconv.ParseFloat(strings.TrimSuffix(m[1], "."), 64)
				if err != nil || !isEdge(v) {
					t.Errorf("%s reads bucket le=%s, which is not one of the latency buckets %v", name, m[1], latencyBuckets)
				}
				checked++
			}
			if !strings.Contains(expr, "outsystems_log_type") {
				continue
			}
			for _, m := range durationFilterRef.FindAllStringSubmatch(expr, -1) {
				ms, _ := strconv.Atoi(m[1])
				if ms == 0 {
					continue // Duration >= 0: the row is timed
				}
				if !isEdge(float64(ms) / 1000) {
					t.Errorf("%s filters Duration at %d ms, which is not one of the latency buckets %v (seconds)", name, ms, latencyBuckets)
				}
				checked++
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no latency thresholds in the dashboards; update the patterns if the queries changed")
	}
}

// collectExprs appends every "expr" string in a dashboard's JSON.
func collectExprs(t *testing.T, content []byte, out *[]string) {
	t.Helper()
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, child := range v {
				if s, ok := child.(string); ok && k == "expr" {
					*out = append(*out, s)
					continue
				}
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	var doc any
	if err := json.Unmarshal(content, &doc); err != nil {
		t.Fatal(err)
	}
	walk(doc)
}
