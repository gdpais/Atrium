package export

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// gathered returns the value of the series of metric with exactly labels,
// whether it is a gauge or a counter, and whether that series is exported.
func gathered(t *testing.T, reg *prometheus.Registry, metric string, labels map[string]string) (float64, bool) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, family := range families {
		if family.GetName() != metric {
			continue
		}
		for _, m := range family.GetMetric() {
			got := map[string]string{}
			for _, pair := range m.GetLabel() {
				got[pair.GetName()] = pair.GetValue()
			}
			if fmt.Sprint(got) != fmt.Sprint(labels) {
				continue
			}
			if m.GetCounter() != nil {
				return m.GetCounter().GetValue(), true
			}
			return m.GetGauge().GetValue(), true
		}
	}
	return 0, false
}

func TestExporterRecordsSamples(t *testing.T) {
	reg := prometheus.NewRegistry()
	exporter := NewPrometheusExporter(reg)
	labels := map[string]string{"environment": "prod", "target": "core-db", "wait_type": "LCK_M_X"}
	if err := exporter.Record([]Sample{{Metric: "atrium_sqlserver_wait_seconds_total", Type: Counter, Value: 42, Labels: labels}}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if value, ok := gathered(t, reg, "atrium_sqlserver_wait_seconds_total", labels); !ok || value != 42 {
		t.Fatalf("unexpected metric value: %v %v", value, ok)
	}
}

// The TYPE line is what Prometheus and Grafana use to pick rate() versus raw
// values, so check the text exposition, not just the stored value.
func TestExporterExposesMetricType(t *testing.T) {
	tests := []struct {
		name   string
		sample Sample
		want   string
	}{
		{
			name:   "zero type is a gauge",
			sample: Sample{Metric: "atrium_test_sessions", Help: "Sessions.", Value: 3, Labels: map[string]string{"target": "db"}},
			want: `# HELP atrium_test_sessions Sessions.
# TYPE atrium_test_sessions gauge
atrium_test_sessions{target="db"} 3
`,
		},
		{
			name:   "counter",
			sample: Sample{Metric: "atrium_test_wait_seconds_total", Help: "Wait time.", Type: Counter, Value: 1.5, Labels: map[string]string{"target": "db"}},
			want: `# HELP atrium_test_wait_seconds_total Wait time.
# TYPE atrium_test_wait_seconds_total counter
atrium_test_wait_seconds_total{target="db"} 1.5
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			exporter := NewPrometheusExporter(reg)
			scope := Scope{Collector: "c", Target: "db", Probe: "p"}
			if err := exporter.RecordScope(scope, []Sample{tt.sample}); err != nil {
				t.Fatalf("RecordScope: %v", err)
			}
			if err := testutil.GatherAndCompare(reg, strings.NewReader(tt.want), tt.sample.Metric); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A SQL Server restart resets its cumulative values.  The exporter must
// export the lower value as is (rate() treats the drop as a counter reset)
// instead of rejecting it or holding the old value.
func TestCounterDropAfterSourceRestartIsExported(t *testing.T) {
	reg := prometheus.NewRegistry()
	exporter := NewPrometheusExporter(reg)
	scope := Scope{Collector: "c", Target: "db", Probe: "waits"}
	labels := map[string]string{"target": "db", "wait_type": "LCK_M_S"}
	for _, value := range []float64{100, 160, 5} {
		sample := Sample{Metric: "atrium_test_wait_seconds_total", Type: Counter, Value: value, Labels: labels}
		if err := exporter.RecordScope(scope, []Sample{sample}); err != nil {
			t.Fatalf("RecordScope(%v): %v", value, err)
		}
		if got, ok := gathered(t, reg, sample.Metric, labels); !ok || got != value {
			t.Fatalf("after recording %v: exported %v %v", value, got, ok)
		}
	}
}

func TestExporterRejectsInvalidSamples(t *testing.T) {
	valid := Sample{Metric: "atrium_test_total", Type: Counter, Value: 1, Labels: map[string]string{"target": "db"}}
	tests := []struct {
		name    string
		sample  Sample
		wantErr string
	}{
		{"type changed", Sample{Metric: valid.Metric, Type: Gauge, Value: 1, Labels: map[string]string{"target": "db"}}, "type changed from counter to gauge"},
		{"negative counter", Sample{Metric: valid.Metric, Type: Counter, Value: -1, Labels: map[string]string{"target": "db"}}, "negative or NaN"},
		{"NaN counter", Sample{Metric: valid.Metric, Type: Counter, Value: math.NaN(), Labels: map[string]string{"target": "db"}}, "negative or NaN"},
		{"missing label", Sample{Metric: valid.Metric, Type: Counter, Value: 1, Labels: map[string]string{}}, "label set changed"},
		{"renamed label", Sample{Metric: valid.Metric, Type: Counter, Value: 1, Labels: map[string]string{"instance": "db"}}, "label set changed"},
		{"invalid UTF-8 label value", Sample{Metric: valid.Metric, Type: Counter, Value: 1, Labels: map[string]string{"target": "db\xff"}}, "not valid UTF-8"},
		{"name registered by another collector", Sample{Metric: "atrium_test_conflict", Value: 1}, "register gauge atrium_test_conflict"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			reg.MustRegister(prometheus.NewCounter(prometheus.CounterOpts{Name: "atrium_test_conflict", Help: "Registered elsewhere."}))
			exporter := NewPrometheusExporter(reg)
			scope := Scope{Collector: "c", Target: "db", Probe: "p"}
			err := exporter.RecordScope(scope, []Sample{valid, tt.sample})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
			// The valid sample of the batch is still recorded, and scrapes
			// still succeed.
			if value, ok := gathered(t, reg, valid.Metric, valid.Labels); !ok || value != 1 {
				t.Fatalf("valid sample not exported: %v %v", value, ok)
			}
		})
	}
}

// Probes report every series every cycle, so recording a known series must
// not allocate.
func TestRecordScopeDoesNotAllocateForExistingSeries(t *testing.T) {
	exporter := NewPrometheusExporter(prometheus.NewRegistry())
	scope := Scope{Collector: "c", Target: "db", Probe: "waits"}
	samples := waitSamples(50)
	if err := exporter.RecordScope(scope, samples); err != nil {
		t.Fatalf("RecordScope: %v", err)
	}
	// A second call grows the scope's spare buffer too.
	_ = exporter.RecordScope(scope, samples)
	allocs := testing.AllocsPerRun(100, func() {
		if err := exporter.RecordScope(scope, samples); err != nil {
			t.Fatalf("RecordScope: %v", err)
		}
	})
	if allocs != 0 {
		t.Fatalf("expected no allocations per RecordScope, got %v", allocs)
	}
}

func TestScrapesDuringRecordingAreConsistent(t *testing.T) {
	reg := prometheus.NewRegistry()
	exporter := NewPrometheusExporter(reg)
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			scope := Scope{Collector: "c", Target: fmt.Sprintf("db-%d", i), Probe: "waits"}
			for j := range 200 {
				samples := waitSamples(10 + j%5)
				for k := range samples {
					samples[k].Labels["target"] = scope.Target
				}
				_ = exporter.RecordScope(scope, samples)
			}
		})
	}
	wg.Go(func() {
		for range 50 {
			if _, err := reg.Gather(); err != nil {
				t.Errorf("Gather: %v", err)
				return
			}
		}
	})
	wg.Wait()
}

// waitSamples returns n counter samples of one metric, one per wait type.
func waitSamples(n int) []Sample {
	samples := make([]Sample, n)
	for i := range samples {
		samples[i] = Sample{
			Metric: "atrium_test_wait_seconds_total",
			Type:   Counter,
			Value:  float64(i),
			Labels: map[string]string{"environment": "prod", "target": "db", "wait_type": fmt.Sprintf("WAIT_%d", i)},
		}
	}
	return samples
}

func BenchmarkRecordScope(b *testing.B) {
	exporter := NewPrometheusExporter(prometheus.NewRegistry())
	scope := Scope{Collector: "c", Target: "db", Probe: "waits"}
	samples := waitSamples(300)
	b.ReportAllocs()
	for b.Loop() {
		_ = exporter.RecordScope(scope, samples)
	}
}

// gatherBenchTargets and gatherBenchWaits size the Gather benchmarks like a
// large deployment: 50 targets with 1,000 wait types each.
const (
	gatherBenchTargets = 50
	gatherBenchWaits   = 1000
)

// BenchmarkGather measures one scrape of the exporter's series.
func BenchmarkGather(b *testing.B) {
	reg := prometheus.NewRegistry()
	exporter := NewPrometheusExporter(reg)
	for i := range gatherBenchTargets {
		target := fmt.Sprintf("db-%02d", i)
		samples := waitSamples(gatherBenchWaits)
		for k := range samples {
			samples[k].Labels["target"] = target
		}
		if err := exporter.RecordScope(Scope{Collector: "c", Target: target, Probe: "waits"}, samples); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := reg.Gather(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGatherGaugeVecBaseline scrapes the same series from a plain
// GaugeVec, the lower bound for comparison with BenchmarkGather.
func BenchmarkGatherGaugeVecBaseline(b *testing.B) {
	reg := prometheus.NewRegistry()
	vec := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "atrium_test_wait_seconds_total", Help: "Wait time."}, []string{"environment", "target", "wait_type"})
	reg.MustRegister(vec)
	for i := range gatherBenchTargets {
		for j := range gatherBenchWaits {
			vec.WithLabelValues("prod", fmt.Sprintf("db-%02d", i), fmt.Sprintf("WAIT_%d", j)).Set(float64(j))
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := reg.Gather(); err != nil {
			b.Fatal(err)
		}
	}
}
