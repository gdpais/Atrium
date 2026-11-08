package collectors

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	collectorconfig "heartbeat/internal/config"
	collectorexport "heartbeat/services/db-collector/internal/export"
	collectormetadata "heartbeat/services/db-collector/internal/metadata"
)

const testMetric = "heartbeat_test_value"

// funcExecutor adapts a function to ProbeExecutor.
type funcExecutor func(context.Context, collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error)

func (f funcExecutor) RunProbe(ctx context.Context, item collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
	return f(ctx, item)
}

// logCapture is a concurrency-safe buffer for a JSON slog handler.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

// records returns every decoded log record whose msg equals msg.
func (c *logCapture) records(t *testing.T, msg string) []map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(c.buf.Bytes()))
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("decode log line %q: %v", scanner.Text(), err)
		}
		if record["msg"] == msg {
			out = append(out, record)
		}
	}
	return out
}

func newCapturedLogger() (*slog.Logger, *logCapture) {
	capture := &logCapture{}
	return slog.New(slog.NewJSONHandler(capture, &slog.HandlerOptions{Level: slog.LevelDebug})), capture
}

func testTarget(name string, probes ...string) collectorconfig.TargetRuntimeConfig {
	target := collectorconfig.TargetRuntimeConfig{Name: name, EnvironmentSlug: "prod", Engine: "sqlserver"}
	for _, probe := range probes {
		target.Probes = append(target.Probes, collectorconfig.ProbeRuntimeConfig{Name: probe})
	}
	return target
}

func testCollector(interval time.Duration, targets ...collectorconfig.TargetRuntimeConfig) collectorconfig.CollectorRuntimeConfig {
	return collectorconfig.CollectorRuntimeConfig{
		ID:             "sql-prod",
		Kind:           "sqlserver",
		Enabled:        true,
		ScrapeInterval: interval,
		Targets:        targets,
	}
}

func sampleLabels(target, probe string) map[string]string {
	return map[string]string{"environment": "prod", "target": target, "probe": probe}
}

func sampleFor(item collectormetadata.ScheduledProbe) []collectorexport.Sample {
	return []collectorexport.Sample{{
		Metric: testMetric,
		Value:  1,
		Labels: sampleLabels(item.Target.Name, item.Definition.Name),
	}}
}

func healthLabels(target string) map[string]string {
	return map[string]string{"collector": "sql-prod", "environment": "prod", "target": target}
}

// fakeClock is a manually advanced clock safe for concurrent reads.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = at
}

func TestRunOnceIsolatesFailingTargetAndLogsEveryProbeError(t *testing.T) {
	logger, logs := newCapturedLogger()
	exporter := collectorexport.NewInMemoryExporter()
	errBoom := errors.New("login failed")
	executor := funcExecutor(func(_ context.Context, item collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
		if item.Target.Name == "bad" {
			return nil, nil, fmt.Errorf("%s: %w", item.Definition.Name, errBoom)
		}
		return sampleFor(item), nil, nil
	})
	runner := NewRunner(executor, exporter, nil).WithLogger(logger)
	collector := testCollector(time.Minute,
		testTarget("good-a", "waits", "sessions"),
		testTarget("bad", "waits", "sessions", "blocking"),
		testTarget("good-b", "waits", "sessions"),
	)

	result, err := runner.RunOnce(context.Background(), collector)
	if err == nil || !errors.Is(err, errBoom) {
		t.Fatalf("expected joined target error, got %v", err)
	}
	if len(result.Targets) != 3 {
		t.Fatalf("expected 3 target results, got %+v", result.Targets)
	}
	states := map[string]TargetResult{}
	for _, target := range result.Targets {
		states[target.Target] = target
	}
	if states["good-a"].State != TargetOK || states["good-b"].State != TargetOK {
		t.Fatalf("healthy targets affected by failing target: %+v", result.Targets)
	}
	bad := states["bad"]
	if bad.State != TargetFailed || bad.ConsecutiveFailures != 1 || bad.NextAttempt.IsZero() {
		t.Fatalf("unexpected failing target result: %+v", bad)
	}
	for _, probe := range []string{"waits", "sessions", "blocking"} {
		if !strings.Contains(bad.Err.Error(), probe+": login failed") {
			t.Fatalf("target error %q is missing probe %s", bad.Err, probe)
		}
	}
	for _, target := range []string{"good-a", "good-b"} {
		for _, probe := range []string{"waits", "sessions"} {
			if _, ok := exporter.Value(testMetric, sampleLabels(target, probe)); !ok {
				t.Fatalf("missing sample for %s/%s", target, probe)
			}
		}
		if up, _ := exporter.Value(MetricTargetUp, healthLabels(target)); up != 1 {
			t.Fatalf("expected %s up=1, got %v", target, up)
		}
	}
	if up, ok := exporter.Value(MetricTargetUp, healthLabels("bad")); !ok || up != 0 {
		t.Fatalf("expected bad up=0, got %v (present=%v)", up, ok)
	}
	if failures, _ := exporter.Value(MetricTargetConsecutiveFailures, healthLabels("bad")); failures != 1 {
		t.Fatalf("expected bad consecutive failures 1, got %v", failures)
	}
	if _, ok := exporter.Value(MetricCycleDuration, map[string]string{"collector": "sql-prod"}); !ok {
		t.Fatalf("missing cycle duration metric")
	}

	records := logs.records(t, "probe failed")
	if len(records) != 3 {
		t.Fatalf("expected 3 probe failure logs, got %d", len(records))
	}
	for _, record := range records {
		if record["level"] != "WARN" || record["collector"] != "sql-prod" || record["target"] != "bad" ||
			record["probe"] == "" || record["error"] == nil || record["consecutive_failures"] != float64(1) {
			t.Fatalf("unexpected probe failure log: %v", record)
		}
	}
	if got := len(logs.records(t, "target failed")); got != 1 {
		t.Fatalf("expected 1 backoff log, got %d", got)
	}
}

func TestConcurrentFailuresAreAllLogged(t *testing.T) {
	logger, logs := newCapturedLogger()
	const targets, probes = 24, 3
	var cfgTargets []collectorconfig.TargetRuntimeConfig
	for i := range targets {
		cfgTargets = append(cfgTargets, testTarget(fmt.Sprintf("db-%02d", i), "p0", "p1", "p2"))
	}
	executor := funcExecutor(func(context.Context, collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
		time.Sleep(time.Millisecond)
		return nil, nil, errors.New("connection refused")
	})
	runner := NewRunner(executor, collectorexport.NewInMemoryExporter(), nil).WithLogger(logger)

	result, err := runner.RunOnce(context.Background(), testCollector(time.Minute, cfgTargets...))
	if err == nil {
		t.Fatalf("expected error")
	}
	if len(result.Targets) != targets {
		t.Fatalf("expected %d targets, got %d", targets, len(result.Targets))
	}
	records := logs.records(t, "probe failed")
	if len(records) != targets*probes {
		t.Fatalf("expected %d probe failure logs, got %d", targets*probes, len(records))
	}
	seen := map[string]bool{}
	for _, record := range records {
		key := fmt.Sprint(record["target"], "/", record["probe"])
		if seen[key] {
			t.Fatalf("duplicate log for %s", key)
		}
		seen[key] = true
	}
	if got := len(logs.records(t, "target failed")); got != targets {
		t.Fatalf("expected %d backoff logs, got %d", targets, got)
	}
}

func TestBackoffSkipsFailingTargetAndResetsOnSuccess(t *testing.T) {
	logger, logs := newCapturedLogger()
	exporter := collectorexport.NewInMemoryExporter()
	var failing atomic.Bool
	failing.Store(true)
	var calls atomic.Int32
	executor := funcExecutor(func(_ context.Context, item collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
		calls.Add(1)
		if failing.Load() {
			return nil, nil, errors.New("timeout")
		}
		return sampleFor(item), nil, nil
	})
	runner := NewRunner(executor, exporter, nil).WithLogger(logger)
	collector := testCollector(10*time.Second, testTarget("core-db", "waits"))
	clock := &fakeClock{}
	tracker := newTargetTracker()
	tracker.now = clock.Now
	tracker.jitter = func() float64 { return 0 } // factor 1.0
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	step := func(offset time.Duration, wantState TargetState, wantCalls int32) TargetResult {
		t.Helper()
		clock.Set(t0.Add(offset))
		result := runner.runCycle(context.Background(), collector, tracker)
		if len(result.Targets) != 1 {
			t.Fatalf("expected 1 target at %s, got %+v", offset, result.Targets)
		}
		got := result.Targets[0]
		if got.State != wantState {
			t.Fatalf("at %s: expected state %s, got %+v", offset, wantState, got)
		}
		if n := calls.Load(); n != wantCalls {
			t.Fatalf("at %s: expected %d executor calls, got %d", offset, wantCalls, n)
		}
		return got
	}

	// A single failure retries on the very next cycle.
	first := step(0, TargetFailed, 1)
	if first.ConsecutiveFailures != 1 || !first.NextAttempt.Equal(t0) {
		t.Fatalf("unexpected first failure: %+v", first)
	}
	second := step(10*time.Second, TargetFailed, 2)
	if second.ConsecutiveFailures != 2 || !second.NextAttempt.Equal(t0.Add(20*time.Second)) {
		t.Fatalf("unexpected second failure: %+v", second)
	}
	skipped := step(15*time.Second, TargetBackoff, 2)
	if !skipped.NextAttempt.Equal(t0.Add(20*time.Second)) || skipped.ConsecutiveFailures != 2 {
		t.Fatalf("unexpected backoff result: %+v", skipped)
	}
	if up, _ := exporter.Value(MetricTargetUp, healthLabels("core-db")); up != 0 {
		t.Fatalf("expected up=0 during backoff, got %v", up)
	}
	third := step(20*time.Second, TargetFailed, 3)
	if third.ConsecutiveFailures != 3 || !third.NextAttempt.Equal(t0.Add(40*time.Second)) {
		t.Fatalf("expected doubled backoff, got %+v", third)
	}
	step(39*time.Second, TargetBackoff, 3)

	failing.Store(false)
	recovered := step(40*time.Second, TargetOK, 4)
	if recovered.ConsecutiveFailures != 0 || !recovered.LastSuccess.Equal(t0.Add(40*time.Second)) || recovered.Err != nil {
		t.Fatalf("unexpected recovered result: %+v", recovered)
	}
	step(41*time.Second, TargetOK, 5)

	if got := len(logs.records(t, "target recovered")); got != 1 {
		t.Fatalf("expected 1 recovery log, got %d", got)
	}
	if got := len(logs.records(t, "target failed")); got != 3 {
		t.Fatalf("expected 3 target failure logs, got %d", got)
	}
	if up, _ := exporter.Value(MetricTargetUp, healthLabels("core-db")); up != 1 {
		t.Fatalf("expected up=1 after recovery, got %v", up)
	}
	if last, _ := exporter.Value(MetricTargetLastSuccess, healthLabels("core-db")); last != float64(t0.Add(41*time.Second).Unix()) {
		t.Fatalf("unexpected last success timestamp %v", last)
	}
}

func TestBackoffFor(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		failures int
		jitter   float64
		want     time.Duration
	}{
		{"first failure retries next cycle", 10 * time.Second, 1, 0.5, 0},
		{"second failure waits one interval", 10 * time.Second, 2, 0, 10 * time.Second},
		{"doubles per failure", 10 * time.Second, 4, 0, 40 * time.Second},
		{"capped at five minutes", 10 * time.Second, 10, 0, 5 * time.Minute},
		{"huge failure count does not overflow", 10 * time.Second, 10_000, 0, 5 * time.Minute},
		{"jitter only adds", 10 * time.Second, 2, 0.5, 11 * time.Second},
		{"maximum jitter", 10 * time.Second, 2, 1, 12 * time.Second},
		{"zero interval uses fallback", 0, 2, 0, fallbackProbeTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := backoffFor(tt.interval, tt.failures, tt.jitter); got != tt.want {
				t.Fatalf("backoffFor(%s, %d, %v) = %s, want %s", tt.interval, tt.failures, tt.jitter, got, tt.want)
			}
		})
	}
}

func TestProbeTimeout(t *testing.T) {
	tests := []struct {
		name      string
		timeoutMS int
		interval  time.Duration
		want      time.Duration
	}{
		{"default is half the interval", 0, 4 * time.Second, 2 * time.Second},
		{"default capped at ten seconds", 0, 30 * time.Second, 10 * time.Second},
		{"fallback without interval", 0, 0, 5 * time.Second},
		{"override wins", 2000, 30 * time.Second, 2 * time.Second},
		{"override capped at interval", 20_000, 10 * time.Second, 10 * time.Second},
		{"override without interval", 20_000, 0, 20 * time.Second},
		{"override capped at the maximum", 60_000, 2 * time.Minute, collectorconfig.MaxProbeTimeout},
		{"override without interval capped at the maximum", 60_000, 0, collectorconfig.MaxProbeTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := probeTimeout(tt.timeoutMS, tt.interval); got != tt.want {
				t.Fatalf("probeTimeout(%d, %s) = %s, want %s", tt.timeoutMS, tt.interval, got, tt.want)
			}
		})
	}
	item := collectormetadata.ScheduledProbe{
		Definition: collectormetadata.ProbeDefinition{TimeoutMS: 20_000},
		Assignment: collectormetadata.ProbeAssignment{IntervalSeconds: 10},
	}
	if got := timeoutFor(item); got != 10*time.Second {
		t.Fatalf("timeoutFor capped override = %s, want 10s", got)
	}
}

func TestRunnerAppliesDefaultProbeTimeout(t *testing.T) {
	var remaining atomic.Int64
	executor := funcExecutor(func(ctx context.Context, item collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			return nil, nil, errors.New("probe context has no deadline")
		}
		remaining.Store(int64(time.Until(deadline)))
		return nil, nil, nil
	})
	runner := NewRunner(executor, collectorexport.NewInMemoryExporter(), nil).WithLogger(slog.New(slog.DiscardHandler))
	if _, err := runner.RunOnce(context.Background(), testCollector(time.Second, testTarget("core-db", "waits"))); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := time.Duration(remaining.Load()); got <= 0 || got > 500*time.Millisecond {
		t.Fatalf("expected probe deadline of at most half the interval, got %s", got)
	}
}

func TestCycleDeadlineSkipsProbesThatCannotStart(t *testing.T) {
	logger, logs := newCapturedLogger()
	slow := atomic.Bool{}
	executor := funcExecutor(func(ctx context.Context, item collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
		if slow.Load() {
			<-ctx.Done()
			return nil, nil, ctx.Err()
		}
		return sampleFor(item), nil, nil
	})
	exporter := collectorexport.NewInMemoryExporter()
	runner := NewRunner(executor, exporter, nil).
		WithLogger(logger).
		WithMaxConcurrentTargets(1)
	collector := testCollector(100*time.Millisecond,
		testTarget("t1", "p1", "p2"),
		testTarget("t2", "p1", "p2"),
		testTarget("t3", "p1", "p2"),
	)
	for i := range collector.Targets {
		for j := range collector.Targets[i].Probes {
			collector.Targets[i].Probes[j].TimeoutMS = 60_000 // capped at the interval
		}
	}
	tracker := newTargetTracker()
	tracker.jitter = func() float64 { return 0 }
	if result := runner.runCycle(context.Background(), collector, tracker); cycleError(result) != nil {
		t.Fatalf("first cycle: %v", cycleError(result))
	}

	slow.Store(true)
	start := time.Now()
	result := runner.runCycle(context.Background(), collector, tracker)
	elapsed := time.Since(start)
	if elapsed > time.Second {
		t.Fatalf("cycle overran its deadline: %s", elapsed)
	}
	if cycleError(result) == nil {
		t.Fatalf("expected the probe that ran into the deadline to fail")
	}
	// The target that got the only slot runs p1 into the deadline and fails;
	// its p2 and both probes of the other targets never start, and are
	// skipped, not failed.
	var failed, skipped []TargetResult
	for _, target := range result.Targets {
		switch target.State {
		case TargetFailed:
			failed = append(failed, target)
		case TargetSkipped:
			skipped = append(skipped, target)
		default:
			t.Fatalf("unexpected target state: %+v", target)
		}
	}
	if len(failed) != 1 || len(skipped) != 2 {
		t.Fatalf("expected 1 failed and 2 skipped targets, got %d and %d", len(failed), len(skipped))
	}
	for _, target := range skipped {
		if !errors.Is(target.Err, errProbeSkipped) || target.ConsecutiveFailures != 0 || !target.NextAttempt.IsZero() {
			t.Fatalf("skipped target must not back off: %+v", target)
		}
		// Its series, health ones included, keep the values of the first cycle.
		for _, probe := range []string{"p1", "p2"} {
			if _, ok := exporter.Value(testMetric, sampleLabels(target.Target, probe)); !ok {
				t.Fatalf("series of skipped %s/%s were cleared", target.Target, probe)
			}
		}
		if up, ok := exporter.Value(MetricTargetUp, map[string]string{"collector": "sql-prod", "environment": "prod", "target": target.Target}); !ok || up != 1 {
			t.Fatalf("target_up of skipped %s = %v, %v; want 1", target.Target, up, ok)
		}
	}
	if got := len(logs.records(t, "probe failed")); got != 1 {
		t.Fatalf("expected 1 probe failure log, got %d", got)
	}

	// The skipped targets run on the next cycle, with no backoff.
	slow.Store(false)
	result = runner.runCycle(context.Background(), collector, tracker)
	for _, target := range result.Targets {
		if target.Target != failed[0].Target && target.State != TargetOK {
			t.Fatalf("skipped target did not run on the next cycle: %+v", target)
		}
	}
}

// A probe that the cycle deadline leaves no time for, after a slow probe of
// the same target succeeded, is skipped: the target is degraded, not failed,
// the probe keeps its series, and the next cycle runs it first.
func TestCycleDeadlineSkippedProbeRunsFirstNextCycle(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	slow := atomic.Bool{}
	executor := funcExecutor(func(ctx context.Context, item collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
		mu.Lock()
		calls = append(calls, item.Definition.Name)
		mu.Unlock()
		if slow.Load() {
			select {
			case <-time.After(110 * time.Millisecond):
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}
		return sampleFor(item), nil, nil
	})
	exporter := collectorexport.NewInMemoryExporter()
	runner := NewRunner(executor, exporter, nil).WithLogger(slog.New(slog.DiscardHandler))
	target := testTarget("t1", "p1", "p2", "p3")
	for i := range target.Probes {
		target.Probes[i].TimeoutMS = 120
	}
	collector := testCollector(200*time.Millisecond, target)
	tracker := newTargetTracker()
	tracker.jitter = func() float64 { return 0 }
	runner.runCycle(context.Background(), collector, tracker)

	// p1 succeeds in 110ms, p2 runs into the 200ms cycle deadline, p3 never
	// starts.
	slow.Store(true)
	mu.Lock()
	calls = nil
	mu.Unlock()
	got := runner.runCycle(context.Background(), collector, tracker).Targets[0]
	if got.State != TargetDegraded {
		t.Fatalf("expected a degraded target, got %+v", got)
	}
	states := map[string]ProbeState{}
	for _, probe := range got.Probes {
		states[probe.Probe] = probe.State
	}
	if states["p2"] != ProbeFailed || states["p3"] != ProbeSkipped || len(states) != 2 {
		t.Fatalf("probe states = %v; want p2 failed, p3 skipped", states)
	}
	if _, ok := exporter.Value(testMetric, sampleLabels("t1", "p3")); !ok {
		t.Fatalf("series of the skipped probe were cleared")
	}
	if up, ok := exporter.Value(MetricProbeUp, map[string]string{"collector": "sql-prod", "environment": "prod", "target": "t1", "probe": "p3"}); !ok || up != 1 {
		t.Fatalf("probe_up of the skipped probe = %v, %v; want its last run's 1", up, ok)
	}
	if up, _ := exporter.Value(MetricProbeUp, map[string]string{"collector": "sql-prod", "environment": "prod", "target": "t1", "probe": "p2"}); up != 0 {
		t.Fatalf("probe_up of the failed probe = %v; want 0", up)
	}
	if err := cycleError(CycleResult{Targets: []TargetResult{got}}); err == nil || errors.Is(err, errProbeSkipped) {
		t.Fatalf("cycle error must name p2 only, got %v", err)
	}

	slow.Store(false)
	mu.Lock()
	calls = nil
	mu.Unlock()
	if got := runner.runCycle(context.Background(), collector, tracker).Targets[0]; got.State != TargetDegraded && got.State != TargetOK {
		t.Fatalf("third cycle: %+v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) == 0 || calls[0] != "p3" {
		t.Fatalf("expected the skipped probe to run first, got calls %v", calls)
	}
}

// unsafeSink is deliberately not concurrency-safe so -race catches unserialised Publish calls.
type unsafeSink struct {
	evidence []collectormetadata.Evidence
}

func (s *unsafeSink) Publish(items []collectormetadata.Evidence) error {
	s.evidence = append(s.evidence, items...)
	return nil
}

func TestConcurrencyBoundAndSerialProbesPerTarget(t *testing.T) {
	for _, limit := range []int{0, 3} {
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			var inFlight, maxInFlight atomic.Int32
			var perTarget sync.Map
			executor := funcExecutor(func(_ context.Context, item collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
				counter, _ := perTarget.LoadOrStore(item.Target.Name, new(atomic.Int32))
				if n := counter.(*atomic.Int32).Add(1); n > 1 {
					return nil, nil, fmt.Errorf("%d concurrent probes on %s", n, item.Target.Name)
				}
				defer counter.(*atomic.Int32).Add(-1)
				n := inFlight.Add(1)
				defer inFlight.Add(-1)
				for {
					current := maxInFlight.Load()
					if n <= current || maxInFlight.CompareAndSwap(current, n) {
						break
					}
				}
				time.Sleep(5 * time.Millisecond)
				return sampleFor(item), []collectormetadata.Evidence{{Kind: "blocking", Title: item.Target.Name}}, nil
			})
			sink := &unsafeSink{}
			runner := NewRunner(executor, collectorexport.NewInMemoryExporter(), sink).
				WithLogger(slog.New(slog.DiscardHandler)).
				WithMaxConcurrentTargets(limit)
			var targets []collectorconfig.TargetRuntimeConfig
			for i := range 20 {
				targets = append(targets, testTarget(fmt.Sprintf("db-%02d", i), "p1", "p2"))
			}

			if _, err := runner.RunOnce(context.Background(), testCollector(time.Minute, targets...)); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			want := int32(limit)
			if limit == 0 {
				want = DefaultMaxConcurrentTargets
			}
			if got := maxInFlight.Load(); got > want || got < 2 {
				t.Fatalf("max in-flight targets = %d, want between 2 and %d", got, want)
			}
			if len(sink.evidence) != 40 {
				t.Fatalf("expected 40 evidence items, got %d", len(sink.evidence))
			}
		})
	}
}

func TestProbePanicIsRecoveredAndIsolated(t *testing.T) {
	logger, logs := newCapturedLogger()
	exporter := collectorexport.NewInMemoryExporter()
	executor := funcExecutor(func(_ context.Context, item collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
		if item.Target.Name == "boom" && item.Definition.Name == "waits" {
			panic("nil map write")
		}
		return sampleFor(item), nil, nil
	})
	runner := NewRunner(executor, exporter, nil).WithLogger(logger)

	result, err := runner.RunOnce(context.Background(), testCollector(time.Minute,
		testTarget("boom", "waits", "sessions"),
		testTarget("fine", "waits"),
	))
	if err == nil || !strings.Contains(err.Error(), "panicked: nil map write") {
		t.Fatalf("expected panic error, got %v", err)
	}
	// The panic fails its probe only: the target's other probe ran.
	if result.Targets[0].State != TargetDegraded || result.Targets[1].State != TargetOK ||
		len(result.Targets[0].Probes) != 1 || result.Targets[0].Probes[0].Probe != "waits" {
		t.Fatalf("unexpected results: %+v", result.Targets)
	}
	if _, ok := exporter.Value(testMetric, sampleLabels("boom", "sessions")); !ok {
		t.Fatalf("probe after the panicking one did not run")
	}
	panics := logs.records(t, "recovered panic")
	if len(panics) != 1 || !strings.Contains(fmt.Sprint(panics[0]["stack"]), "goroutine") || panics[0]["target"] != "boom" {
		t.Fatalf("expected one panic log with stack, got %v", panics)
	}
	if got := len(logs.records(t, "probe failed")); got != 1 {
		t.Fatalf("expected 1 probe failure log, got %d", got)
	}
}

// reportLog collects cycle reports safely.
type reportLog struct {
	mu      sync.Mutex
	results []CycleResult
}

func (r *reportLog) add(result CycleResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.results = append(r.results, result)
}

func (r *reportLog) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.results)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPollerKeepsRunningAfterErrorsAndForgetsSeriesOnCancel(t *testing.T) {
	exporter := collectorexport.NewInMemoryExporter()
	var goodRuns atomic.Int32
	executor := funcExecutor(func(_ context.Context, item collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
		if item.Target.Name == "bad" {
			return nil, nil, errors.New("connection refused")
		}
		goodRuns.Add(1)
		return sampleFor(item), nil, nil
	})
	reports := &reportLog{}
	poller := Poller{
		Runner:    NewRunner(executor, exporter, nil).WithLogger(slog.New(slog.DiscardHandler)),
		Collector: testCollector(10*time.Millisecond, testTarget("bad", "waits"), testTarget("good", "waits")),
		Report:    reports.add,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- poller.Start(ctx) }()

	waitFor(t, "several cycles", func() bool { return goodRuns.Load() >= 5 && reports.len() >= 5 })
	select {
	case err := <-done:
		t.Fatalf("poller exited early: %v", err)
	default:
	}
	if _, ok := exporter.Value(testMetric, sampleLabels("good", "waits")); !ok {
		t.Fatalf("expected good target sample while running")
	}
	reports.mu.Lock()
	sawBackoff := false
	for _, result := range reports.results {
		for _, target := range result.Targets {
			if target.Target == "bad" && target.State == TargetBackoff {
				sawBackoff = true
			}
		}
	}
	reports.mu.Unlock()
	if !sawBackoff {
		t.Fatalf("expected the failing target to back off")
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("poller did not exit after cancel")
	}
	if _, ok := exporter.Value(testMetric, sampleLabels("good", "waits")); ok {
		t.Fatalf("probe series survived poller shutdown")
	}
	if _, ok := exporter.Value(MetricTargetUp, healthLabels("good")); ok {
		t.Fatalf("health series survived poller shutdown")
	}
}

func TestPollerSurvivesPanickingReport(t *testing.T) {
	logger, logs := newCapturedLogger()
	var reports atomic.Int32
	poller := Poller{
		Runner:    NewRunner(fakeExecutor{}, collectorexport.NewInMemoryExporter(), nil).WithLogger(logger),
		Collector: testCollector(5*time.Millisecond, testTarget("core-db", "waits")),
		Report: func(CycleResult) {
			reports.Add(1)
			panic("report exploded")
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- poller.Start(ctx) }()
	waitFor(t, "repeated reports", func() bool { return reports.Load() >= 3 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if got := len(logs.records(t, "recovered panic")); got < 3 {
		t.Fatalf("expected a panic log per report, got %d", got)
	}
}

func TestPollerRejectsNonPositiveInterval(t *testing.T) {
	poller := Poller{Runner: NewRunner(fakeExecutor{}, nil, nil), Collector: testCollector(0, testTarget("core-db", "waits"))}
	if err := poller.Start(context.Background()); err == nil {
		t.Fatalf("expected error for zero interval")
	}
}

// failingRecorder is a plain Recorder whose writes always fail.
type failingRecorder struct{}

func (failingRecorder) Record([]collectorexport.Sample) error { return errors.New("label set changed") }

// failingSink rejects every publish.
type failingSink struct{}

func (failingSink) Publish([]collectormetadata.Evidence) error { return errors.New("sink unavailable") }

func TestExporterAndSinkErrorsAreLogged(t *testing.T) {
	logger, logs := newCapturedLogger()
	executor := funcExecutor(func(_ context.Context, item collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
		return sampleFor(item), []collectormetadata.Evidence{{Kind: "blocking"}}, nil
	})
	runner := NewRunner(executor, failingRecorder{}, failingSink{}).WithLogger(logger)
	result, err := runner.RunOnce(context.Background(), testCollector(time.Minute, testTarget("core-db", "waits")))
	if err != nil || !result.Healthy() {
		t.Fatalf("exporter errors must not fail the target: %v %+v", err, result)
	}
	// Probe samples, target health, and cycle duration each fail to record.
	if got := len(logs.records(t, "record samples failed")); got != 3 {
		t.Fatalf("expected 3 exporter error logs, got %d", got)
	}
	if got := len(logs.records(t, "publish evidence failed")); got != 1 {
		t.Fatalf("expected 1 sink error log, got %d", got)
	}
}

func TestProbeFailureClearsStaleSeries(t *testing.T) {
	exporter := collectorexport.NewInMemoryExporter()
	var failing atomic.Bool
	executor := funcExecutor(func(_ context.Context, item collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
		if failing.Load() {
			return nil, nil, errors.New("timeout")
		}
		return sampleFor(item), nil, nil
	})
	runner := NewRunner(executor, exporter, nil).WithLogger(slog.New(slog.DiscardHandler))
	collector := testCollector(time.Minute, testTarget("core-db", "waits"))
	if _, err := runner.RunOnce(context.Background(), collector); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if _, ok := exporter.Value(testMetric, sampleLabels("core-db", "waits")); !ok {
		t.Fatalf("expected sample after success")
	}
	failing.Store(true)
	if _, err := runner.RunOnce(context.Background(), collector); err == nil {
		t.Fatalf("expected failure")
	}
	if _, ok := exporter.Value(testMetric, sampleLabels("core-db", "waits")); ok {
		t.Fatalf("stale sample survived probe failure")
	}
}

func TestNilLoggerIsSafe(t *testing.T) {
	executor := funcExecutor(func(context.Context, collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
		return nil, nil, errors.New("boom")
	})
	runner := NewRunner(executor, nil, nil).WithLogger(nil)
	if _, err := runner.RunOnce(context.Background(), testCollector(time.Minute, testTarget("core-db", "waits"))); err == nil {
		t.Fatalf("expected error")
	}
}

// A probe that returns ErrBlocked waits for downstream: its target is up and
// blocked, not failed, does not back off, and no probe error is counted.
// Another probe of the target failing still fails it.
func TestBlockedProbeIsNotAFailure(t *testing.T) {
	exporter := collectorexport.NewInMemoryExporter()
	metrics, reg := newTestProbeMetrics(t)
	failing := false
	executor := funcExecutor(func(_ context.Context, item collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
		switch {
		case item.Definition.Name == "error":
			return nil, nil, fmt.Errorf("log type error: 10 rows waiting for the OTel Collector: %w", ErrBlocked)
		case failing:
			return nil, nil, errDivideByZero
		}
		return sampleFor(item), nil, nil
	})
	runner := NewRunner(executor, exporter, nil).WithLogger(slog.New(slog.DiscardHandler)).WithProbeMetrics(metrics)
	tracker := newTargetTracker()
	clock := &fakeClock{}
	tracker.now = clock.Now
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	collector := testCollector(10*time.Second, testTarget("logging-db", "error", "general"))
	runner.initCollector(collector)

	for cycle := range 3 {
		clock.Set(t0.Add(time.Duration(cycle) * 10 * time.Second))
		result := runner.runCycle(context.Background(), collector, tracker)
		target := result.Targets[0]
		if target.State != TargetBlocked || !errors.Is(target.Err, ErrBlocked) || target.ConsecutiveFailures != 0 || !target.NextAttempt.IsZero() {
			t.Fatalf("cycle %d: target %+v, want blocked without failures or backoff", cycle, target)
		}
		if !result.Healthy() || cycleError(result) != nil {
			t.Fatalf("cycle %d: a blocked target counts as a failure: %v", cycle, cycleError(result))
		}
		if got := exporter.LastValue(MetricTargetUp); got != 1 {
			t.Fatalf("cycle %d: target_up = %v, want 1", cycle, got)
		}
	}
	for _, series := range probeSeries(t, reg, MetricProbeErrors) {
		if !strings.HasSuffix(series, "=0") {
			t.Fatalf("probe error counted for a blocked probe: %s", series)
		}
	}

	failing = true
	clock.Set(t0.Add(30 * time.Second))
	result := runner.runCycle(context.Background(), collector, tracker)
	// The blocked log type ran, so the general one fails only itself.
	if target := result.Targets[0]; target.State != TargetDegraded || !errors.Is(target.Err, errDivideByZero) ||
		len(target.Probes) != 1 || target.Probes[0].Probe != "general" || target.Probes[0].State != ProbeFailed {
		t.Fatalf("target %+v, want degraded by the general probe only", target)
	}
	if err := cycleError(result); !errors.Is(err, errDivideByZero) || errors.Is(err, ErrBlocked) {
		t.Fatalf("cycle error %v, want the general probe's failure only", err)
	}
}

// Targets take the concurrency slots in dispatch order, which starts at the
// first target the previous cycle skipped, so under sustained overload the
// skipped targets take turns rather than the same ones never running.
func TestCycleDeadlineSkippedTargetIsDispatchedFirstNextCycle(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	slow := atomic.Bool{}
	executor := funcExecutor(func(ctx context.Context, item collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
		mu.Lock()
		calls = append(calls, item.Target.Name)
		mu.Unlock()
		if slow.Load() {
			select {
			case <-time.After(150 * time.Millisecond):
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}
		return sampleFor(item), nil, nil
	})
	runner := NewRunner(executor, collectorexport.NewInMemoryExporter(), nil).
		WithLogger(slog.New(slog.DiscardHandler)).
		WithMaxConcurrentTargets(1)
	collector := testCollector(200*time.Millisecond, testTarget("t1", "p1"), testTarget("t2", "p1"), testTarget("t3", "p1"))
	for i := range collector.Targets {
		collector.Targets[i].Probes[0].TimeoutMS = 60_000 // capped at the interval
	}
	tracker := newTargetTracker()
	tracker.jitter = func() float64 { return 0 }

	// t1 takes 150ms of the 200ms cycle, t2 runs into the deadline, t3 never
	// gets the slot.
	slow.Store(true)
	result := runner.runCycle(context.Background(), collector, tracker)
	states := []TargetState{result.Targets[0].State, result.Targets[1].State, result.Targets[2].State}
	if fmt.Sprint(states) != fmt.Sprint([]TargetState{TargetOK, TargetFailed, TargetSkipped}) {
		t.Fatalf("first cycle states = %v; want ok, failed, skipped", states)
	}

	slow.Store(false)
	mu.Lock()
	calls = nil
	mu.Unlock()
	runner.runCycle(context.Background(), collector, tracker)
	mu.Lock()
	defer mu.Unlock()
	if len(calls) == 0 || calls[0] != "t3" {
		t.Fatalf("expected the skipped target dispatched first, got calls %v", calls)
	}
}
