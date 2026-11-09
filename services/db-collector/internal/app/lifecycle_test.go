package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	atriumconfig "atrium/internal/config"
	"atrium/services/db-collector/internal/collectors"
)

func testLifecycle(t *testing.T, pollers *fakePollers, backoff restartBackoff) *pollerLifecycle {
	t.Helper()
	lifecycle := newPollerLifecycleWithRun(context.Background(), nil, pollers.run)
	lifecycle.backoff = backoff
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = lifecycle.shutdown(ctx)
	})
	return lifecycle
}

func collectorConfig(id, env string) atriumconfig.CollectorRuntimeConfig {
	return atriumconfig.CollectorRuntimeConfig{ID: id, Kind: "sqlserver", Enabled: true, Environment: env, ScrapeInterval: time.Second}
}

func stateOf(l *pollerLifecycle, id string) (collectorState, bool) {
	for _, state := range l.states() {
		if state.Collector.ID == id {
			return state, true
		}
	}
	return collectorState{}, false
}

func TestRestartBackoffDoublesToMax(t *testing.T) {
	b := restartBackoff{initial: time.Second, max: time.Minute}
	for crashes, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 4: 8 * time.Second, 7: time.Minute, 50: time.Minute} {
		if got := b.delay(crashes); got != want {
			t.Fatalf("delay(%d) = %s, want %s", crashes, got, want)
		}
	}
}

func TestPollerCrashIsRestartedWithBackoff(t *testing.T) {
	pollers := newFakePollers()
	var runs atomic.Int32
	healthy := healthyPoller("a", collectors.TargetOK)
	pollers.set("a", func(ctx context.Context, report func(collectors.CycleResult)) error {
		switch runs.Add(1) {
		case 1, 3:
			panic("boom")
		case 2:
			return nil // returned while still active: a crash
		default:
			return healthy(ctx, report)
		}
	})
	lifecycle := testLifecycle(t, pollers, restartBackoff{initial: 20 * time.Millisecond, max: 80 * time.Millisecond})
	if err := lifecycle.Start(context.Background(), collectorConfig("a", "prod")); err != nil {
		t.Fatal(err)
	}
	eventually(t, 2*time.Second, func() bool {
		state, _ := stateOf(lifecycle, "a")
		return state.Phase == phaseRunning && state.Restarts == 3 && state.HasCycle
	}, "collector did not recover after crashes: %+v", lazy(func() any { return lifecycle.states() }))

	pollers.mu.Lock()
	starts := append([]time.Time(nil), pollers.starts["a"]...)
	pollers.mu.Unlock()
	for i, want := range []time.Duration{20 * time.Millisecond, 40 * time.Millisecond, 80 * time.Millisecond} {
		if gap := starts[i+1].Sub(starts[i]); gap < want*9/10 {
			t.Fatalf("restart %d after %s, want >= %s", i+1, gap, want)
		}
	}
	if state, _ := stateOf(lifecycle, "a"); state.Crashes != 0 {
		t.Fatalf("completed cycle did not reset crash count: %d", state.Crashes)
	}
}

func TestCrashedPollerDoesNotAffectOthers(t *testing.T) {
	pollers := newFakePollers()
	pollers.set("bad", func(context.Context, func(collectors.CycleResult)) error { panic("boom") })
	lifecycle := testLifecycle(t, pollers, restartBackoff{initial: time.Hour, max: time.Hour})
	for _, id := range []string{"bad", "good"} {
		if err := lifecycle.Start(context.Background(), collectorConfig(id, "prod")); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, time.Second, func() bool {
		bad, _ := stateOf(lifecycle, "bad")
		good, _ := stateOf(lifecycle, "good")
		return bad.Phase == phaseBackingOff && !bad.NextRestart.IsZero() && good.Phase == phaseRunning && good.HasCycle
	}, "unexpected states: %+v", lazy(func() any { return lifecycle.states() }))
	if !lifecycle.needsRestart("bad") || lifecycle.needsRestart("good") {
		t.Fatal("needsRestart must flag only the crashed collector")
	}
}

func TestRepeatedCrashesAreReportedFailed(t *testing.T) {
	pollers := newFakePollers()
	pollers.set("a", func(context.Context, func(collectors.CycleResult)) error { panic("boom") })
	// A growing backoff keeps the collector failed between restarts: with a
	// flat 1ms backoff the phase flipped back to running every millisecond and
	// polling could miss the failed window on a loaded runner. Reaching
	// failedAfterCrashes takes 1+2+4+8ms; the fifth wait is 16ms and doubles.
	lifecycle := testLifecycle(t, pollers, restartBackoff{initial: time.Millisecond, max: time.Hour})
	if err := lifecycle.Start(context.Background(), collectorConfig("a", "prod")); err != nil {
		t.Fatal(err)
	}
	eventually(t, 2*time.Second, func() bool {
		state, _ := stateOf(lifecycle, "a")
		return state.Phase == phaseFailed && state.Crashes >= failedAfterCrashes
	}, "collector not marked failed: %+v", lazy(func() any { return lifecycle.states() }))
}

func TestPollerConfigErrorMarksFailedWithoutRestartLoop(t *testing.T) {
	pollers := newFakePollers()
	pollers.set("a", func(context.Context, func(collectors.CycleResult)) error {
		return errors.New("scrape interval must be positive")
	})
	lifecycle := testLifecycle(t, pollers, restartBackoff{initial: time.Millisecond, max: time.Millisecond})
	if err := lifecycle.Start(context.Background(), collectorConfig("a", "prod")); err != nil {
		t.Fatal(err)
	}
	eventually(t, time.Second, func() bool {
		state, _ := stateOf(lifecycle, "a")
		return state.Phase == phaseFailed
	}, "collector not marked failed: %+v", lazy(func() any { return lifecycle.states() }))
	time.Sleep(50 * time.Millisecond)
	if n := pollers.startCount("a"); n != 1 {
		t.Fatalf("config error was restart-looped %d times", n)
	}
}

func TestUpdateStartsNewPollerOnlyAfterOldExits(t *testing.T) {
	pollers := newFakePollers()
	var oldExited atomic.Int64
	var runs atomic.Int32
	pollers.set("a", func(ctx context.Context, report func(collectors.CycleResult)) error {
		if runs.Add(1) > 1 {
			return healthyPoller("a", collectors.TargetOK)(ctx, report)
		}
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond) // in-flight probe + exporter cleanup
		oldExited.Store(time.Now().UnixNano())
		return ctx.Err()
	})
	lifecycle := testLifecycle(t, pollers, defaultRestartBackoff)
	if err := lifecycle.Start(context.Background(), collectorConfig("a", "prod")); err != nil {
		t.Fatal(err)
	}
	eventually(t, time.Second, func() bool { return pollers.startCount("a") == 1 }, "poller not started")
	if err := lifecycle.Update(context.Background(), collectorConfig("a", "staging")); err != nil {
		t.Fatal(err)
	}
	eventually(t, time.Second, func() bool { return pollers.startCount("a") == 2 }, "replacement poller not started")
	pollers.mu.Lock()
	newStart := pollers.starts["a"][1]
	pollers.mu.Unlock()
	if exited := oldExited.Load(); exited == 0 || newStart.UnixNano() < exited {
		t.Fatal("new poller started before the old poller exited")
	}
}

func TestStopHonoursDeadlineAndDiscardsCancelledReports(t *testing.T) {
	pollers := newFakePollers()
	release := make(chan struct{})
	pollers.set("a", func(ctx context.Context, report func(collectors.CycleResult)) error {
		now := time.Now()
		report(collectors.CycleResult{CollectorID: "a", Finished: now, Targets: []collectors.TargetResult{{Target: "core-db", State: collectors.TargetOK}}})
		<-ctx.Done()
		// The in-flight cycle fails with context.Canceled after cancellation.
		report(collectors.CycleResult{CollectorID: "a", Finished: time.Now(), Targets: []collectors.TargetResult{{Target: "core-db", State: collectors.TargetFailed, Err: ctx.Err()}}})
		<-release // ignores cancellation, like a hung driver call
		return ctx.Err()
	})
	lifecycle := testLifecycle(t, pollers, defaultRestartBackoff)
	t.Cleanup(func() { close(release) }) // runs before the lifecycle shutdown cleanup
	if err := lifecycle.Start(context.Background(), collectorConfig("a", "prod")); err != nil {
		t.Fatal(err)
	}
	eventually(t, time.Second, func() bool { s, _ := stateOf(lifecycle, "a"); return s.HasCycle }, "no cycle")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := lifecycle.Stop(ctx, "a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline error, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("Stop ignored its deadline")
	}
	state, ok := stateOf(lifecycle, "a")
	if !ok || state.Phase != phaseStopping {
		t.Fatalf("timed-out stop must leave the entry stopping: %+v", state)
	}
	if state.LastCycle.Targets[0].State != collectors.TargetOK {
		t.Fatal("report after cancellation was recorded")
	}
}

func TestStartAfterShutdownIsRefused(t *testing.T) {
	lifecycle := testLifecycle(t, newFakePollers(), defaultRestartBackoff)
	if err := lifecycle.shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Start(context.Background(), collectorConfig("a", "prod")); !errors.Is(err, errLifecycleClosed) {
		t.Fatalf("expected errLifecycleClosed, got %v", err)
	}
}

func TestShutdownStopsAllPollersConcurrently(t *testing.T) {
	pollers := newFakePollers()
	slowStop := func(ctx context.Context, _ func(collectors.CycleResult)) error {
		<-ctx.Done()
		time.Sleep(100 * time.Millisecond)
		return ctx.Err()
	}
	lifecycle := testLifecycle(t, pollers, defaultRestartBackoff)
	for _, id := range []string{"a", "b", "c"} {
		pollers.set(id, slowStop)
		if err := lifecycle.Start(context.Background(), collectorConfig(id, "prod")); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	if err := lifecycle.shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("pollers were stopped one at a time (%s)", elapsed)
	}
	if len(lifecycle.states()) != 0 {
		t.Fatalf("entries left after shutdown: %+v", lifecycle.states())
	}
}
