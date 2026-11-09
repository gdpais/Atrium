package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	atriumconfig "atrium/internal/config"
	"atrium/services/db-collector/internal/collectors"
)

// Collector phases reported by the lifecycle and served on /readyz.
const (
	// phaseRunning means a poller goroutine is active for the collector.
	phaseRunning = "running"
	// phaseRestarting means an Update cancelled the old poller and is waiting
	// for it to exit before starting the replacement.
	phaseRestarting = "restarting"
	// phaseBackingOff means the poller crashed and the supervisor is waiting
	// before restarting it.
	phaseBackingOff = "backing_off"
	// phaseFailed means the poller crashed failedAfterCrashes times in a row
	// without completing a cycle (the supervisor keeps retrying at the maximum
	// backoff), the poller rejected its config (no automatic restart), or an
	// Update gave up waiting and no replacement was started. The next reload
	// restarts failed collectors.
	phaseFailed = "failed"
	// phaseDraining means the collector is about to be removed.
	phaseDraining = "draining"
	// phaseStopping means the poller was cancelled and is finishing its
	// in-flight cycle.
	phaseStopping = "stopping"
	// phaseStopped is reported for IDs the lifecycle does not manage.
	phaseStopped = "stopped"
)

// failedAfterCrashes is the number of consecutive crashes (without a completed
// cycle in between) after which a backing-off collector is reported as failed.
const failedAfterCrashes = 5

// errLifecycleClosed is returned by Start and Update after shutdown began.
var errLifecycleClosed = errors.New("collector lifecycle is shut down")

// restartBackoff is an exponential crash-restart delay policy.
type restartBackoff struct {
	initial time.Duration
	max     time.Duration
}

// defaultRestartBackoff restarts crashed pollers after 1s, doubling to 1m.
var defaultRestartBackoff = restartBackoff{initial: time.Second, max: time.Minute}

// delay returns the wait before restart number crashes (1-based).
func (b restartBackoff) delay(crashes int) time.Duration {
	d := b.initial
	for i := 1; i < crashes && d < b.max; i++ {
		d *= 2
	}
	return min(d, b.max)
}

// pollerRunFunc runs one poller until ctx is cancelled. It is a seam so tests
// can simulate pollers that crash; production uses collectors.Poller.Start.
type pollerRunFunc func(ctx context.Context, collector atriumconfig.CollectorRuntimeConfig, report func(collectors.CycleResult)) error

// pollerEntry is the supervised state of one collector. All fields except
// collector, cancel, and done are guarded by pollerLifecycle.mu.
type pollerEntry struct {
	collector atriumconfig.CollectorRuntimeConfig
	cancel    context.CancelFunc
	// done is closed when the supervisor goroutine exits for good.
	done chan struct{}

	phase string
	// started is when the current poller run began (reset on crash restart).
	started   time.Time
	lastCycle collectors.CycleResult
	hasCycle  bool
	// crashes counts consecutive crashes without a completed cycle.
	crashes     int
	restarts    int
	nextRestart time.Time
}

// collectorState is an immutable copy of a pollerEntry used for readiness.
type collectorState struct {
	Collector   atriumconfig.CollectorRuntimeConfig
	Phase       string
	Started     time.Time
	LastCycle   collectors.CycleResult
	HasCycle    bool
	Crashes     int
	Restarts    int
	NextRestart time.Time
}

// pollerLifecycle supervises one poller goroutine per collector. It implements
// atriumconfig.CollectorLifecycle.
//
// Pollers run under a context derived from the lifecycle's base context, never
// from the context passed to Start/Update, so they outlive the reload request
// that created them. That context is only passed to bound waits for old
// pollers to exit. The base context is cancelled by shutdown.
type pollerLifecycle struct {
	run        pollerRunFunc
	logger     *slog.Logger
	backoff    restartBackoff
	now        func() time.Time
	baseCtx    context.Context
	cancelBase context.CancelFunc

	mu     sync.Mutex
	items  map[string]*pollerEntry
	closed bool
}

// newPollerLifecycle creates a supervisor whose pollers run under a child of
// parent and execute cycles with runner.
func newPollerLifecycle(parent context.Context, runner collectors.Runner, logger *slog.Logger) *pollerLifecycle {
	return newPollerLifecycleWithRun(parent, logger, func(ctx context.Context, collector atriumconfig.CollectorRuntimeConfig, report func(collectors.CycleResult)) error {
		return collectors.Poller{Runner: runner, Collector: collector, Report: report}.Start(ctx)
	})
}

// newPollerLifecycleWithRun is newPollerLifecycle with an explicit poller
// implementation.
func newPollerLifecycleWithRun(parent context.Context, logger *slog.Logger, run pollerRunFunc) *pollerLifecycle {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	baseCtx, cancel := context.WithCancel(parent)
	return &pollerLifecycle{
		run:        run,
		logger:     logger,
		backoff:    defaultRestartBackoff,
		now:        time.Now,
		baseCtx:    baseCtx,
		cancelBase: cancel,
		items:      map[string]*pollerEntry{},
	}
}

// Start launches a poller for collector and returns once its goroutine is
// running; it does not wait for the first cycle. If the collector already
// exists, Start falls back to Update.
func (l *pollerLifecycle) Start(ctx context.Context, collector atriumconfig.CollectorRuntimeConfig) error {
	l.mu.Lock()
	if _, exists := l.items[collector.ID]; !exists {
		defer l.mu.Unlock()
		return l.startLocked(collector)
	}
	l.mu.Unlock()
	return l.Update(ctx, collector)
}

// Update replaces the collector's poller with one running collector. It
// cancels the old poller and waits for it to exit before starting the new one,
// because the old poller clears the collector's exported series on exit and
// would otherwise delete series the new one just wrote. It gives up with an error
// when ctx ends first; in that case the old entry stays registered (phase
// restarting, then failed once it exits) so the next reload restarts it.
func (l *pollerLifecycle) Update(ctx context.Context, collector atriumconfig.CollectorRuntimeConfig) error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return errLifecycleClosed
	}
	old := l.items[collector.ID]
	if old != nil {
		old.phase = phaseRestarting
		old.cancel()
	}
	l.mu.Unlock()

	if old != nil {
		if err := waitStopped(ctx, old); err != nil {
			l.logger.Warn("collector poller did not stop before update deadline", "collector", collector.ID, "error", err)
			return fmt.Errorf("previous poller did not stop: %w", err)
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if current := l.items[collector.ID]; current != nil && current != old {
		return fmt.Errorf("collector %s was replaced concurrently", collector.ID)
	}
	return l.startLocked(collector)
}

// startLocked registers and launches a supervised poller. l.mu must be held.
func (l *pollerLifecycle) startLocked(collector atriumconfig.CollectorRuntimeConfig) error {
	if l.closed {
		return errLifecycleClosed
	}
	ctx, cancel := context.WithCancel(l.baseCtx)
	entry := &pollerEntry{
		collector: collector,
		cancel:    cancel,
		done:      make(chan struct{}),
		phase:     phaseRunning,
		started:   l.now(),
	}
	l.items[collector.ID] = entry
	go l.supervise(ctx, entry)
	l.logger.Info("collector poller started", "collector", collector.ID, "scrape_interval", collector.ScrapeInterval.String(), "targets", len(collector.Targets))
	return nil
}

// supervise runs the poller for entry until ctx is cancelled.
//
// Poller.Start only returns early with an error for configuration it can
// never run (a non-positive scrape interval), so a returned error marks the
// collector failed without restart-looping; the next reload restarts it. A
// panic, or a return without error while ctx is live, is treated as a crash
// and restarted with exponential backoff under the same entry context. The
// old run has fully returned (including its exporter cleanup) before a
// restart begins.
func (l *pollerLifecycle) supervise(ctx context.Context, entry *pollerEntry) {
	defer l.exited(entry)
	for {
		crashed, err := l.runPoller(ctx, entry)
		if ctx.Err() != nil {
			return
		}
		if !crashed {
			l.recordFailure(entry, err)
			<-ctx.Done()
			return
		}
		delay := l.recordCrash(entry, err)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		l.recordRestart(entry)
	}
}

// runPoller runs one poller. crashed is true when it panicked or returned
// without an error while ctx was still live; err describes what happened.
func (l *pollerLifecycle) runPoller(ctx context.Context, entry *pollerEntry) (crashed bool, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			crashed, err = true, fmt.Errorf("poller panicked: %v", recovered)
			l.logger.Error("collector poller panicked", "collector", entry.collector.ID, "panic", fmt.Sprint(recovered), "stack", string(debug.Stack()))
		}
	}()
	err = l.run(ctx, entry.collector, func(result collectors.CycleResult) { l.recordCycle(ctx, entry, result) })
	if err == nil && ctx.Err() == nil {
		return true, errors.New("poller returned without error while still active")
	}
	return false, err
}

// recordFailure marks entry failed after its poller rejected its config.
func (l *pollerLifecycle) recordFailure(entry *pollerEntry, err error) {
	l.mu.Lock()
	if entry.phase == phaseRunning {
		entry.phase = phaseFailed
	}
	phase := entry.phase
	l.mu.Unlock()
	l.logger.Error("collector poller failed; not restarting until the next reload", "collector", entry.collector.ID, "error", err, "phase", phase)
}

// recordCrash marks entry as crashed and returns the restart delay.
func (l *pollerLifecycle) recordCrash(entry *pollerEntry, err error) time.Duration {
	l.mu.Lock()
	entry.crashes++
	delay := l.backoff.delay(entry.crashes)
	entry.nextRestart = l.now().Add(delay)
	if entry.phase == phaseRunning || entry.phase == phaseBackingOff || entry.phase == phaseFailed {
		entry.phase = phaseBackingOff
		if entry.crashes >= failedAfterCrashes {
			entry.phase = phaseFailed
		}
	}
	crashes, phase := entry.crashes, entry.phase
	l.mu.Unlock()
	l.logger.Error("collector poller crashed", "collector", entry.collector.ID, "error", err, "consecutive_crashes", crashes, "phase", phase, "restart_in", delay.String())
	return delay
}

// recordRestart marks entry as running again after a crash backoff.
func (l *pollerLifecycle) recordRestart(entry *pollerEntry) {
	l.mu.Lock()
	entry.restarts++
	entry.started = l.now()
	entry.nextRestart = time.Time{}
	if entry.phase == phaseBackingOff || entry.phase == phaseFailed {
		entry.phase = phaseRunning
	}
	restarts := entry.restarts
	l.mu.Unlock()
	l.logger.Info("collector poller restarted", "collector", entry.collector.ID, "restarts", restarts)
}

// recordCycle stores the latest cycle result for entry. Reports that arrive
// after the poller's ctx was cancelled are discarded: in-flight probes fail
// with context.Canceled during Update and shutdown, and must not look like
// target failures. Cancellation always happens under l.mu, so checking ctx
// under the same lock is race-free. Target errors are logged by the Runner
// and never stored here.
func (l *pollerLifecycle) recordCycle(ctx context.Context, entry *pollerEntry, result collectors.CycleResult) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ctx.Err() != nil {
		return
	}
	entry.lastCycle = result
	entry.hasCycle = true
	entry.crashes = 0
}

// exited finalises an entry after its supervisor goroutine returns.
func (l *pollerLifecycle) exited(entry *pollerEntry) {
	l.mu.Lock()
	phase := entry.phase
	if l.items[entry.collector.ID] == entry {
		switch phase {
		case phaseStopping:
			delete(l.items, entry.collector.ID)
		case phaseRestarting:
			// The Update that cancelled this poller either replaces the entry
			// right after done closes or has given up; in the latter case no
			// poller runs until the next reload restarts it.
			entry.phase = phaseFailed
		}
	}
	l.mu.Unlock()
	close(entry.done)
	l.logger.Info("collector poller stopped", "collector", entry.collector.ID, "phase", phase)
}

// Drain marks a collector as draining before it is stopped.
func (l *pollerLifecycle) Drain(_ context.Context, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if entry, exists := l.items[id]; exists {
		entry.phase = phaseDraining
	}
	return nil
}

// Stop cancels the collector poller and waits for it to exit, returning an
// error when ctx ends first. A poller that outlives ctx stays registered as
// stopping and is removed when it finally exits.
func (l *pollerLifecycle) Stop(ctx context.Context, id string) error {
	l.mu.Lock()
	entry, exists := l.items[id]
	if !exists {
		l.mu.Unlock()
		return nil
	}
	entry.phase = phaseStopping
	entry.cancel()
	l.mu.Unlock()
	if err := waitStopped(ctx, entry); err != nil {
		l.logger.Warn("collector poller did not stop before deadline", "collector", id, "error", err)
		return fmt.Errorf("poller did not stop: %w", err)
	}
	return nil
}

// Status returns the current lifecycle status for one collector. It never
// includes raw error text.
func (l *pollerLifecycle) Status(id string) atriumconfig.CollectorStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	if entry, exists := l.items[id]; exists {
		return atriumconfig.CollectorStatus{ID: id, Phase: entry.phase, Version: id}
	}
	return atriumconfig.CollectorStatus{ID: id, Phase: phaseStopped}
}

// needsRestart reports whether a collector whose config did not change must
// still be restarted by a reload because it is not running normally. Reloads
// are serialised, so any transitional phase seen here is left over from a
// timed-out earlier operation.
func (l *pollerLifecycle) needsRestart(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, exists := l.items[id]
	return exists && entry.phase != phaseRunning
}

// configs returns the configs of every managed collector, i.e. what the
// runtime is actually running, sorted by ID.
func (l *pollerLifecycle) configs() []atriumconfig.CollectorRuntimeConfig {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]atriumconfig.CollectorRuntimeConfig, 0, len(l.items))
	for _, entry := range l.items {
		out = append(out, entry.collector)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// states returns a copy of every managed collector's state, sorted by ID.
func (l *pollerLifecycle) states() []collectorState {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]collectorState, 0, len(l.items))
	for _, entry := range l.items {
		out = append(out, collectorState{
			Collector:   entry.collector,
			Phase:       entry.phase,
			Started:     entry.started,
			LastCycle:   entry.lastCycle,
			HasCycle:    entry.hasCycle,
			Crashes:     entry.crashes,
			Restarts:    entry.restarts,
			NextRestart: entry.nextRestart,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Collector.ID < out[j].Collector.ID })
	return out
}

// shutdown refuses further starts, cancels every poller at once, and waits for
// all of them together until ctx ends. It returns an error naming the pollers
// that were still running at the deadline.
func (l *pollerLifecycle) shutdown(ctx context.Context) error {
	l.mu.Lock()
	l.closed = true
	entries := make([]*pollerEntry, 0, len(l.items))
	for _, entry := range l.items {
		entry.phase = phaseStopping
		entry.cancel()
		entries = append(entries, entry)
	}
	l.mu.Unlock()
	l.cancelBase()

	var stuck []string
	for _, entry := range entries {
		if waitStopped(ctx, entry) != nil {
			stuck = append(stuck, entry.collector.ID)
		}
	}
	if len(stuck) > 0 {
		sort.Strings(stuck)
		return fmt.Errorf("collector pollers still running at shutdown deadline: %s", strings.Join(stuck, ", "))
	}
	return nil
}

// waitStopped blocks until entry's supervisor exits or ctx ends.
func waitStopped(ctx context.Context, entry *pollerEntry) error {
	select {
	case <-entry.done:
		return nil
	default:
	}
	select {
	case <-entry.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
