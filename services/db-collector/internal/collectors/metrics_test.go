package collectors

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	collectorconfig "heartbeat/internal/config"
	collectorexport "heartbeat/services/db-collector/internal/export"
	collectormetadata "heartbeat/services/db-collector/internal/metadata"
)

// newTestProbeMetrics returns probe metrics registered in a fresh registry.
func newTestProbeMetrics(t *testing.T) (*ProbeMetrics, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	metrics, err := NewProbeMetrics(reg)
	if err != nil {
		t.Fatalf("NewProbeMetrics: %v", err)
	}
	return metrics, reg
}

// probeSeries returns the exported series of metric as sorted
// "target/probe[/reason]=value" strings; histograms report their count.
func probeSeries(t *testing.T, reg *prometheus.Registry, metric string) []string {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var out []string
	for _, family := range families {
		if family.GetName() != metric {
			continue
		}
		for _, m := range family.GetMetric() {
			labels := map[string]string{}
			for _, pair := range m.GetLabel() {
				labels[pair.GetName()] = pair.GetValue()
			}
			key := labels["collector"] + "/" + labels["target"] + "/" + labels["probe"]
			if reason, ok := labels["reason"]; ok {
				key += "/" + reason
			}
			value := m.GetCounter().GetValue()
			if m.GetHistogram() != nil {
				value = float64(m.GetHistogram().GetSampleCount())
			}
			out = append(out, fmt.Sprintf("%s=%v", key, value))
		}
	}
	sort.Strings(out)
	return out
}

// nonZero filters series strings down to those with a value other than 0.
func nonZero(series []string) []string {
	var out []string
	for _, s := range series {
		if !strings.HasSuffix(s, "=0") {
			out = append(out, s)
		}
	}
	return out
}

func TestProbeFailuresAreCountedByReason(t *testing.T) {
	metrics, reg := newTestProbeMetrics(t)
	executor := funcExecutor(func(ctx context.Context, item collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
		switch item.Target.Name {
		case "err":
			// Error text must never reach a label.
			return nil, nil, errors.New("login failed for user 'sa'")
		case "slow", "late":
			<-ctx.Done()
			return nil, nil, ctx.Err()
		case "boom":
			panic("nil map write")
		}
		return sampleFor(item), nil, nil
	})
	runner := NewRunner(executor, collectorexport.NewInMemoryExporter(), nil).
		WithLogger(slog.New(slog.DiscardHandler)).
		WithProbeMetrics(metrics)
	slow := testTarget("slow", "p")
	slow.Probes[0].TimeoutMS = 10
	// late/p1 runs into the cycle deadline, so late/p2 never starts.
	late := testTarget("late", "p1", "p2")
	late.Probes[0].TimeoutMS = 60_000 // capped at the interval
	collector := testCollector(200*time.Millisecond, testTarget("ok", "p"), testTarget("err", "p"), slow, testTarget("boom", "p"), late)

	if _, err := runner.RunOnce(context.Background(), collector); err == nil {
		t.Fatal("expected probe failures")
	}

	wantErrors := []string{
		"sql-prod/boom/p/panic=1",
		"sql-prod/err/p/error=1",
		"sql-prod/late/p1/timeout=1",
		"sql-prod/late/p2/not_started=1",
		"sql-prod/slow/p/timeout=1",
	}
	if got := nonZero(probeSeries(t, reg, MetricProbeErrors)); fmt.Sprint(got) != fmt.Sprint(wantErrors) {
		t.Fatalf("probe errors:\n got %v\nwant %v", got, wantErrors)
	}
	// Every started probe is timed, failed ones included; late/p2 never ran.
	wantDurations := []string{
		"sql-prod/boom/p=1",
		"sql-prod/err/p=1",
		"sql-prod/late/p1=1",
		"sql-prod/ok/p=1",
		"sql-prod/slow/p=1",
	}
	if got := probeSeries(t, reg, MetricProbeDuration); fmt.Sprint(got) != fmt.Sprint(wantDurations) {
		t.Fatalf("probe durations:\n got %v\nwant %v", got, wantDurations)
	}
}

// The error counters must exist at 0 before the first error so rate() and
// increase() count it, and must follow the poller's lifetime so a reload that
// removes a target leaves no stale series.
func TestPollerInitializesAndForgetsProbeMetrics(t *testing.T) {
	metrics, reg := newTestProbeMetrics(t)
	exporter := collectorexport.NewInMemoryExporter()
	runner := NewRunner(fakeExecutor{}, exporter, nil).
		WithLogger(slog.New(slog.DiscardHandler)).
		WithProbeMetrics(metrics)
	// Another collector's series must survive this collector's restarts.
	other := testCollector(time.Minute, testTarget("other-db", "waits"))
	other.ID = "sql-other"
	if _, err := runner.RunOnce(context.Background(), other); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	metrics.initCollector(other)

	start := func(collector collectorExpectation) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		reports := &reportLog{}
		go func() {
			done <- Poller{Runner: runner, Collector: collector.config, Report: reports.add}.Start(ctx)
		}()
		waitFor(t, "first cycle", func() bool { return reports.len() >= 1 })
		errorsSeries := probeSeries(t, reg, MetricProbeErrors)
		if fmt.Sprint(errorsSeries) != fmt.Sprint(collector.errors) {
			t.Fatalf("probe errors while running:\n got %v\nwant %v", errorsSeries, collector.errors)
		}
		durations := probeSeries(t, reg, MetricProbeDuration)
		if len(durations) != len(collector.durations) {
			t.Fatalf("probe durations while running: got %v, want series %v", durations, collector.durations)
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	}
	otherErrors := []string{
		"sql-other/other-db/waits/error=0",
		"sql-other/other-db/waits/not_started=0",
		"sql-other/other-db/waits/panic=0",
		"sql-other/other-db/waits/timeout=0",
	}
	start(collectorExpectation{
		config: testCollector(time.Minute, testTarget("a", "waits"), testTarget("b", "waits")),
		errors: append(append([]string(nil), otherErrors...),
			"sql-prod/a/waits/error=0", "sql-prod/a/waits/not_started=0", "sql-prod/a/waits/panic=0", "sql-prod/a/waits/timeout=0",
			"sql-prod/b/waits/error=0", "sql-prod/b/waits/not_started=0", "sql-prod/b/waits/panic=0", "sql-prod/b/waits/timeout=0",
		),
		durations: []string{"sql-other/other-db/waits", "sql-prod/a/waits", "sql-prod/b/waits"},
	})
	// After the stop only the other collector's series remain.
	if got := probeSeries(t, reg, MetricProbeErrors); fmt.Sprint(got) != fmt.Sprint(otherErrors) {
		t.Fatalf("probe errors after stop: %v", got)
	}
	if got := probeSeries(t, reg, MetricProbeDuration); len(got) != 1 {
		t.Fatalf("probe durations after stop: %v", got)
	}
	// A reload that removes target b restarts the poller without it.
	start(collectorExpectation{
		config: testCollector(time.Minute, testTarget("a", "waits")),
		errors: append(append([]string(nil), otherErrors...),
			"sql-prod/a/waits/error=0", "sql-prod/a/waits/not_started=0", "sql-prod/a/waits/panic=0", "sql-prod/a/waits/timeout=0",
		),
		durations: []string{"sql-other/other-db/waits", "sql-prod/a/waits"},
	})
}

// collectorExpectation is a collector config and the probe metric series
// expected while its poller runs.
type collectorExpectation struct {
	config    collectorconfig.CollectorRuntimeConfig
	errors    []string
	durations []string
}

func TestProbeMetricsRegistrationAndNilSafety(t *testing.T) {
	reg := prometheus.NewRegistry()
	if _, err := NewProbeMetrics(reg); err != nil {
		t.Fatalf("NewProbeMetrics: %v", err)
	}
	if _, err := NewProbeMetrics(reg); err == nil {
		t.Fatal("expected a duplicate registration error")
	}
	// A Runner without probe metrics, or with a zero value, records nothing
	// and does not panic.
	for _, metrics := range []*ProbeMetrics{nil, {}} {
		item := collectormetadata.ScheduledProbe{CollectorID: "c"}
		metrics.initCollector(testCollector(time.Minute, testTarget("db", "waits")))
		metrics.observe(item, time.Second)
		metrics.failed(item, ReasonError)
		metrics.forgetCollector("c")
	}
	// Invalid label values are skipped, never a panic in a target goroutine.
	metrics, reg := newTestProbeMetrics(t)
	bad := collectormetadata.ScheduledProbe{CollectorID: "c", Target: collectormetadata.DatabaseTarget{Name: "db\xff"}}
	metrics.observe(bad, time.Second)
	metrics.failed(bad, ReasonError)
	if count, err := testutil.GatherAndCount(reg); err != nil || count != 0 {
		t.Fatalf("expected no series, got %d (%v)", count, err)
	}
}

// A poller stopping for shutdown or a reload cancels its cycle; the probe in
// flight and the probes that never start did not fail on their own.
func TestCanceledCycleCountsNoProbeErrors(t *testing.T) {
	metrics, reg := newTestProbeMetrics(t)
	started := make(chan struct{})
	executor := funcExecutor(func(ctx context.Context, item collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
		close(started)
		<-ctx.Done()
		return nil, nil, ctx.Err()
	})
	runner := NewRunner(executor, collectorexport.NewInMemoryExporter(), nil).
		WithLogger(slog.New(slog.DiscardHandler)).
		WithProbeMetrics(metrics)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	if _, err := runner.RunOnce(ctx, testCollector(time.Minute, testTarget("core-db", "p1", "p2"))); err == nil {
		t.Fatal("expected the canceled cycle to fail its target")
	}
	if got := nonZero(probeSeries(t, reg, MetricProbeErrors)); len(got) != 0 {
		t.Fatalf("expected no probe errors for a canceled cycle, got %v", got)
	}
}

func TestCycleMetrics(t *testing.T) {
	metrics, reg := newTestProbeMetrics(t)
	collector := collectorconfig.CollectorRuntimeConfig{ID: "c", Enabled: true}
	metrics.initCollector(collector)
	if got := probeSeries(t, reg, MetricCycles); len(got) != len(CycleOutcomes) {
		t.Fatalf("%s series before any cycle = %v, want one per outcome at 0", MetricCycles, got)
	}

	metrics.observeCycle("c", time.Second, 15*time.Second)
	metrics.observeCycle("c", 2*time.Second, 15*time.Second)
	metrics.observeCycle("c", 15*time.Second, 15*time.Second)
	metrics.cyclePanicked("c")
	metrics.observeStartDelay("c", 3*time.Second)
	metrics.observeStartDelay("c", -time.Millisecond) // timer jitter

	want := `
# HELP heartbeat_collector_cycles_total Collection cycles by outcome (on_time, late, panic).
# TYPE heartbeat_collector_cycles_total counter
heartbeat_collector_cycles_total{collector="c",outcome="late"} 1
heartbeat_collector_cycles_total{collector="c",outcome="on_time"} 2
heartbeat_collector_cycles_total{collector="c",outcome="panic"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), MetricCycles); err != nil {
		t.Error(err)
	}
	if got := testutil.CollectAndCount(metrics.cycleTime); got != 1 {
		t.Errorf("%s has %d metrics, want 1", MetricCycleSeconds, got)
	}
	if got := testutil.CollectAndCount(metrics.startDelay); got != 1 {
		t.Errorf("%s has %d metrics, want 1", MetricCycleStartDelay, got)
	}

	metrics.forgetCollector("c")
	for _, name := range []string{MetricCycles, MetricCycleSeconds, MetricCycleStartDelay} {
		if got := probeSeries(t, reg, name); len(got) != 0 {
			t.Errorf("%s series after the collector stopped = %v", name, got)
		}
	}
}

// A Poller reports how late each cycle after the first started, and a panic
// in a cycle is counted instead of being lost.
func TestPollerRecordsCycles(t *testing.T) {
	metrics, reg := newTestProbeMetrics(t)
	runner := Runner{metrics: metrics}
	poller := Poller{Runner: runner, Collector: collectorconfig.CollectorRuntimeConfig{ID: "c", Enabled: true, ScrapeInterval: time.Minute}}
	tracker := newTargetTracker()
	now := time.Unix(1000, 0)
	tracker.now = func() time.Time { return now }

	poller.cycle(context.Background(), tracker)
	if got := testutil.CollectAndCount(metrics.startDelay); got != 0 {
		t.Errorf("the first cycle has no previous start, yet %d delay metrics", got)
	}
	now = now.Add(time.Minute + 2*time.Second)
	poller.cycle(context.Background(), tracker)
	if err := testutil.GatherAndCompare(reg, strings.NewReader(`
# HELP heartbeat_collector_cycles_total Collection cycles by outcome (on_time, late, panic).
# TYPE heartbeat_collector_cycles_total counter
heartbeat_collector_cycles_total{collector="c",outcome="on_time"} 2
`), MetricCycles); err != nil {
		t.Error(err)
	}
	if got := probeHistogramSum(t, reg, MetricCycleStartDelay); got != 2 {
		t.Errorf("start delay sum = %v, want 2", got)
	}
}

// A collector stopping ends its cycle, which says nothing about how it runs.
func TestStoppingCycleIsNotCounted(t *testing.T) {
	metrics, reg := newTestProbeMetrics(t)
	runner := Runner{metrics: metrics}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner.runCycle(ctx, collectorconfig.CollectorRuntimeConfig{ID: "c", Enabled: true, ScrapeInterval: time.Minute}, newTargetTracker())
	if got := probeSeries(t, reg, MetricCycles); len(got) != 0 {
		t.Errorf("%s series = %v, want none", MetricCycles, got)
	}
}

func probeHistogramSum(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == name {
			return family.GetMetric()[0].GetHistogram().GetSampleSum()
		}
	}
	t.Fatalf("%s not found", name)
	return 0
}
