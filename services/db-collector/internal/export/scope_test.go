package export

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

const blockedMetric = "atrium_sqlserver_blocked_requests"

func blocked(target, session string, value float64) Sample {
	return Sample{
		Metric: blockedMetric,
		Help:   "Blocked requests.",
		Value:  value,
		Labels: map[string]string{"environment": "prod", "target": target, "blocking_session_id": session},
	}
}

// exported returns the blocking_session_id label of every exported series of
// blockedMetric, keyed by target.
func exported(t *testing.T, reg *prometheus.Registry) map[string][]string {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	out := map[string][]string{}
	for _, family := range families {
		if family.GetName() != blockedMetric {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, pair := range metric.GetLabel() {
				labels[pair.GetName()] = pair.GetValue()
			}
			out[labels["target"]] = append(out[labels["target"]], labels["blocking_session_id"])
		}
	}
	return out
}

func TestRecordScopeDeletesSeriesNoLongerReported(t *testing.T) {
	reg := prometheus.NewRegistry()
	exporter := NewPrometheusExporter(reg)
	scope := Scope{Collector: "sql-prod", Target: "core-db", Probe: "blocking"}

	if err := exporter.RecordScope(scope, []Sample{blocked("core-db", "57", 3), blocked("core-db", "58", 1)}); err != nil {
		t.Fatalf("RecordScope: %v", err)
	}
	if got := exported(t, reg)["core-db"]; len(got) != 2 {
		t.Fatalf("expected 2 series, got %v", got)
	}
	if err := exporter.RecordScope(scope, []Sample{blocked("core-db", "58", 2)}); err != nil {
		t.Fatalf("RecordScope: %v", err)
	}
	if got := exported(t, reg)["core-db"]; len(got) != 1 || got[0] != "58" {
		t.Fatalf("expected only session 58, got %v", got)
	}
	if value, ok := gathered(t, reg, blockedMetric, map[string]string{"blocking_session_id": "58", "environment": "prod", "target": "core-db"}); !ok || value != 2 {
		t.Fatalf("unexpected value %v", value)
	}
	// Block cleared: the probe returns zero rows.
	if err := exporter.RecordScope(scope, nil); err != nil {
		t.Fatalf("RecordScope: %v", err)
	}
	if got := exported(t, reg)["core-db"]; len(got) != 0 {
		t.Fatalf("expected no series after empty result, got %v", got)
	}
}

func TestClearScopeAndForgetCollector(t *testing.T) {
	reg := prometheus.NewRegistry()
	exporter := NewPrometheusExporter(reg)
	a := Scope{Collector: "c1", Target: "db-a", Probe: "blocking"}
	b := Scope{Collector: "c1", Target: "db-b", Probe: "blocking"}
	other := Scope{Collector: "c2", Target: "db-c", Probe: "blocking"}
	for scope, sample := range map[Scope]Sample{a: blocked("db-a", "1", 1), b: blocked("db-b", "2", 1), other: blocked("db-c", "3", 1)} {
		if err := exporter.RecordScope(scope, []Sample{sample}); err != nil {
			t.Fatalf("RecordScope: %v", err)
		}
	}
	// Unscoped series are not owned by any scope, so scope cleanup keeps them.
	if err := exporter.Record([]Sample{blocked("legacy", "9", 1)}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	exporter.ClearScope(a)
	got := exported(t, reg)
	if len(got["db-a"]) != 0 || len(got["db-b"]) != 1 || len(got["db-c"]) != 1 {
		t.Fatalf("ClearScope removed the wrong series: %v", got)
	}

	exporter.ForgetCollector("c1")
	got = exported(t, reg)
	if len(got["db-b"]) != 0 || len(got["db-c"]) != 1 || len(got["legacy"]) != 1 {
		t.Fatalf("ForgetCollector removed the wrong series: %v", got)
	}

	// Scopes can be reused after being forgotten.
	if err := exporter.RecordScope(a, []Sample{blocked("db-a", "4", 1)}); err != nil {
		t.Fatalf("RecordScope: %v", err)
	}
	if got := exported(t, reg)["db-a"]; len(got) != 1 || got[0] != "4" {
		t.Fatalf("expected re-recorded series, got %v", got)
	}
}

func TestSharedSeriesSurvivesUntilLastOwnerDrops(t *testing.T) {
	reg := prometheus.NewRegistry()
	exporter := NewPrometheusExporter(reg)
	first := Scope{Collector: "c1", Target: "db", Probe: "p1"}
	second := Scope{Collector: "c1", Target: "db", Probe: "p2"}
	shared := blocked("db", "1", 1)
	for _, scope := range []Scope{first, second} {
		if err := exporter.RecordScope(scope, []Sample{shared}); err != nil {
			t.Fatalf("RecordScope: %v", err)
		}
	}
	exporter.ClearScope(first)
	if got := exported(t, reg)["db"]; len(got) != 1 {
		t.Fatalf("shared series deleted while still owned: %v", got)
	}
	exporter.ClearScope(second)
	if got := exported(t, reg)["db"]; len(got) != 0 {
		t.Fatalf("shared series survived its last owner: %v", got)
	}
}

func TestRecordScopeRecordsValidSamplesDespiteErrors(t *testing.T) {
	reg := prometheus.NewRegistry()
	exporter := NewPrometheusExporter(reg)
	scope := Scope{Collector: "c1", Target: "db", Probe: "blocking"}
	bad := Sample{Metric: blockedMetric, Value: 1, Labels: map[string]string{"target": "db"}}
	if err := exporter.RecordScope(scope, []Sample{blocked("db", "1", 1)}); err != nil {
		t.Fatalf("RecordScope: %v", err)
	}
	err := exporter.RecordScope(scope, []Sample{bad, blocked("db", "2", 1)})
	if err == nil || !strings.Contains(err.Error(), "label set changed") {
		t.Fatalf("expected label set error, got %v", err)
	}
	if got := exported(t, reg)["db"]; len(got) != 1 || got[0] != "2" {
		t.Fatalf("expected only the valid new series, got %v", got)
	}
}

func TestScopedExporterIsConcurrencySafe(t *testing.T) {
	reg := prometheus.NewRegistry()
	exporter := NewPrometheusExporter(reg)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			target := fmt.Sprintf("db-%d", i)
			scope := Scope{Collector: "c1", Target: target, Probe: "blocking"}
			for j := range 50 {
				_ = exporter.RecordScope(scope, []Sample{blocked(target, fmt.Sprint(j), 1)})
				if j%10 == 0 {
					exporter.ClearScope(scope)
				}
			}
		}()
	}
	wg.Wait()
	got := exported(t, reg)
	for i := range 16 {
		if series := got[fmt.Sprintf("db-%d", i)]; len(series) != 1 || series[0] != "49" {
			t.Fatalf("db-%d: expected only the latest series, got %v", i, series)
		}
	}
	exporter.ForgetCollector("c1")
	if got := exported(t, reg); len(got) != 0 {
		t.Fatalf("expected no series after ForgetCollector, got %v", got)
	}
}

func TestInMemoryExporterScopes(t *testing.T) {
	exporter := NewInMemoryExporter()
	scope := Scope{Collector: "c1", Target: "db", Probe: "blocking"}
	first, second := blocked("db", "1", 5), blocked("db", "2", 6)
	if err := exporter.RecordScope(scope, []Sample{first, second}); err != nil {
		t.Fatalf("RecordScope: %v", err)
	}
	if err := exporter.RecordScope(scope, []Sample{second}); err != nil {
		t.Fatalf("RecordScope: %v", err)
	}
	if _, ok := exporter.Value(first.Metric, first.Labels); ok {
		t.Fatalf("stale series survived")
	}
	if value, ok := exporter.Value(second.Metric, second.Labels); !ok || value != 6 {
		t.Fatalf("expected current series, got %v %v", value, ok)
	}
	exporter.ForgetCollector("c1")
	if _, ok := exporter.Value(second.Metric, second.Labels); ok {
		t.Fatalf("series survived ForgetCollector")
	}
}

func TestRecordScopeReportsDuplicateSeries(t *testing.T) {
	for name, exporter := range map[string]ScopedRecorder{
		"prometheus": NewPrometheusExporter(prometheus.NewRegistry()),
		"in-memory":  NewInMemoryExporter(),
	} {
		t.Run(name, func(t *testing.T) {
			scope := Scope{Collector: "sql-prod", Target: "core-db", Probe: "blocking"}
			err := exporter.RecordScope(scope, []Sample{blocked("core-db", "57", 1), blocked("core-db", "57", 2)})
			if err == nil || !strings.Contains(err.Error(), "duplicate series") {
				t.Fatalf("expected duplicate series error, got %v", err)
			}
			if err := exporter.RecordScope(scope, []Sample{blocked("core-db", "57", 1), blocked("core-db", "58", 2)}); err != nil {
				t.Fatalf("distinct series must not error: %v", err)
			}
		})
	}
}

// Two collectors scraping one target would take turns on its counters, and
// every drop would read as a reset to rate().  Gauges may be shared.
func TestCounterSeriesHaveASingleOwner(t *testing.T) {
	sample := func(metricType MetricType, value float64) Sample {
		name := "atrium_test_gauge"
		if metricType == Counter {
			name = "atrium_test_total"
		}
		return Sample{Metric: name, Type: metricType, Value: value, Labels: map[string]string{"environment": "prod", "target": "core-db"}}
	}
	first := Scope{Collector: "a", Target: "core-db", Probe: "waits"}
	second := Scope{Collector: "b", Target: "core-db", Probe: "waits"}
	tests := []struct {
		name      string
		metric    MetricType
		wantErr   bool
		wantValue float64
	}{
		{"counter keeps the owner's value", Counter, true, 100},
		{"gauge is shared, last writer wins", Gauge, false, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			exporter := NewPrometheusExporter(reg)
			owned := sample(tt.metric, 100)
			if err := exporter.RecordScope(first, []Sample{owned}); err != nil {
				t.Fatalf("RecordScope(first): %v", err)
			}
			err := exporter.RecordScope(second, []Sample{sample(tt.metric, 5)})
			if tt.wantErr != (err != nil) || (err != nil && !strings.Contains(err.Error(), "already reported by another scope")) {
				t.Fatalf("RecordScope(second): wantErr %v, got %v", tt.wantErr, err)
			}
			if value, ok := gathered(t, reg, owned.Metric, owned.Labels); !ok || value != tt.wantValue {
				t.Fatalf("exported %v %v, want %v", value, ok, tt.wantValue)
			}
			// The owner keeps writing as usual.
			if err := exporter.RecordScope(first, []Sample{sample(tt.metric, 110)}); err != nil {
				t.Fatalf("owner rewrite: %v", err)
			}
		})
	}

	t.Run("counter is free again once its owner drops it", func(t *testing.T) {
		reg := prometheus.NewRegistry()
		exporter := NewPrometheusExporter(reg)
		if err := exporter.RecordScope(first, []Sample{sample(Counter, 100)}); err != nil {
			t.Fatalf("RecordScope(first): %v", err)
		}
		if err := exporter.Record([]Sample{sample(Counter, 1)}); err == nil {
			t.Fatal("Record must not overwrite an owned counter")
		}
		exporter.ForgetCollector("a")
		if err := exporter.RecordScope(second, []Sample{sample(Counter, 7)}); err != nil {
			t.Fatalf("RecordScope(second) after the owner left: %v", err)
		}
		if value, ok := gathered(t, reg, "atrium_test_total", sample(Counter, 0).Labels); !ok || value != 7 {
			t.Fatalf("exported %v %v, want 7", value, ok)
		}
	})
}
