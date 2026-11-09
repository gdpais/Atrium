package collectors

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	collectorconfig "atrium/internal/config"
	collectorexport "atrium/services/db-collector/internal/export"
	collectormetadata "atrium/services/db-collector/internal/metadata"
	catalogsqlserver "atrium/services/db-collector/internal/probes/sqlserver"
)

type fakeExecutor struct{}

func (fakeExecutor) RunProbe(context.Context, collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
	return []collectorexport.Sample{{
		Metric: "atrium_sqlserver_wait_seconds_total",
		Type:   collectorexport.Counter,
		Value:  12,
		Labels: map[string]string{"environment": "prod", "target": "core-db", "wait_type": "LCK_M_X"},
	}}, []collectormetadata.Evidence{{Title: "blocking snapshot", Kind: "blocking"}}, nil
}

type fakeSink struct {
	evidence []collectormetadata.Evidence
}

func (f *fakeSink) Publish(items []collectormetadata.Evidence) error {
	f.evidence = append(f.evidence, items...)
	return nil
}

func TestRunnerExecutesConfiguredCollector(t *testing.T) {
	exporter := collectorexport.NewInMemoryExporter()
	sink := &fakeSink{}
	runner := NewRunner(fakeExecutor{}, exporter, sink)

	result, err := runner.RunOnce(context.Background(), collectorconfig.CollectorRuntimeConfig{
		ID:             "sql-prod",
		Kind:           "sqlserver",
		Enabled:        true,
		CredentialRef:  "kv/sql-prod",
		ScrapeInterval: 30 * time.Second,
		Environment:    "prod",
		Targets: []collectorconfig.TargetRuntimeConfig{{
			Name:            "core-db",
			EnvironmentSlug: "prod",
			Engine:          "sqlserver",
			Probes: []collectorconfig.ProbeRuntimeConfig{{
				Name:          "waits",
				QueryTemplate: "SELECT 1",
				TimeoutMS:     5000,
			}},
		}},
	})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !result.Healthy() || len(result.Targets) != 1 || result.Targets[0].State != TargetOK {
		t.Fatalf("unexpected cycle result: %+v", result)
	}

	if got := exporter.LastValue("atrium_sqlserver_wait_seconds_total"); got != 12 {
		t.Fatalf("unexpected metric value: %v", got)
	}
	if len(sink.evidence) != 1 {
		t.Fatalf("expected 1 evidence item, got %d", len(sink.evidence))
	}
}

// backoffScenario drives one target with a healthy "throughput" probe and a
// "blocking" probe that fails until healthy is set, through cycles on a fake
// clock with a 10s interval and no jitter: failed at 0s and 10s (backoff until
// 20s), skipped at 15s.
type backoffScenario struct {
	t        *testing.T
	exporter *collectorexport.InMemoryExporter
	runner   Runner
	tracker  *targetTracker
	clock    *fakeClock
	t0       time.Time
	// healthy false fails every probe, as an unreachable database does, or
	// only broken when it is set.
	healthy bool
	broken  string
}

var errDivideByZero = errors.New("mssql: Divide by zero error encountered")

func newBackoffScenario(t *testing.T) *backoffScenario {
	s := &backoffScenario{
		t:        t,
		exporter: collectorexport.NewInMemoryExporter(),
		clock:    &fakeClock{},
		tracker:  newTargetTracker(),
		t0:       time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	executor := funcExecutor(func(_ context.Context, item collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
		if !s.healthy && (s.broken == "" || item.Definition.Name == s.broken) {
			return nil, nil, errDivideByZero
		}
		return sampleFor(item), nil, nil
	})
	s.runner = NewRunner(executor, s.exporter, nil).WithLogger(slog.New(slog.DiscardHandler))
	s.tracker.now = s.clock.Now
	s.tracker.jitter = func() float64 { return 0 }
	return s
}

// cycle runs one cycle at offset from t0 and returns the target's result
// after checking its state.
func (s *backoffScenario) cycle(offset time.Duration, want TargetState) TargetResult {
	s.t.Helper()
	s.clock.Set(s.t0.Add(offset))
	collector := testCollector(10*time.Second, testTarget("core-db", "throughput", "blocking"))
	result := s.runner.runCycle(context.Background(), collector, s.tracker)
	if len(result.Targets) != 1 || result.Targets[0].State != want {
		s.t.Fatalf("at %s: expected one %s target, got %+v", offset, want, result.Targets)
	}
	return result.Targets[0]
}

// healthyProbeExported reports whether the healthy probe's series is exported.
func (s *backoffScenario) healthyProbeExported() bool {
	_, ok := s.exporter.Value(testMetric, sampleLabels("core-db", "throughput"))
	return ok
}

// A target backing off runs no probes, so it must not keep exporting the last
// values of the probes that succeeded in its failed cycle: they would stay
// frozen for up to the backoff cap.  Its health series stay.
func TestBackoffClearsSeriesOfHealthyProbes(t *testing.T) {
	s := newBackoffScenario(t)
	s.healthy = true
	s.cycle(-10*time.Second, TargetOK)
	if !s.healthyProbeExported() {
		t.Fatalf("expected the probe's sample in a successful cycle")
	}
	s.healthy = false
	s.cycle(0, TargetFailed)
	s.cycle(10*time.Second, TargetFailed)

	s.cycle(15*time.Second, TargetBackoff)
	if s.healthyProbeExported() {
		t.Fatalf("healthy probe's sample stayed exported during backoff")
	}
	if up, ok := s.exporter.Value(MetricTargetUp, healthLabels("core-db")); !ok || up != 0 {
		t.Fatalf("expected target_up=0 during backoff, got %v (present %v)", up, ok)
	}
	if failures, ok := s.exporter.Value(MetricTargetConsecutiveFailures, healthLabels("core-db")); !ok || failures != 2 {
		t.Fatalf("expected consecutive_failures=2 during backoff, got %v (present %v)", failures, ok)
	}

	s.healthy = true
	s.cycle(20*time.Second, TargetOK)
	if !s.healthyProbeExported() {
		t.Fatalf("expected the healthy probe's sample back after recovery")
	}
}

// Every backoff cycle reports the error that caused the backoff, so
// /admin/config shows why a target is not collected for the whole failure
// episode, not only in its failed cycles.
func TestBackoffReportsLastFailureError(t *testing.T) {
	s := newBackoffScenario(t)
	s.cycle(0, TargetFailed)
	failed := s.cycle(10*time.Second, TargetFailed)
	if !errors.Is(failed.Err, errDivideByZero) {
		t.Fatalf("expected the probe error on the failed cycle, got %v", failed.Err)
	}

	skipped := s.cycle(15*time.Second, TargetBackoff)
	if skipped.Err == nil || skipped.Err.Error() != failed.Err.Error() {
		t.Fatalf("expected the last failure error during backoff, got %v, want %v", skipped.Err, failed.Err)
	}

	s.healthy = true
	if recovered := s.cycle(20*time.Second, TargetOK); recovered.Err != nil {
		t.Fatalf("expected no error after recovery, got %v", recovered.Err)
	}
	if state := s.tracker.get("core-db"); state.lastErr != nil {
		t.Fatalf("expected recovery to forget the last error, got %v", state.lastErr)
	}
}

func TestDecodeRowsUsesExplicitMetricDescriptors(t *testing.T) {
	item := collectormetadata.ScheduledProbe{
		Target: collectormetadata.DatabaseTarget{Name: "core-db", EnvironmentSlug: "prod"},
	}
	probe := catalogsqlserver.Probe{
		Metrics: []catalogsqlserver.Metric{{
			Name:         "atrium_sqlserver_wait_seconds_total",
			Help:         "Wait time.",
			Type:         collectorexport.Counter,
			ValueColumn:  "wait_time_ms",
			Scale:        0.001,
			LabelColumns: []string{"wait_type"},
		}},
	}
	rows := []map[string]any{{
		"wait_type":       "LCK_M_X",
		"wait_time_ms":    int64(42),
		"ignored_numeric": int64(99),
		"ignored_label":   "high-cardinality-value",
	}}

	samples := decodeRows(item, probe, rows)
	if len(samples) != 1 {
		t.Fatalf("expected 1 sample, got %d", len(samples))
	}
	sample := samples[0]
	if sample.Metric != "atrium_sqlserver_wait_seconds_total" || sample.Type != collectorexport.Counter {
		t.Fatalf("unexpected metric: %s (%s)", sample.Metric, sample.Type)
	}
	if sample.Value != 0.042 {
		t.Fatalf("unexpected value: %v", sample.Value)
	}
	if sample.Labels["wait_type"] != "LCK_M_X" {
		t.Fatalf("unexpected wait_type label: %s", sample.Labels["wait_type"])
	}
	if _, ok := sample.Labels["ignored_label"]; ok {
		t.Fatalf("unexpected ignored_label in labels")
	}
}

func TestStorageProbeEmitsOneSeriesPerFile(t *testing.T) {
	probe, ok := catalogsqlserver.DefaultCatalog().Get("storage")
	if !ok {
		t.Fatal("storage probe missing")
	}
	item := collectormetadata.ScheduledProbe{
		Target: collectormetadata.DatabaseTarget{Name: "core-db", EnvironmentSlug: "prod"},
	}
	// A database has at least a data and a log file; both share database_name.
	rows := []map[string]any{
		{"database_name": "sales", "file_name": "sales", "file_type": "ROWS", "size_pages": int64(65536)},
		{"database_name": "sales", "file_name": "sales_log", "file_type": "LOG", "size_pages": int64(8192)},
	}
	samples := decodeRows(item, probe, rows)
	if len(samples) != 2 {
		t.Fatalf("expected 2 samples, got %d", len(samples))
	}
	exporter := collectorexport.NewInMemoryExporter()
	if err := exporter.RecordScope(collectorexport.Scope{Collector: "c", Target: "core-db", Probe: "storage"}, samples); err != nil {
		t.Fatalf("files of one database must not collide: %v", err)
	}
}

// Each catalog probe decodes rows shaped like its query's result into
// samples in base units with the right type.  The live SQL Server test checks
// the queries themselves.
func TestCatalogProbesDecodeToBaseUnitsAndTypes(t *testing.T) {
	type want struct {
		metric     string
		metricType collectorexport.MetricType
		value      float64
		label      string // label=value beyond environment and target, if any
	}
	tests := []struct {
		probe string
		rows  []map[string]any
		want  []want
	}{
		{
			probe: "waits",
			rows:  []map[string]any{{"wait_type": "LCK_M_X", "wait_time_ms": int64(2500)}},
			want:  []want{{"atrium_sqlserver_wait_seconds_total", collectorexport.Counter, 2.5, "wait_type=LCK_M_X"}},
		},
		{
			probe: "memory_pressure",
			rows:  []map[string]any{{"total_server_memory_kb": int64(2048)}},
			want:  []want{{"atrium_sqlserver_total_server_memory_bytes", collectorexport.Gauge, 2 << 20, ""}},
		},
		{
			probe: "storage",
			rows:  []map[string]any{{"database_name": "sales", "file_name": "sales", "file_type": "ROWS", "size_pages": int64(128)}},
			want:  []want{{"atrium_sqlserver_database_file_size_bytes", collectorexport.Gauge, 1 << 20, "database_name=sales"}},
		},
		{
			// One pivoted row yields one counter per column.
			probe: "throughput",
			rows:  []map[string]any{{"batch_requests": int64(1200), "transactions": int64(300)}},
			want: []want{
				{"atrium_sqlserver_batch_requests_total", collectorexport.Counter, 1200, ""},
				{"atrium_sqlserver_transactions_total", collectorexport.Counter, 300, ""},
			},
		},
		{
			// A counter missing on the server is NULL after the pivot: no
			// sample rather than a fake 0.
			probe: "throughput",
			rows:  []map[string]any{{"batch_requests": int64(1200), "transactions": nil}},
			want:  []want{{"atrium_sqlserver_batch_requests_total", collectorexport.Counter, 1200, ""}},
		},
		{
			// Percent from the scheduler monitor record becomes a 0-1 ratio.
			probe: "cpu",
			rows:  []map[string]any{{"sql_process_percent": int64(25), "other_process_percent": int64(10)}},
			want: []want{
				{"atrium_sqlserver_cpu_sql_process_ratio", collectorexport.Gauge, 0.25, ""},
				{"atrium_sqlserver_cpu_other_process_ratio", collectorexport.Gauge, 0.1, ""},
			},
		},
		{
			// The driver returns the float division as float64.
			probe: "buffer_cache",
			rows:  []map[string]any{{"page_life_expectancy_seconds": int64(3600), "buffer_cache_hit_ratio": 0.995}},
			want: []want{
				{"atrium_sqlserver_page_life_expectancy_seconds", collectorexport.Gauge, 3600, ""},
				{"atrium_sqlserver_buffer_cache_hit_ratio", collectorexport.Gauge, 0.995, ""},
			},
		},
		{
			// A zero hit ratio base is NULL after NULLIF: no sample rather
			// than a division by zero.
			probe: "buffer_cache",
			rows:  []map[string]any{{"page_life_expectancy_seconds": int64(3600), "buffer_cache_hit_ratio": nil}},
			want:  []want{{"atrium_sqlserver_page_life_expectancy_seconds", collectorexport.Gauge, 3600, ""}},
		},
		{
			// One file row yields six counters, stall time in seconds.
			probe: "file_io",
			rows: []map[string]any{{
				"database_name": "sales", "file_name": "sales_log", "file_type": "LOG",
				"num_of_reads": int64(10), "num_of_writes": int64(400),
				"num_of_bytes_read": int64(81920), "num_of_bytes_written": int64(1 << 20),
				"io_stall_read_ms": int64(30), "io_stall_write_ms": int64(1500),
			}},
			want: []want{
				{"atrium_sqlserver_database_file_reads_total", collectorexport.Counter, 10, "file_name=sales_log"},
				{"atrium_sqlserver_database_file_writes_total", collectorexport.Counter, 400, "file_type=LOG"},
				{"atrium_sqlserver_database_file_read_bytes_total", collectorexport.Counter, 81920, "database_name=sales"},
				{"atrium_sqlserver_database_file_written_bytes_total", collectorexport.Counter, 1 << 20, "database_name=sales"},
				{"atrium_sqlserver_database_file_read_stall_seconds_total", collectorexport.Counter, 0.03, "database_name=sales"},
				{"atrium_sqlserver_database_file_write_stall_seconds_total", collectorexport.Counter, 1.5, "database_name=sales"},
			},
		},
	}
	item := collectormetadata.ScheduledProbe{Target: collectormetadata.DatabaseTarget{Name: "core-db", EnvironmentSlug: "prod"}}
	for _, tt := range tests {
		t.Run(tt.probe, func(t *testing.T) {
			probe, ok := catalogsqlserver.DefaultCatalog().Get(tt.probe)
			if !ok {
				t.Fatalf("probe %s missing", tt.probe)
			}
			samples := decodeRows(item, probe, tt.rows)
			if len(samples) != len(tt.want) {
				t.Fatalf("expected %d samples, got %+v", len(tt.want), samples)
			}
			for i, w := range tt.want {
				got := samples[i]
				if got.Metric != w.metric || got.Type != w.metricType || got.Value != w.value {
					t.Errorf("sample %d = %s %s %v, want %s %s %v", i, got.Metric, got.Type, got.Value, w.metric, w.metricType, w.value)
				}
				if w.label != "" {
					name, value, _ := strings.Cut(w.label, "=")
					if got.Labels[name] != value {
						t.Errorf("sample %d label %s = %q, want %q", i, name, got.Labels[name], value)
					}
				}
				if got.Labels["environment"] != "prod" || got.Labels["target"] != "core-db" {
					t.Errorf("sample %d lacks environment/target labels: %v", i, got.Labels)
				}
			}
		})
	}
}

// probeUp returns the probe_up value of a probe of core-db.
func (s *backoffScenario) probeUp(probe string) (float64, bool) {
	labels := healthLabels("core-db")
	labels["probe"] = probe
	return s.exporter.Value(MetricProbeUp, labels)
}

// A probe that fails while its target's other probes succeed fails only
// itself: the target stays up and keeps collecting, and the probe backs off
// on its own, with its own failure count, until it recovers.
func TestFailingProbeBacksOffAlone(t *testing.T) {
	s := newBackoffScenario(t)
	logger, logs := newCapturedLogger()
	s.runner = s.runner.WithLogger(logger)
	s.broken = "blocking"
	for i, offset := range []time.Duration{0, 10 * time.Second} {
		degraded := s.cycle(offset, TargetDegraded)
		if len(degraded.Probes) != 1 || degraded.Probes[0].Probe != "blocking" || degraded.Probes[0].State != ProbeFailed ||
			degraded.Probes[0].ConsecutiveFailures != i+1 || !errors.Is(degraded.Probes[0].Err, errDivideByZero) {
			t.Fatalf("at %s: probes %+v, want blocking failed %d times", offset, degraded.Probes, i+1)
		}
		if degraded.ConsecutiveFailures != 0 || !degraded.NextAttempt.IsZero() {
			t.Fatalf("at %s: the target backs off: %+v", offset, degraded)
		}
		if !degraded.LastSuccess.Equal(s.t0.Add(offset)) {
			t.Fatalf("at %s: last success %s, want this cycle's", offset, degraded.LastSuccess)
		}
	}
	// One log line per failure, with the probe's own failure count.
	failures := logs.records(t, "probe failed")
	for i, record := range failures {
		if record["probe"] != "blocking" || record["consecutive_failures"] != float64(i+1) || record["target_state"] != string(TargetDegraded) {
			t.Fatalf("probe failure log %d: %v", i, record)
		}
	}
	if len(failures) != 2 {
		t.Fatalf("got %d probe failure logs, want 2", len(failures))
	}
	if !s.healthyProbeExported() {
		t.Fatal("the healthy probe stopped exporting")
	}
	if up, _ := s.exporter.Value(MetricTargetUp, healthLabels("core-db")); up != 1 {
		t.Fatalf("target_up = %v, want 1", up)
	}
	if up, _ := s.probeUp("throughput"); up != 1 {
		t.Fatalf("probe_up{throughput} = %v, want 1", up)
	}
	if up, ok := s.probeUp("blocking"); !ok || up != 0 {
		t.Fatalf("probe_up{blocking} = %v (present %v), want 0", up, ok)
	}

	// Two failures: blocking waits 10s from the cycle at 10s, the healthy
	// probe runs.
	s.clock.Set(s.t0.Add(15 * time.Second))
	collector := testCollector(10*time.Second, testTarget("core-db", "throughput", "blocking"))
	var ran []string
	s.runner.executor = funcExecutor(func(_ context.Context, item collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
		ran = append(ran, item.Definition.Name)
		return sampleFor(item), nil, nil
	})
	result := s.runner.runCycle(context.Background(), collector, s.tracker)
	waiting := result.Targets[0]
	if waiting.State != TargetDegraded || len(waiting.Probes) != 1 || waiting.Probes[0].State != ProbeBackoff ||
		!waiting.Probes[0].NextAttempt.Equal(s.t0.Add(20*time.Second)) || !errors.Is(waiting.Probes[0].Err, errDivideByZero) {
		t.Fatalf("at 15s: %+v, want blocking backing off until 20s with its last error", waiting)
	}
	if len(ran) != 1 || ran[0] != "throughput" {
		t.Fatalf("at 15s ran %v, want only throughput", ran)
	}
	if _, ok := s.exporter.Value(testMetric, sampleLabels("core-db", "blocking")); ok {
		t.Fatal("at 15s blocking exports while it backs off")
	}

	// At 20s it runs again, and recovers.
	s.clock.Set(s.t0.Add(20 * time.Second))
	ran = nil
	if result := s.runner.runCycle(context.Background(), collector, s.tracker); result.Targets[0].State != TargetOK || len(ran) != 2 {
		t.Fatalf("at 20s: %+v ran %v, want ok with both probes", result.Targets[0], ran)
	}
	if up, _ := s.probeUp("blocking"); up != 1 {
		t.Fatalf("probe_up{blocking} = %v after recovery, want 1", up)
	}
}

// A target is failed, and backs off as a whole, only when every probe that
// ran failed; its probes' own failures are then forgotten. When every probe
// backs off on its own, they all run.
func TestTargetFailsOnlyWhenEveryProbeFails(t *testing.T) {
	s := newBackoffScenario(t)
	s.broken = "blocking"
	s.cycle(0, TargetDegraded)
	s.cycle(10*time.Second, TargetDegraded) // blocking waits until 20s
	s.broken = ""                           // the database goes away
	failed := s.cycle(15*time.Second, TargetFailed)
	if failed.ConsecutiveFailures != 1 {
		t.Fatalf("target %+v, want its first failure", failed)
	}
	if _, waiting := s.tracker.probeWaiting("core-db", "blocking", s.t0.Add(15*time.Second)); waiting {
		t.Fatal("the probe's own backoff outlived its target's failure")
	}
	if up, _ := s.probeUp("throughput"); up != 0 {
		t.Fatalf("probe_up{throughput} = %v while its target failed, want 0", up)
	}

	// Both probes back off on their own: both run.
	s.healthy = true
	collector := testCollector(10*time.Second, testTarget("core-db", "throughput", "blocking"))
	start := s.t0.Add(time.Minute)
	for _, probe := range []string{"throughput", "blocking"} {
		for range 3 {
			s.tracker.probeFail("core-db", probe, start, 10*time.Second, errDivideByZero)
		}
	}
	s.clock.Set(start)
	var ran []string
	s.runner.executor = funcExecutor(func(_ context.Context, item collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
		ran = append(ran, item.Definition.Name)
		return sampleFor(item), nil, nil
	})
	if result := s.runner.runCycle(context.Background(), collector, s.tracker); result.Targets[0].State != TargetOK || len(ran) != 2 {
		t.Fatalf("every probe backing off: %+v ran %v, want both run and ok", result.Targets[0], ran)
	}
}
