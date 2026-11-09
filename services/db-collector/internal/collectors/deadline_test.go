package collectors

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	collectorexport "atrium/services/db-collector/internal/export"
	collectormetadata "atrium/services/db-collector/internal/metadata"
)

// testAbandonGrace keeps abandonment fast in tests.
const testAbandonGrace = 30 * time.Millisecond

// stuckExecutor blocks every probe of one target, ignoring its context like a
// driver waiting for a cancel acknowledgement on a dead connection, until it
// is released; it then returns samples and evidence that must be discarded.
// Every other probe succeeds at once.
type stuckExecutor struct {
	target  string
	release chan struct{}
	// started receives one value per stuck call, without blocking.
	started chan struct{}
	// stuckCalls and otherCalls count executor calls.
	stuckCalls atomic.Int32
	otherCalls atomic.Int32
}

func newStuckExecutor(target string) *stuckExecutor {
	return &stuckExecutor{target: target, release: make(chan struct{}), started: make(chan struct{}, 16)}
}

func (e *stuckExecutor) RunProbe(_ context.Context, item collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
	if item.Target.Name != e.target {
		e.otherCalls.Add(1)
		return sampleFor(item), nil, nil
	}
	e.stuckCalls.Add(1)
	select {
	case e.started <- struct{}{}:
	default:
	}
	<-e.release
	return sampleFor(item), []collectormetadata.Evidence{{Kind: "blocking", Title: item.Target.Name}}, nil
}

// unstick releases every stuck call, now and later, and waits until runner
// has no call in flight, so no test leaks a goroutine.  It is idempotent.
func (e *stuckExecutor) unstick(t *testing.T, runner Runner) {
	t.Helper()
	select {
	case <-e.release:
	default:
		close(e.release)
	}
	waitFor(t, "abandoned calls to return", func() bool { return callsInFlight(runner) == 0 })
}

// callsInFlight returns the number of executor calls runner has in flight.
func callsInFlight(runner Runner) int {
	calls := runner.probeCalls()
	calls.mu.Lock()
	defer calls.mu.Unlock()
	return len(calls.calls)
}

// stuckRunner returns a Runner over executor with a short abandon grace.
func stuckRunner(executor ProbeExecutor, exporter collectorexport.Recorder, sink EvidenceSink, logger *slog.Logger, metrics *ProbeMetrics) Runner {
	runner := NewRunner(executor, exporter, sink).WithLogger(logger).WithProbeMetrics(metrics)
	runner.abandonGrace = testAbandonGrace
	return runner
}

// A probe whose executor ignores its context must not hold up the cycle: it
// is abandoned at its timeout plus the grace, the rest of its target does not
// start, and every other target is collected as usual.
func TestStuckProbeIsAbandonedWithoutStallingTheCycle(t *testing.T) {
	logger, logs := newCapturedLogger()
	metrics, reg := newTestProbeMetrics(t)
	exporter := collectorexport.NewInMemoryExporter()
	executor := newStuckExecutor("stuck")
	runner := stuckRunner(executor, exporter, nil, logger, metrics)
	t.Cleanup(func() { executor.unstick(t, runner) })
	stuck := testTarget("stuck", "p1", "p2")
	stuck.Probes[0].TimeoutMS = 20
	collector := testCollector(time.Minute, testTarget("good-a", "p1", "p2"), stuck, testTarget("good-b", "p1", "p2"))

	start := time.Now()
	result, err := runner.RunOnce(context.Background(), collector)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cycle took %s, want about timeout + grace (50ms)", elapsed)
	}
	if err == nil || !errors.Is(err, errProbeAbandoned) {
		t.Fatalf("expected an abandoned probe error, got %v", err)
	}
	states := map[string]TargetResult{}
	for _, target := range result.Targets {
		states[target.Target] = target
	}
	for _, name := range []string{"good-a", "good-b"} {
		if states[name].State != TargetOK {
			t.Fatalf("healthy target %s affected by the stuck one: %+v", name, states[name])
		}
		for _, probe := range []string{"p1", "p2"} {
			if _, ok := exporter.Value(testMetric, sampleLabels(name, probe)); !ok {
				t.Fatalf("missing sample for %s/%s", name, probe)
			}
		}
	}
	got := states["stuck"]
	if got.State != TargetFailed || got.ConsecutiveFailures != 1 {
		t.Fatalf("unexpected stuck target result: %+v", got)
	}
	for _, want := range []string{
		"probe p1 timed out after 20ms and did not stop within 30ms: abandoned while still running",
		"probe p2 not started: probe p1 of the target was abandoned and is still running",
	} {
		if !strings.Contains(got.Err.Error(), want) {
			t.Fatalf("stuck target error %q is missing %q", got.Err, want)
		}
	}
	if up, _ := exporter.Value(MetricTargetUp, healthLabels("stuck")); up != 0 {
		t.Fatalf("expected stuck up=0, got %v", up)
	}
	if n := executor.stuckCalls.Load(); n != 1 {
		t.Fatalf("expected 1 call for the stuck target, got %d", n)
	}
	if n := callsInFlight(runner); n != 1 {
		t.Fatalf("expected the abandoned call in flight, got %d calls", n)
	}

	wantErrors := []string{"sql-prod/stuck/p1/timeout=1", "sql-prod/stuck/p2/not_started=1"}
	if got := nonZero(probeSeries(t, reg, MetricProbeErrors)); fmt.Sprint(got) != fmt.Sprint(wantErrors) {
		t.Fatalf("probe errors:\n got %v\nwant %v", got, wantErrors)
	}
	// The abandoned probe is observed once, at abandonment; p2 never ran.
	wantDurations := []string{
		"sql-prod/good-a/p1=1", "sql-prod/good-a/p2=1",
		"sql-prod/good-b/p1=1", "sql-prod/good-b/p2=1",
		"sql-prod/stuck/p1=1",
	}
	if got := probeSeries(t, reg, MetricProbeDuration); fmt.Sprint(got) != fmt.Sprint(wantDurations) {
		t.Fatalf("probe durations:\n got %v\nwant %v", got, wantDurations)
	}
	if got := len(logs.records(t, "probe failed")); got != 2 {
		t.Fatalf("expected 2 probe failure logs, got %d", got)
	}
}

// A target keeps at most one call in flight across cycles, the abandoned
// call's late result is discarded, and the target recovers once the call has
// returned.
func TestAbandonedProbeBlocksItsTargetUntilItReturns(t *testing.T) {
	logger, logs := newCapturedLogger()
	metrics, reg := newTestProbeMetrics(t)
	exporter := collectorexport.NewInMemoryExporter()
	sink := &unsafeSink{}
	executor := newStuckExecutor("stuck")
	runner := stuckRunner(executor, exporter, sink, logger, metrics)
	t.Cleanup(func() { executor.unstick(t, runner) })
	stuck := testTarget("stuck", "p1")
	stuck.Probes[0].TimeoutMS = 10
	collector := testCollector(10*time.Second, stuck, testTarget("good", "p1"))
	clock := &fakeClock{}
	tracker := newTargetTracker()
	tracker.now = clock.Now
	tracker.jitter = func() float64 { return 0 }
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cycle := func(offset time.Duration) TargetResult {
		t.Helper()
		clock.Set(t0.Add(offset))
		result := runner.runCycle(context.Background(), collector, tracker)
		if result.Targets[1].State != TargetOK {
			t.Fatalf("at %s: healthy target affected: %+v", offset, result.Targets[1])
		}
		return result.Targets[0]
	}

	if got := cycle(0); got.State != TargetFailed || !errors.Is(got.Err, errProbeAbandoned) {
		t.Fatalf("first cycle: expected the stuck probe abandoned, got %+v", got)
	}
	// The first failure retries on the next cycle, but the abandoned call is
	// still running, so no second call starts.
	if got := cycle(10 * time.Second); got.State != TargetFailed || got.ConsecutiveFailures != 2 ||
		!strings.Contains(got.Err.Error(), "not started: probe p1 of the target was abandoned") {
		t.Fatalf("second cycle: expected the probe not started, got %+v", got)
	}
	if n := executor.stuckCalls.Load(); n != 1 {
		t.Fatalf("expected 1 call for the stuck target, got %d", n)
	}

	// The late result arrives: nothing is recorded, published, or counted.
	executor.unstick(t, runner)
	if _, ok := exporter.Value(testMetric, sampleLabels("stuck", "p1")); ok {
		t.Fatalf("late result of the abandoned call was recorded")
	}
	if len(sink.evidence) != 0 {
		t.Fatalf("late evidence of the abandoned call was published: %+v", sink.evidence)
	}
	wantErrors := []string{"sql-prod/stuck/p1/not_started=1", "sql-prod/stuck/p1/timeout=1"}
	if got := nonZero(probeSeries(t, reg, MetricProbeErrors)); fmt.Sprint(got) != fmt.Sprint(wantErrors) {
		t.Fatalf("probe errors:\n got %v\nwant %v", got, wantErrors)
	}
	// The call is unregistered before its goroutine logs, so wait for the
	// log rather than read it right after unstick.
	waitFor(t, "the abandoned call's return to be logged", func() bool {
		return len(logs.records(t, "abandoned probe returned; result discarded")) > 0
	})
	returned := logs.records(t, "abandoned probe returned; result discarded")
	if len(returned) != 1 || returned[0]["target"] != "stuck" || returned[0]["probe"] != "p1" || returned[0]["running"] == nil {
		t.Fatalf("expected one log for the returned abandoned call, got %v", returned)
	}

	// After its backoff the target runs again and recovers.
	if got := cycle(30 * time.Second); got.State != TargetOK {
		t.Fatalf("expected the target to recover, got %+v", got)
	}
	if n := executor.stuckCalls.Load(); n != 2 {
		t.Fatalf("expected a second call after the first returned, got %d", n)
	}
	if _, ok := exporter.Value(testMetric, sampleLabels("stuck", "p1")); !ok {
		t.Fatalf("missing sample after recovery")
	}
	if len(sink.evidence) != 1 {
		t.Fatalf("expected the recovered call's evidence only, got %+v", sink.evidence)
	}
	if got := probeSeries(t, reg, MetricProbeDuration); fmt.Sprint(got) != "[sql-prod/good/p1=3 sql-prod/stuck/p1=2]" {
		t.Fatalf("probe durations: %v", got)
	}
}

// The original failure: one stuck probe stalled every later cycle, so healthy
// targets stopped being collected.  The Poller must keep its interval, and a
// replacement Poller sharing the Runner, as after a reload, must not start a
// second call for the stuck target.
func TestPollerKeepsCollectingWhileAProbeIsStuck(t *testing.T) {
	executor := newStuckExecutor("stuck")
	runner := stuckRunner(executor, collectorexport.NewInMemoryExporter(), nil, slog.New(slog.DiscardHandler), nil)
	t.Cleanup(func() { executor.unstick(t, runner) })
	stuck := testTarget("stuck", "p1")
	stuck.Probes[0].TimeoutMS = 5
	collector := testCollector(20*time.Millisecond, stuck, testTarget("good", "p1"))

	for run := range 2 {
		before := executor.otherCalls.Load()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- Poller{Runner: runner, Collector: collector}.Start(ctx) }()
		waitFor(t, "healthy target collected every cycle", func() bool { return executor.otherCalls.Load() >= before+5 })
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("poller %d did not stop", run)
		}
	}
	if n := executor.stuckCalls.Load(); n != 1 {
		t.Fatalf("expected 1 call for the stuck target across both pollers, got %d", n)
	}
}

// Stopping a Poller cancels its cycle; a probe that ignores the cancellation
// is abandoned after the grace, so shutdown and reloads stay bounded, and the
// cancellation is not counted as a probe error.
func TestPollerStopIsBoundedByTheAbandonGrace(t *testing.T) {
	logger, logs := newCapturedLogger()
	metrics, reg := newTestProbeMetrics(t)
	exporter := collectorexport.NewInMemoryExporter()
	executor := newStuckExecutor("stuck")
	runner := stuckRunner(executor, exporter, nil, logger, metrics)
	t.Cleanup(func() { executor.unstick(t, runner) })
	// The probe timeout (10s) is far beyond the test; only the stop ends it.
	poller := Poller{Runner: runner, Collector: testCollector(time.Minute, testTarget("stuck", "p1"))}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- poller.Start(ctx) }()
	select {
	case <-executor.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the stuck probe never started")
	}

	stopped := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("poller stop waited for the stuck probe")
	}
	if elapsed := time.Since(stopped); elapsed > time.Second {
		t.Fatalf("poller stop took %s, want about the grace (30ms)", elapsed)
	}
	failed := logs.records(t, "probe failed")
	if len(failed) != 1 || !strings.Contains(fmt.Sprint(failed[0]["error"]), "probe p1 was canceled and did not stop within 30ms") {
		t.Fatalf("expected the canceled probe logged as abandoned, got %v", failed)
	}
	// The collector's series are deleted on stop, and the late result must
	// not bring them back.
	executor.unstick(t, runner)
	if _, ok := exporter.Value(testMetric, sampleLabels("stuck", "p1")); ok {
		t.Fatalf("late result recorded after the poller stopped")
	}
	if got := probeSeries(t, reg, MetricProbeErrors); len(got) != 0 {
		t.Fatalf("probe metrics survived the stop: %v", got)
	}
}

func TestProbeCalls(t *testing.T) {
	item := func(target, probe string) collectormetadata.ScheduledProbe {
		return collectormetadata.ScheduledProbe{
			CollectorID: "sql-prod",
			Target:      collectormetadata.DatabaseTarget{Name: target},
			Definition:  collectormetadata.ProbeDefinition{Name: probe},
		}
	}
	calls := newProbeCalls()
	now := time.Now()
	first, busy := calls.start(item("db", "p1"), now)
	if first == nil || busy != nil {
		t.Fatalf("first start: call=%v busy=%v", first, busy)
	}
	if call, busy := calls.start(item("db", "p2"), now); call != nil || busy == nil || busy.probe != "p1" || busy.abandoned {
		t.Fatalf("second start on the same target: call=%v busy=%+v, want p1 in flight", call, busy)
	}
	if err := busyError("p2", probeCall{probe: "p1", started: now}); !strings.Contains(err.Error(), "probe p1 of the target is still in flight") {
		t.Fatalf("busy error for a call in flight: %v", err)
	}
	other := item("db", "p1")
	other.CollectorID = "sql-other"
	if call, busy := calls.start(other, now); call == nil || busy != nil {
		t.Fatalf("same target name in another collector must not be busy: call=%v busy=%v", call, busy)
	}
	if !calls.abandon(first) {
		t.Fatal("abandon of a running call reported it finished")
	}
	if _, busy := calls.start(item("db", "p2"), now); busy == nil || !busy.abandoned {
		t.Fatalf("start after abandon: busy=%+v, want the abandoned call", busy)
	}
	if abandoned := calls.finish(item("db", "p1"), first); !abandoned {
		t.Fatal("finish did not report the abandonment")
	}
	if calls.abandon(first) {
		t.Fatal("abandon of a finished call reported it running")
	}
	next, busy := calls.start(item("db", "p2"), now)
	if next == nil || busy != nil {
		t.Fatalf("start after finish: call=%v busy=%v", next, busy)
	}
	if abandoned := calls.finish(item("db", "p2"), next); abandoned {
		t.Fatal("finish reported a call that was never abandoned")
	}
}

// Regression: the executor goroutine used to send the result before
// unregistering the call, so the target's next probe could find the call
// still in flight and be refused, at random.  The hook holds the goroutine
// after the send, which made that deterministic.
func TestReturnedCallIsUnregisteredBeforeTheNextProbeStarts(t *testing.T) {
	release := make(chan struct{})
	var held atomic.Bool
	runner := NewRunner(funcExecutor(func(_ context.Context, item collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
		return sampleFor(item), nil, nil
	}), collectorexport.NewInMemoryExporter(), nil).WithLogger(slog.New(slog.DiscardHandler))
	runner.testHookCallSent = func() {
		if held.CompareAndSwap(false, true) {
			<-release
		}
	}
	t.Cleanup(func() {
		close(release)
		waitFor(t, "held call to finish", func() bool { return callsInFlight(runner) == 0 })
	})
	if _, err := runner.RunOnce(context.Background(), testCollector(time.Minute, testTarget("core-db", "p1", "p2", "p3"))); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !held.Load() {
		t.Fatal("the hook never held a call")
	}
}

// A probe that reached its own timeout stays a timeout when the poller starts
// stopping during the abandon grace: why the probe ended is taken when its
// context ended, not when the Runner stops waiting.
func TestProbeTimeoutKeepsItsCauseWhenTheCycleIsCanceledDuringTheGrace(t *testing.T) {
	tests := []struct {
		name string
		// returns makes the executor return once the cycle is canceled
		// instead of ignoring its context until released.
		returns bool
		want    string
	}{
		{"abandoned", false, "probe p1 timed out after 20ms and did not stop within 30ms: abandoned while still running"},
		{"returned", true, "probe p1 timed out after 20ms: query probe: context deadline exceeded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metrics, reg := newTestProbeMetrics(t)
			ctx, cancelCycle := context.WithCancel(context.Background())
			defer cancelCycle()
			release := make(chan struct{})
			executor := funcExecutor(func(probeCtx context.Context, _ collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
				<-probeCtx.Done()
				// The poller stops while the Runner is still waiting.
				cancelCycle()
				if !tt.returns {
					<-release
				}
				return nil, nil, fmt.Errorf("query probe: %w", probeCtx.Err())
			})
			runner := stuckRunner(executor, collectorexport.NewInMemoryExporter(), nil, slog.New(slog.DiscardHandler), metrics)
			t.Cleanup(func() {
				close(release)
				waitFor(t, "the probe call to return", func() bool { return callsInFlight(runner) == 0 })
			})
			target := testTarget("core-db", "p1")
			target.Probes[0].TimeoutMS = 20

			_, err := runner.RunOnce(ctx, testCollector(time.Minute, target))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}
			want := []string{"sql-prod/core-db/p1/timeout=1"}
			if got := nonZero(probeSeries(t, reg, MetricProbeErrors)); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("probe errors:\n got %v\nwant %v", got, want)
			}
		})
	}
}

// A driver that gives up on a silent socket reports a network timeout, not a
// context error; it is still a timeout.
func TestNetworkTimeoutIsCountedAsTimeout(t *testing.T) {
	metrics, reg := newTestProbeMetrics(t)
	readTimeout := &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
	executor := funcExecutor(func(_ context.Context, item collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
		if item.Definition.Name == "slow" {
			return nil, nil, fmt.Errorf("query probe: %w", readTimeout)
		}
		return nil, nil, errors.New("login failed")
	})
	runner := NewRunner(executor, collectorexport.NewInMemoryExporter(), nil).WithLogger(slog.New(slog.DiscardHandler)).WithProbeMetrics(metrics)
	if _, err := runner.RunOnce(context.Background(), testCollector(time.Minute, testTarget("a", "slow"), testTarget("b", "broken"))); err == nil {
		t.Fatal("expected the probes to fail")
	}
	want := []string{"sql-prod/a/slow/timeout=1", "sql-prod/b/broken/error=1"}
	if got := nonZero(probeSeries(t, reg, MetricProbeErrors)); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("probe errors:\n got %v\nwant %v", got, want)
	}
}

func TestFailureReason(t *testing.T) {
	netTimeout := fmt.Errorf("query probe: %w", &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded})
	netRefused := fmt.Errorf("query probe: %w", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")})
	tests := []struct {
		name string
		end  probeEnd
		err  error
		want string
	}{
		{"own timeout", probeTimedOut, context.DeadlineExceeded, ReasonTimeout},
		{"cycle deadline", probeCycleDeadline, context.DeadlineExceeded, ReasonTimeout},
		{"network timeout before the deadline", probeRunning, netTimeout, ReasonTimeout},
		{"other network error", probeRunning, netRefused, ReasonError},
		{"query error", probeRunning, errors.New("invalid object name"), ReasonError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := failureReason(tt.end, tt.err); got != tt.want {
				t.Fatalf("failureReason(%d, %v) = %s, want %s", tt.end, tt.err, got, tt.want)
			}
		})
	}
}

func TestProbeEndOf(t *testing.T) {
	newProbeCtx := func(parent context.Context, timeout time.Duration) context.Context {
		ctx, cancel := context.WithTimeoutCause(parent, timeout, errProbeTimedOut)
		t.Cleanup(cancel)
		return ctx
	}
	if got := probeEndOf(newProbeCtx(context.Background(), time.Minute)); got != probeRunning {
		t.Fatalf("running probe: got %d", got)
	}
	own := newProbeCtx(context.Background(), time.Nanosecond)
	<-own.Done()
	if got := probeEndOf(own); got != probeTimedOut {
		t.Fatalf("own timeout: got %d", got)
	}
	cycle, cancelCycle := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancelCycle()
	atDeadline := newProbeCtx(cycle, time.Minute)
	<-atDeadline.Done()
	if got := probeEndOf(atDeadline); got != probeCycleDeadline {
		t.Fatalf("cycle deadline: got %d", got)
	}
	stopping, stop := context.WithCancel(context.Background())
	canceled := newProbeCtx(stopping, time.Minute)
	stop()
	if got := probeEndOf(canceled); got != probeCanceled {
		t.Fatalf("canceled: got %d", got)
	}
	// Once ended, a probe keeps its cause when its cycle ends later.
	later, cancelLater := context.WithCancel(context.Background())
	first := newProbeCtx(later, time.Nanosecond)
	<-first.Done()
	cancelLater()
	if got := probeEndOf(first); got != probeTimedOut {
		t.Fatalf("own timeout then cancel: got %d", got)
	}
}

// An executor that ends its goroutine with runtime.Goexit, as t.FailNow
// does, must not leave its call registered: the target would never run
// again.
func TestExecutorGoexitUnregistersTheCall(t *testing.T) {
	var calls atomic.Int32
	executor := funcExecutor(func(_ context.Context, item collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
		if calls.Add(1) == 1 {
			runtime.Goexit()
		}
		return sampleFor(item), nil, nil
	})
	exporter := collectorexport.NewInMemoryExporter()
	runner := NewRunner(executor, exporter, nil).WithLogger(slog.New(slog.DiscardHandler))
	collector := testCollector(time.Minute, testTarget("core-db", "p1"))

	if _, err := runner.RunOnce(context.Background(), collector); !errors.Is(err, errExecutorExited) {
		t.Fatalf("expected the exited call reported, got %v", err)
	}
	if n := callsInFlight(runner); n != 0 {
		t.Fatalf("exited call still registered: %d calls in flight", n)
	}
	if _, err := runner.RunOnce(context.Background(), collector); err != nil {
		t.Fatalf("target did not run again after the exited call: %v", err)
	}
	if _, ok := exporter.Value(testMetric, sampleLabels("core-db", "p1")); !ok {
		t.Fatal("missing sample after the exited call")
	}
}
