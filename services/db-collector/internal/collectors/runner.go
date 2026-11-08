// Package collectors provides the runtime execution layer for database probes.
//
// The central types are [Runner] and [Poller]:
//
//   - Runner executes a single scrape cycle for one collector.  Targets run
//     concurrently (bounded by [DefaultMaxConcurrentTargets]); the probes of
//     one target run serially so each database sees at most one collector
//     query at a time.  A probe error only affects its own target: every
//     other probe and target still runs and records samples.  Samples are
//     recorded per probe through a [collectorexport.ScopedRecorder] when the
//     exporter supports it, so series that disappear from a probe result,
//     belong to a failed probe, or belong to a target skipped while it backs
//     off are deleted instead of going stale.  Evidence is forwarded to an
//     [EvidenceSink] as soon as each target finishes.
//
//   - Poller wraps a Runner and runs a cycle on a fixed interval driven by
//     [CollectorRuntimeConfig.ScrapeInterval].  It owns the per-target
//     backoff state, reports every cycle through its Report callback, and
//     keeps running until the context is cancelled, whatever the probes do.
//
// Every probe has a hard deadline: when the executor has not returned
// [DefaultAbandonGrace] after the probe's context is done (its timeout, the
// cycle deadline, or cancellation), the Runner stops waiting, reports the probe
// as timed out, and leaves the call to finish in the background.  A database
// driver that cannot cancel a query on a dead connection therefore never stalls
// the cycle, the other targets, or a Poller stopping.  An abandoned call's late
// result is discarded, and while it is still running no other probe of its
// target starts in that collector, so each collector holds at most one stuck
// call, and one connection, per target name.  The bound is per collector and
// target name, not per database: see [probeCalls].
//
// With [Runner.WithProbeMetrics], every probe execution is also timed and
// every probe failure counted by reason (see [ProbeMetrics]).
//
// A probe that returns an error wrapping [ErrBlocked] did its part but waits
// for its output to be accepted downstream.  That is backpressure, not a
// failure: it is not counted as a probe error, its target does not back off,
// and the target's state is [TargetBlocked] with
// heartbeat_collector_target_up at 1.
//
// Every probe failure, recovered panic, backoff transition, recovery, and
// exporter or sink error is logged synchronously through the Runner's
// [slog.Logger] from the goroutine that observed it, so no error is lost to
// concurrency.
//
// SQLExecutor is the default [ProbeExecutor] implementation: it opens a live
// database connection via a [connector.Manager], looks up the probe in the
// [catalogsqlserver.Catalog], executes the SQL query, and decodes the result
// set into typed [collectorexport.Sample] values.  It reads at most the
// probe's max_rows rows ([ErrTooManyRows]).
package collectors

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"runtime/debug"
	"slices"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	collectorconfig "heartbeat/internal/config"
	connector "heartbeat/services/db-collector/internal/connectors/sqlserver"
	collectorexport "heartbeat/services/db-collector/internal/export"
	collectormetadata "heartbeat/services/db-collector/internal/metadata"
	catalogsqlserver "heartbeat/services/db-collector/internal/probes/sqlserver"
)

// DefaultMaxConcurrentTargets bounds how many targets of one collector are
// scraped at the same time unless [Runner.WithMaxConcurrentTargets] overrides
// it.
const DefaultMaxConcurrentTargets = 8

// DefaultAbandonGrace is how long the Runner keeps waiting for a probe after
// its context is done before abandoning it.  Cancelling a healthy query takes
// one round trip, so the grace only expires when the executor ignores its
// context, typically on a connection whose peer froze or vanished.
const DefaultAbandonGrace = 2 * time.Second

const (
	// maxBackoff caps the delay before a failing target is retried.
	maxBackoff = 5 * time.Minute
	// backoffJitter is the relative jitter added to every backoff (+0..20%).
	backoffJitter = 0.2
	// maxDefaultProbeTimeout caps the default probe timeout.
	maxDefaultProbeTimeout = 10 * time.Second
	// fallbackProbeTimeout is used when no scrape interval is known.
	fallbackProbeTimeout = 5 * time.Second

	// healthScopeProbe and cycleScopeProbe are reserved exporter scope names
	// for the collector self-observability series.
	healthScopeProbe = "__health__"
	cycleScopeProbe  = "__cycle__"
)

// Self-observability metrics recorded through the Runner's exporter.
const (
	// MetricTargetUp is 1 when the target's last cycle succeeded, a blocked
	// ([TargetBlocked]) or degraded ([TargetDegraded]) one included, and 0
	// when it failed or is backing off.  Labels: collector, environment,
	// target.
	MetricTargetUp = "heartbeat_collector_target_up"
	// MetricTargetLastSuccess is the Unix time of the target's last
	// successful cycle, a blocked or degraded one included: the last time
	// MetricTargetUp was 1.  Absent until the first success.  Labels:
	// collector, environment, target.
	MetricTargetLastSuccess = "heartbeat_collector_target_last_success_timestamp_seconds"
	// MetricTargetConsecutiveFailures counts failed cycles since the target's
	// last success.  Labels: collector, environment, target.
	MetricTargetConsecutiveFailures = "heartbeat_collector_target_consecutive_failures"
	// MetricCycleDuration is the wall time of the collector's last cycle.
	// Labels: collector.
	MetricCycleDuration = "heartbeat_collector_cycle_duration_seconds"
	// MetricProbeUp is 1 when the probe's last execution succeeded, waiting
	// downstream included, and 0 when it failed, backs off on its own, or its
	// target failed or backs off.  Labels: collector, environment, target,
	// probe.
	MetricProbeUp = "heartbeat_collector_probe_up"
	// MetricTargetLoginSysadmin is 1 when the collector's login for the target
	// is sysadmin-equivalent (a member of the sysadmin server role, or holding
	// CONTROL SERVER) and 0 when it is not, as of the last check, which runs
	// when the target's connection pool is created and about every 10
	// minutes after.  Absent while unknown, or when the executor is not a
	// [SysadminReporter].  Labels: collector, environment, target.
	MetricTargetLoginSysadmin = "heartbeat_collector_target_login_sysadmin"
)

// fallbackSinkMu serialises evidence publishing, and fallbackProbeCalls
// tracks probe calls, for Runners that were not built with [NewRunner].
var (
	fallbackSinkMu     sync.Mutex
	fallbackProbeCalls = newProbeCalls()
)

// errProbeAbandoned marks a probe the Runner stopped waiting for.
var errProbeAbandoned = errors.New("abandoned while still running")

// errProbeTimedOut is the cause of a probe context ended by the probe's own
// timeout, which tells it apart from the cycle deadline.
var errProbeTimedOut = errors.New("probe timeout")

// errProbeSkipped marks a probe that did not start because its cycle had
// already reached its deadline: the probes before it, or the wait for a
// concurrency slot, used the whole cycle.  It is not a failure of the probe
// or of its target: neither backs off, and the probe keeps exporting the
// series of its last run until it runs again.
var errProbeSkipped = errors.New("skipped: the cycle reached its deadline before the probe started")

// ErrBlocked marks a probe call that ran without error but leaves output
// waiting to be accepted downstream, so the probe's work is paused: an
// outsystems_logs log type whose batch the OTel Collector has not accepted
// yet.  An executor returns an error wrapping it, with the details.  The
// Runner does not treat it as a failure (see the package documentation).
var ErrBlocked = errors.New("waiting for its output to be accepted downstream")

// ErrTooManyRows reports a probe that returned more rows than its max_rows.
// It fails the probe like any query error (reason "error" in
// [MetricProbeErrors]).
var ErrTooManyRows = errors.New("returned more rows than max_rows")

// errExecutorExited reports an executor call that ended without returning,
// through runtime.Goexit.
var errExecutorExited = errors.New("executor exited without returning")

// ProbeExecutor executes a single scheduled probe against a live database and
// returns the decoded metric samples along with any structured evidence.
type ProbeExecutor interface {
	RunProbe(context.Context, collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error)
}

// SysadminReporter is an optional extension of [ProbeExecutor].  When the
// Runner's executor implements it, the Runner exports
// [MetricTargetLoginSysadmin] with each target's health series.
type SysadminReporter interface {
	// TargetSysadmin reports whether the login used for target is
	// sysadmin-equivalent (a member of sysadmin, or holding CONTROL SERVER).
	// ok is false while that is unknown, for example before the first
	// connection to the target.  It must not block.
	TargetSysadmin(target collectormetadata.DatabaseTarget) (sysadmin, ok bool)
}

// CollectorObserver is an optional extension of [ProbeExecutor] for executors
// that keep series or state per collector.  A [Poller] calls InitCollector
// when it starts and ForgetCollector when it stops, after its last cycle, so
// a reload that changes or removes a collector leaves no stale series.  Both
// must not block.
type CollectorObserver interface {
	InitCollector(collectorconfig.CollectorRuntimeConfig)
	ForgetCollector(collectorID string)
}

// EvidenceSink receives structured evidence produced by probes in categories
// such as "blocking" or "sessions" for downstream processing or alerting.
type EvidenceSink interface {
	Publish([]collectormetadata.Evidence) error
}

// Runner orchestrates a single scrape cycle for a collector: it fans out to
// all scheduled targets, records the resulting metric samples, and forwards
// evidence to the configured sink.
//
// Runner is a value type that may be shared by many Pollers; its only
// per-collector state is the set of probe calls in flight, which every copy
// shares so a Poller replaced by a reload sees the calls its predecessor
// abandoned.
type Runner struct {
	executor      ProbeExecutor
	exporter      collectorexport.Recorder
	sink          EvidenceSink
	logger        *slog.Logger
	metrics       *ProbeMetrics
	maxConcurrent int
	// abandonGrace overrides DefaultAbandonGrace when positive (tests).
	abandonGrace time.Duration
	// testHookCallSent, when set, runs in the executor goroutine right after
	// a call's result is sent (tests).
	testHookCallSent func()
	// sinkMu serialises Publish calls because EvidenceSink implementations
	// need not be safe for concurrent use.  It is shared by every copy of
	// the Runner.
	sinkMu *sync.Mutex
	// calls tracks executor calls in flight, abandoned ones included.  It is
	// shared by every copy of the Runner.
	calls *probeCalls
}

// WithLogger returns a copy of r that logs through logger.  A nil logger
// falls back to [slog.Default].
func (r Runner) WithLogger(logger *slog.Logger) Runner {
	r.logger = logger
	return r
}

// WithProbeMetrics returns a copy of r that records probe durations and
// errors in metrics.  A nil metrics records nothing.
func (r Runner) WithProbeMetrics(metrics *ProbeMetrics) Runner {
	r.metrics = metrics
	return r
}

// WithMaxConcurrentTargets returns a copy of r that scrapes at most n targets
// of one collector at the same time.  Values below 1 select
// [DefaultMaxConcurrentTargets].
func (r Runner) WithMaxConcurrentTargets(n int) Runner {
	r.maxConcurrent = n
	return r
}

// NewRunner constructs a Runner wired to the given executor, exporter, and
// evidence sink.  sink may be nil; evidence is silently discarded in that case.
// When exporter implements [collectorexport.ScopedRecorder], stale series are
// deleted per probe scope.
func NewRunner(executor ProbeExecutor, exporter collectorexport.Recorder, sink EvidenceSink) Runner {
	return Runner{executor: executor, exporter: exporter, sink: sink, sinkMu: &sync.Mutex{}, calls: newProbeCalls()}
}

// RunOnce executes one scrape cycle for collector and returns its result.
// Backoff state is not carried between RunOnce calls; use [Poller] for
// repeated scraping.  The returned error joins the errors of every failed
// target and failed probe of a degraded one, and is nil when all targets
// succeeded or collector.Enabled is false.  A failing target never prevents
// the other targets from running.
func (r Runner) RunOnce(ctx context.Context, collector collectorconfig.CollectorRuntimeConfig) (CycleResult, error) {
	result := r.runCycle(ctx, collector, newTargetTracker())
	return result, cycleError(result)
}

// cycleError joins the errors of every failed target in result, and of the
// failed probes of degraded ones, leaving out their [ErrBlocked] errors and
// the targets and probes the cycle deadline skipped.
func cycleError(result CycleResult) error {
	var errs []error
	for _, target := range result.Targets {
		switch {
		case target.State == TargetDegraded:
			for _, probe := range target.Probes {
				if probe.State != ProbeSkipped {
					errs = append(errs, fmt.Errorf("target %s: %w", target.Target, probe.Err))
				}
			}
		case target.Err != nil && target.State != TargetBlocked && target.State != TargetSkipped:
			errs = append(errs, fmt.Errorf("target %s: %w", target.Target, target.Err))
		}
	}
	return errors.Join(errs...)
}

// targetGroup holds the serially executed probes of one target.
type targetGroup struct {
	name        string
	environment string
	items       []collectormetadata.ScheduledProbe
}

// groupByTarget groups scheduled probes by target name, preserving the
// configured target and probe order.
func groupByTarget(items []collectormetadata.ScheduledProbe) []targetGroup {
	var groups []targetGroup
	index := map[string]int{}
	for _, item := range items {
		i, ok := index[item.Target.Name]
		if !ok {
			i = len(groups)
			index[item.Target.Name] = i
			groups = append(groups, targetGroup{name: item.Target.Name, environment: item.Target.EnvironmentSlug})
		}
		groups[i].items = append(groups[i].items, item)
	}
	return groups
}

// targetState is the backoff bookkeeping for one target.
type targetState struct {
	consecutiveFailures int
	lastSuccess         time.Time
	nextAttempt         time.Time
	// lastErr is the error of the last failed cycle, nil after a success.
	lastErr error
}

// probeState is the backoff bookkeeping for one probe that failed while
// other probes of its target succeeded.
type probeState struct {
	consecutiveFailures int
	nextAttempt         time.Time
	lastErr             error
}

// probeKey identifies a probe of a target.
type probeKey struct{ target, probe string }

// targetTracker holds per-target, and per-probe, backoff state for one
// collector across cycles.  It is owned by a single Poller and is safe for
// concurrent use by that Poller's target goroutines.
type targetTracker struct {
	mu      sync.Mutex
	targets map[string]targetState
	probes  map[probeKey]probeState
	now     func() time.Time
	jitter  func() float64 // uniform in [0, 1)
	// lastCycleStart is when the previous cycle started; only the Poller's own
	// goroutine reads and writes it, between cycles.
	lastCycleStart time.Time
	// firstTarget names the target the next cycle dispatches first: the first
	// one the previous cycle skipped.  Only the cycle's own goroutine reads
	// and writes it, outside the target goroutines.
	firstTarget string
	// startAt names, per target, the probe its next cycle runs first: the
	// first probe the previous cycle skipped, so that a cycle too short for
	// all of a target's probes does not starve the same ones every time.
	startAt map[string]string
	// lastUp is, per probe, whether its last run succeeded (or waits
	// downstream): the heartbeat_collector_probe_up a skipped probe keeps.
	lastUp map[probeKey]bool
}

func newTargetTracker() *targetTracker {
	return &targetTracker{targets: map[string]targetState{}, probes: map[probeKey]probeState{}, now: time.Now, jitter: rand.Float64}
}

// probeWaiting returns the state of a probe of target and whether it is
// still backing off at cycleStart.
func (t *targetTracker) probeWaiting(target, probe string, cycleStart time.Time) (probeState, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	state, ok := t.probes[probeKey{target, probe}]
	return state, ok && cycleStart.Before(state.nextAttempt)
}

// probeFail records a failure of a probe of target in the cycle that started
// at cycleStart, with the same backoff schedule as a target's.
func (t *targetTracker) probeFail(target, probe string, cycleStart time.Time, interval time.Duration, err error) (probeState, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := probeKey{target, probe}
	state := t.probes[key]
	state.consecutiveFailures++
	state.lastErr = err
	backoff := backoffFor(interval, state.consecutiveFailures, t.jitter())
	state.nextAttempt = cycleStart.Add(backoff)
	t.probes[key] = state
	return state, backoff
}

// probeSucceed forgets a probe's failures and returns how many there were.
func (t *targetTracker) probeSucceed(target, probe string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := probeKey{target, probe}
	failures := t.probes[key].consecutiveFailures
	delete(t.probes, key)
	return failures
}

// resetProbes forgets the probe failures of target: it failed as a whole,
// so its probes start over once it recovers.
func (t *targetTracker) resetProbes(target string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for key := range t.probes {
		if key.target == target {
			delete(t.probes, key)
		}
	}
}

// firstProbe returns the probe the next cycle of target runs first, or ""
// for the configured order.
func (t *targetTracker) firstProbe(target string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.startAt[target]
}

// setFirstProbe makes the next cycle of target run probe first; "" restores
// the configured order.
func (t *targetTracker) setFirstProbe(target, probe string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if probe == "" {
		delete(t.startAt, target)
		return
	}
	if t.startAt == nil {
		t.startAt = map[string]string{}
	}
	t.startAt[target] = probe
}

// setLastUp records whether the probe of target that just ran succeeded.
func (t *targetTracker) setLastUp(target, probe string, up bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.lastUp == nil {
		t.lastUp = map[probeKey]bool{}
	}
	t.lastUp[probeKey{target, probe}] = up
}

// probeLastUp returns 1 when the last run of the probe of target succeeded,
// and 0 when it failed or the probe never ran.
func (t *targetTracker) probeLastUp(target, probe string) float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.lastUp[probeKey{target, probe}] {
		return 1
	}
	return 0
}

// get returns the current state of target.
func (t *targetTracker) get(target string) targetState {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.targets[target]
}

// succeed resets target after a successful cycle and returns the state
// before and after the reset.
func (t *targetTracker) succeed(target string, at time.Time) (previous, current targetState) {
	t.mu.Lock()
	defer t.mu.Unlock()
	previous = t.targets[target]
	current = targetState{lastSuccess: at}
	t.targets[target] = current
	return previous, current
}

// fail records a cycle that started at cycleStart and failed with err, and
// schedules the next attempt.  It returns the new state and the chosen
// backoff.
func (t *targetTracker) fail(target string, cycleStart time.Time, interval time.Duration, err error) (targetState, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.targets[target]
	state.consecutiveFailures++
	state.lastErr = err
	backoff := backoffFor(interval, state.consecutiveFailures, t.jitter())
	state.nextAttempt = cycleStart.Add(backoff)
	t.targets[target] = state
	return state, backoff
}

// backoffFor returns how long a target waits after failures consecutive
// failed cycles.  The first failure retries on the next cycle so a single
// transient error never costs a scrape.  From the second failure on the delay
// is min(interval * 2^(failures-2), maxBackoff) scaled by a jitter factor in
// [1, 1+backoffJitter); the jitter only adds, so each backoff always skips at
// least the next cycle instead of randomly retrying on it.  jitter is uniform
// in [0, 1).
func backoffFor(interval time.Duration, failures int, jitter float64) time.Duration {
	if failures <= 1 {
		return 0
	}
	delay := interval
	if delay <= 0 {
		delay = fallbackProbeTimeout
	}
	for i := 2; i < failures && delay < maxBackoff; i++ {
		delay *= 2
	}
	delay = min(delay, maxBackoff)
	factor := 1 + backoffJitter*jitter
	return time.Duration(float64(delay) * factor)
}

// runCycle executes one cycle for collector using tracker for backoff state.
// Targets run concurrently, bounded by the Runner's concurrency limit, and the
// whole cycle is bounded by the scrape interval so cycles never overlap.
func (r Runner) runCycle(ctx context.Context, collector collectorconfig.CollectorRuntimeConfig, tracker *targetTracker) CycleResult {
	started := tracker.now()
	result := CycleResult{CollectorID: collector.ID, Started: started}
	if !collector.Enabled {
		result.Finished = started
		return result
	}
	spanCtx, span := tracer.Start(ctx, "collector.cycle", trace.WithAttributes(attribute.String("heartbeat.collector", collector.ID), attribute.String("heartbeat.collector.kind", string(collector.Kind))))
	defer span.End()
	cycleCtx, cancel := cycleContext(spanCtx, collector.ScrapeInterval)
	defer cancel()
	groups := groupByTarget(scheduledProbes(collector))
	result.Targets = make([]TargetResult, len(groups))
	sem := make(chan struct{}, r.concurrency())
	var wg sync.WaitGroup
	// Never leave target goroutines behind, even if this function panics.
	defer wg.Wait()
	// Slots are taken here, in dispatch order, so that order decides which
	// targets a cycle too short for all of them leaves out.  It starts at the
	// first target the previous cycle skipped: under sustained overload the
	// skipped targets take turns instead of the same ones never running.
	order := targetOrder(groups, tracker.firstTarget)
	for _, i := range order {
		group := groups[i]
		state := tracker.get(group.name)
		if started.Before(state.nextAttempt) {
			result.Targets[i] = r.skipTarget(collector.ID, group, state)
			continue
		}
		if !acquire(cycleCtx, sem) {
			// The cycle is over: its probes find the deadline passed and
			// are skipped.
			result.Targets[i] = r.runTarget(cycleCtx, collector, group, tracker, started)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			result.Targets[i] = r.runTarget(cycleCtx, collector, group, tracker, started)
		}()
	}
	wg.Wait()
	tracker.firstTarget = ""
	for _, i := range order {
		if result.Targets[i].State == TargetSkipped {
			tracker.firstTarget = result.Targets[i].Target
			break
		}
	}
	result.Finished = tracker.now()
	r.recordCycle(collector.ID, result.Finished.Sub(started))
	if !errors.Is(ctx.Err(), context.Canceled) {
		r.metrics.observeCycle(collector.ID, result.Finished.Sub(started), collector.ScrapeInterval)
	}
	return result
}

// targetOrder returns the indexes of groups in the order a cycle dispatches
// them: the configured order, rotated to start at the target named first
// when it is one of groups.
func targetOrder(groups []targetGroup, first string) []int {
	start := max(slices.IndexFunc(groups, func(g targetGroup) bool { return g.name == first }), 0)
	order := make([]int, 0, len(groups))
	for i := range groups {
		order = append(order, (start+i)%len(groups))
	}
	return order
}

// cycleContext bounds a cycle by the scrape interval when one is set.
func cycleContext(ctx context.Context, interval time.Duration) (context.Context, context.CancelFunc) {
	if interval > 0 {
		return context.WithTimeout(ctx, interval)
	}
	return context.WithCancel(ctx)
}

// skipTarget reports a target that is still backing off, with the error of
// its last failed cycle.  It runs none of the target's probes and clears
// their series: the probes that succeeded in the failed cycle would otherwise
// keep exporting their last values, unchanged, for the whole backoff.
func (r Runner) skipTarget(collectorID string, group targetGroup, state targetState) TargetResult {
	r.log().Debug("target skipped during backoff",
		"collector", collectorID,
		"target", group.name,
		"consecutive_failures", state.consecutiveFailures,
		"next_attempt", state.nextAttempt)
	for _, item := range group.items {
		r.clearScope(collectorexport.Scope{Collector: collectorID, Target: item.Target.Name, Probe: item.Definition.Name})
	}
	result := TargetResult{
		Target:              group.name,
		State:               TargetBackoff,
		Err:                 state.lastErr,
		ConsecutiveFailures: state.consecutiveFailures,
		LastSuccess:         state.lastSuccess,
		NextAttempt:         state.nextAttempt,
	}
	r.recordHealth(collectorID, group, result, nil)
	return result
}

// probeRun is the outcome of one probe in runTarget.
type probeRun struct {
	item collectormetadata.ScheduledProbe
	// err is the probe's failure; nil when it succeeded or is blocked.
	err error
	// waiting is the probe's state when it backs off on its own and did not
	// run.
	waiting *probeState
	// skipped is set, wrapping [errProbeSkipped], when the probe did not
	// start because the cycle reached its deadline first.
	skipped error
}

// runOrder returns the indexes of items in the order a cycle runs them: the
// configured order, rotated to start at the probe named first when it is
// one of items.
func runOrder(items []collectormetadata.ScheduledProbe, first string) []int {
	start := 0
	for i, item := range items {
		if item.Definition.Name == first {
			start = i
			break
		}
	}
	order := make([]int, 0, len(items))
	for i := range items {
		order = append(order, (start+i)%len(items))
	}
	return order
}

// runTarget runs the probes of one target serially, records their samples
// and evidence, and updates the target's and its probes' backoff state.  A
// probe that failed while others of the target succeeded backs off on its
// own and does not run until its backoff ends, unless every probe of the
// target is backing off, when they all run.  Probes the cycle deadline
// leaves no time for are skipped, not failed, and the next cycle starts with
// the first of them.  It never panics.
func (r Runner) runTarget(ctx context.Context, collector collectorconfig.CollectorRuntimeConfig, group targetGroup, tracker *targetTracker, cycleStart time.Time) (result TargetResult) {
	ctx, span := tracer.Start(ctx, "collector.target", trace.WithAttributes(attribute.String("heartbeat.target", group.name)))
	defer func() {
		span.SetAttributes(attribute.String("heartbeat.target.state", string(result.State)))
		if result.State == TargetFailed {
			span.SetStatus(codes.Error, "target failed")
		}
		span.End()
	}()
	runs := make([]probeRun, len(group.items))
	waitingAll := true
	for i, item := range group.items {
		runs[i].item = item
		if state, ok := tracker.probeWaiting(group.name, item.Definition.Name, cycleStart); ok {
			runs[i].waiting = &state
		} else {
			waitingAll = false
		}
	}
	var blocked []error
	var evidence []collectormetadata.Evidence
	firstSkipped := ""
	for _, i := range runOrder(group.items, tracker.firstProbe(group.name)) {
		item := group.items[i]
		if runs[i].waiting != nil && !waitingAll {
			r.clearScope(collectorexport.Scope{Collector: collector.ID, Target: item.Target.Name, Probe: item.Definition.Name})
			continue
		}
		runs[i].waiting = nil
		items, err := r.runScopedProbe(ctx, collector, item)
		switch {
		case errors.Is(err, errProbeSkipped):
			runs[i].skipped = err
			if firstSkipped == "" {
				firstSkipped = item.Definition.Name
			}
			continue
		case errors.Is(err, ErrBlocked):
			blocked = append(blocked, err)
		case err != nil:
			runs[i].err = err
			tracker.setLastUp(group.name, item.Definition.Name, false)
			continue
		}
		tracker.setLastUp(group.name, item.Definition.Name, true)
		evidence = append(evidence, items...)
	}
	tracker.setFirstProbe(group.name, firstSkipped)
	r.publish(collector.ID, group.name, evidence)
	return r.finishTarget(ctx, collector, group, tracker, cycleStart, runs, errors.Join(blocked...))
}

// acquire takes a concurrency slot, giving up when ctx is done.
func acquire(ctx context.Context, sem chan struct{}) bool {
	if ctx.Err() != nil {
		return false
	}
	select {
	case sem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// runScopedProbe executes one probe and records its samples under the probe's
// exporter scope.  A failed probe's series are cleared; finishTarget logs
// it, once the target's outcome says which failure count applies.  A blocked
// probe ([ErrBlocked]) records its samples like a successful one and returns
// its error, logged at debug level only: the executor logs when the wait
// starts and ends.
func (r Runner) runScopedProbe(ctx context.Context, collector collectorconfig.CollectorRuntimeConfig, item collectormetadata.ScheduledProbe) ([]collectormetadata.Evidence, error) {
	scope := collectorexport.Scope{Collector: collector.ID, Target: item.Target.Name, Probe: item.Definition.Name}
	ctx, span := tracer.Start(ctx, "collector.probe", trace.WithAttributes(attribute.String("heartbeat.probe", item.Definition.Name)))
	defer span.End()
	samples, evidence, err := r.executeProbe(ctx, item, collector.ScrapeInterval)
	if errors.Is(err, errProbeSkipped) {
		// Keep the series of its last run: it did not fail.
		span.SetAttributes(attribute.Bool("heartbeat.probe.skipped", true))
		return nil, err
	}
	if errors.Is(err, ErrBlocked) {
		span.SetAttributes(attribute.Bool("heartbeat.probe.blocked", true))
		r.log().DebugContext(ctx, "probe blocked",
			"collector", scope.Collector,
			"target", scope.Target,
			"probe", scope.Probe,
			"error", err)
		r.recordScope(scope, samples)
		return evidence, err
	}
	if err != nil {
		// Not the error's text, which can name the host and the login. The log
		// line of the same trace has it.
		span.SetStatus(codes.Error, "probe failed")
		r.clearScope(scope)
		return nil, err
	}
	r.recordScope(scope, samples)
	return evidence, nil
}

// executeProbe runs item under its probe timeout.  Probes that cannot start
// before the cycle deadline are reported as skipped ([errProbeSkipped]);
// probes canceled before they start, probes whose target still has an
// abandoned call running, and probes that panic are reported as errors.  A
// probe that does not return within the abandon grace after its context is
// done is abandoned and reported as timed out.  Executions are timed and failures counted
// through the Runner's [ProbeMetrics].
func (r Runner) executeProbe(ctx context.Context, item collectormetadata.ScheduledProbe, interval time.Duration) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
	name := item.Definition.Name
	if err := ctx.Err(); err != nil {
		r.metrics.failedUnlessCanceled(ctx, item, ReasonNotStarted)
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, nil, fmt.Errorf("probe %s %w", name, errProbeSkipped)
		}
		return nil, nil, notStartedError(name, err)
	}
	call, busy := r.probeCalls().start(item, time.Now())
	if busy != nil {
		r.metrics.failedUnlessCanceled(ctx, item, ReasonNotStarted)
		return nil, nil, busyError(name, *busy)
	}
	timeout := probeTimeout(item.Definition.TimeoutMS, interval)
	probeCtx, cancel := context.WithTimeoutCause(ctx, timeout, errProbeTimedOut)
	defer cancel()
	out := r.callExecutor(probeCtx, item, call)
	r.metrics.observe(item, out.elapsed)
	if out.abandoned {
		r.metrics.failedUnlessStopping(out.end, item, ReasonTimeout)
		return nil, nil, abandonedError(out.end, name, timeout, r.grace())
	}
	if out.panicErr != nil {
		r.metrics.failed(item, ReasonPanic)
		return nil, nil, out.panicErr
	}
	if out.err == nil {
		return out.samples, out.evidence, nil
	}
	if errors.Is(out.err, ErrBlocked) {
		// Backpressure, not a failure, even when the executor returned it at
		// its deadline.
		return out.samples, out.evidence, out.err
	}
	r.metrics.failedUnlessStopping(out.end, item, failureReason(out.end, out.err))
	if out.end == probeTimedOut {
		return nil, nil, fmt.Errorf("probe %s timed out after %s: %w", name, timeout, out.err)
	}
	return nil, nil, fmt.Errorf("probe %s: %w", name, out.err)
}

// probeEnd is why a probe's context ended, if it did.
type probeEnd int

const (
	// probeRunning is a probe whose context had not ended.
	probeRunning probeEnd = iota
	// probeTimedOut is a probe that reached its own timeout.
	probeTimedOut
	// probeCycleDeadline is a probe that reached the cycle deadline.
	probeCycleDeadline
	// probeCanceled is a probe whose cycle was canceled because the poller
	// is stopping.
	probeCanceled
)

// probeEndOf reports why probeCtx, created by executeProbe, ended.  A
// context's cause is fixed the moment it is done, so the answer reflects what
// ended the probe first, however much later it is asked: a probe that timed
// out on its own stays timed out when the cycle is canceled during the
// abandon grace.
func probeEndOf(probeCtx context.Context) probeEnd {
	switch {
	case probeCtx.Err() == nil:
		return probeRunning
	case errors.Is(context.Cause(probeCtx), errProbeTimedOut):
		return probeTimedOut
	case errors.Is(probeCtx.Err(), context.DeadlineExceeded):
		return probeCycleDeadline
	default:
		return probeCanceled
	}
}

// probeOutcome is the result of one executor call as seen by executeProbe.
type probeOutcome struct {
	samples  []collectorexport.Sample
	evidence []collectormetadata.Evidence
	err      error
	panicErr error
	// elapsed is the call's duration, or the time until it was abandoned.
	elapsed time.Duration
	// end is why the probe's context ended, taken when the executor
	// returned or, for an abandoned call, when the Runner stopped waiting.
	end probeEnd
	// abandoned reports that the call outlived its hard deadline; the other
	// fields except elapsed are then unset.
	abandoned bool
}

// callExecutor runs the executor for item in its own goroutine and waits for
// it until the abandon grace after probeCtx is done.  call must have been
// registered for item; it is finished when the executor returns, whether or
// not the Runner still waits.  The result of an abandoned call is discarded:
// it is never recorded, published, or counted.
func (r Runner) callExecutor(probeCtx context.Context, item collectormetadata.ScheduledProbe, call *probeCall) probeOutcome {
	calls := r.probeCalls()
	attrs := []any{"collector", item.CollectorID, "target", item.Target.Name, "probe", item.Definition.Name}
	// Buffered so an abandoned call never blocks on the send.
	results := make(chan probeOutcome, 1)
	go func() {
		var out probeOutcome
		returned := false
		// Deferred so the call is unregistered and the waiter answered even
		// when the executor ends its goroutine with runtime.Goexit.
		defer func() {
			if !returned {
				out = probeOutcome{err: errExecutorExited, end: probeEndOf(probeCtx)}
			}
			out.elapsed = time.Since(call.started)
			// Unregister before sending: a waiter that receives the result
			// goes on to start the target's next probe, which must not find
			// this call still in flight.  A call abandon finds finished sends
			// right after.
			abandoned := calls.finish(item, call)
			results <- out
			if r.testHookCallSent != nil {
				r.testHookCallSent()
			}
			if abandoned {
				args := append(attrs, "running", out.elapsed.Round(time.Millisecond).String())
				if out.err != nil {
					args = append(args, "error", out.err)
				}
				r.log().Info("abandoned probe returned; result discarded", args...)
			}
		}()
		out.panicErr = r.protect("probe "+item.Definition.Name, attrs, func() {
			out.samples, out.evidence, out.err = r.executor.RunProbe(probeCtx, item)
		})
		out.end = probeEndOf(probeCtx)
		returned = true
	}()
	select {
	case out := <-results:
		return out
	case <-probeCtx.Done():
	}
	end := probeEndOf(probeCtx)
	grace := time.NewTimer(r.grace())
	defer grace.Stop()
	select {
	case out := <-results:
		return out
	case <-grace.C:
	}
	if !calls.abandon(call) {
		// The call finished while the grace expired; its result is being
		// sent.
		return <-results
	}
	return probeOutcome{abandoned: true, end: end, elapsed: time.Since(call.started)}
}

// busyError explains why a probe was not started while another call for its
// target is in flight.  Probes of one target run serially, so busy is almost
// always an abandoned call; one merely in flight means two cycles of the same
// collector overlapped, as with concurrent RunOnce calls.
func busyError(probe string, busy probeCall) error {
	running := time.Since(busy.started).Round(time.Millisecond)
	if busy.abandoned {
		return fmt.Errorf("probe %s not started: probe %s of the target was abandoned and is still running after %s", probe, busy.probe, running)
	}
	return fmt.Errorf("probe %s not started: probe %s of the target is still in flight after %s", probe, busy.probe, running)
}

// abandonedError explains why a probe was abandoned.  end is why its
// context ended.
func abandonedError(end probeEnd, probe string, timeout, grace time.Duration) error {
	cause := "timed out after " + timeout.String()
	switch end {
	case probeCycleDeadline:
		cause = "reached the cycle deadline"
	case probeCanceled:
		cause = "was canceled"
	}
	return fmt.Errorf("probe %s %s and did not stop within %s: %w", probe, cause, grace, errProbeAbandoned)
}

// notStartedError explains why a probe was never started.
func notStartedError(probe string, cause error) error {
	if errors.Is(cause, context.DeadlineExceeded) {
		return fmt.Errorf("probe %s not started before cycle deadline: %w", probe, cause)
	}
	return fmt.Errorf("probe %s not started: %w", probe, cause)
}

// probeCalls tracks the executor calls in flight, at most one per target of
// a collector, including calls the Runner abandoned.  Probes of one target run
// serially, so a second call for a target can only be requested while an
// abandoned one is still running; start refuses it.  This bounds each
// collector and target name to one stuck goroutine and connection however
// long the outage lasts.  It does not bound a database: the same database
// reached under another target name or by another collector, or a target
// renamed by a reload, can hold one more stuck call each, and stuck calls
// occupy connections of the connector's shared pool, whose size is small.
// probeCalls is safe for concurrent use.
type probeCalls struct {
	mu    sync.Mutex
	calls map[probeCallKey]*probeCall
}

// probeCallKey identifies a target of a collector.
type probeCallKey struct {
	collector string
	target    string
}

// probeCall is one executor call.  probe and started never change.
type probeCall struct {
	probe   string
	started time.Time
	// abandoned and finished are guarded by probeCalls.mu.
	abandoned bool
	finished  bool
}

func newProbeCalls() *probeCalls {
	return &probeCalls{calls: map[probeCallKey]*probeCall{}}
}

func callKey(item collectormetadata.ScheduledProbe) probeCallKey {
	return probeCallKey{collector: item.CollectorID, target: item.Target.Name}
}

// start registers a call of item's probe starting at now.  When a call for
// item's target is still in flight it registers nothing and returns a copy of
// that call, taken under the lock, as busy instead.
func (c *probeCalls) start(item collectormetadata.ScheduledProbe, now time.Time) (call, busy *probeCall) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := callKey(item)
	if existing := c.calls[key]; existing != nil {
		snapshot := *existing
		return nil, &snapshot
	}
	call = &probeCall{probe: item.Definition.Name, started: now}
	c.calls[key] = call
	return call, nil
}

// abandon marks call as abandoned and reports whether it was still running.
func (c *probeCalls) abandon(call *probeCall) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if call.finished {
		return false
	}
	call.abandoned = true
	return true
}

// finish unregisters call once its executor returned and reports whether it
// had been abandoned.
func (c *probeCalls) finish(item collectormetadata.ScheduledProbe, call *probeCall) (abandoned bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	call.finished = true
	if key := callKey(item); c.calls[key] == call {
		delete(c.calls, key)
	}
	return call.abandoned
}

// finishTarget updates the backoff state of a target and its probes after
// they ran, logs each probe failure and the target's recovery or backoff
// entry, and records the health series.  runs holds each probe's outcome;
// blocked joins the [ErrBlocked] errors, which count as success but make the
// target [TargetBlocked].  The target fails only when every probe that ran
// failed; otherwise its failing probes back off on their own and it is
// [TargetDegraded], a success of the target.
func (r Runner) finishTarget(ctx context.Context, collector collectorconfig.CollectorRuntimeConfig, group targetGroup, tracker *targetTracker, cycleStart time.Time, runs []probeRun, blocked error) TargetResult {
	var errs, skipped []error
	ran, failed := 0, 0
	for _, run := range runs {
		if run.skipped != nil {
			skipped = append(skipped, run.skipped)
			continue
		}
		if run.waiting == nil {
			ran++
		}
		if run.err != nil {
			failed++
			errs = append(errs, run.err)
		}
	}
	// No probe ran: the cycle reached its deadline before the target's turn,
	// or before its first probe.  That says nothing about the target, so its
	// state and series stay as they are.
	if ran == 0 && len(skipped) > 0 {
		return r.skippedTarget(ctx, collector.ID, group, tracker.get(group.name), errors.Join(skipped...))
	}
	// Probes backing off on their own or skipped did not run, so they do not
	// count: when a single probe ran, its one failure fails the target and
	// forgets the others' backoffs, and they all run on the target's next
	// attempt.
	if failed > 0 && failed == ran {
		tracker.resetProbes(group.name)
		return r.failTarget(ctx, collector, group, tracker, cycleStart, runs, errors.Join(errs...))
	}
	var probes []ProbeResult
	for _, run := range runs {
		name := run.item.Definition.Name
		switch {
		case run.skipped != nil:
			probes = append(probes, ProbeResult{Probe: name, State: ProbeSkipped, Err: run.skipped})
			errs = append(errs, run.skipped)
		case run.waiting != nil:
			probes = append(probes, ProbeResult{Probe: name, State: ProbeBackoff, Err: run.waiting.lastErr,
				ConsecutiveFailures: run.waiting.consecutiveFailures, NextAttempt: run.waiting.nextAttempt})
			errs = append(errs, run.waiting.lastErr)
		case run.err != nil:
			state, backoff := tracker.probeFail(group.name, name, cycleStart, collector.ScrapeInterval, run.err)
			r.logProbeFailure(ctx, collector.ID, run, state.consecutiveFailures,
				"target_state", TargetDegraded,
				"backoff", backoff.String(),
				"next_attempt", state.nextAttempt)
			probes = append(probes, ProbeResult{Probe: name, State: ProbeFailed, Err: run.err,
				ConsecutiveFailures: state.consecutiveFailures, NextAttempt: state.nextAttempt})
		default:
			if previous := tracker.probeSucceed(group.name, name); previous > 0 {
				r.log().Info("probe recovered",
					"collector", collector.ID,
					"target", group.name,
					"probe", name,
					"previous_failures", previous)
			}
		}
	}
	previous, current := tracker.succeed(group.name, tracker.now())
	if previous.consecutiveFailures > 0 {
		r.log().Info("target recovered",
			"collector", collector.ID,
			"target", group.name,
			"previous_failures", previous.consecutiveFailures,
			"failing_probes", len(probes))
	}
	result := TargetResult{Target: group.name, State: TargetOK, LastSuccess: current.lastSuccess}
	switch {
	case len(probes) > 0:
		result.State, result.Err, result.Probes = TargetDegraded, errors.Join(append(errs, blocked)...), probes
	case blocked != nil:
		result.State, result.Err = TargetBlocked, blocked
	}
	r.recordHealth(collector.ID, group, result, tracker)
	return result
}

// skippedTarget reports a target none of whose probes ran because the cycle
// reached its deadline first.  It changes neither the target's backoff state
// nor its series, health ones included: the last cycle that ran it still
// describes it, and its last success does not advance.
func (r Runner) skippedTarget(ctx context.Context, collectorID string, group targetGroup, state targetState, err error) TargetResult {
	r.log().WarnContext(ctx, "target skipped: the cycle reached its deadline before its probes started",
		"collector", collectorID,
		"target", group.name)
	return TargetResult{
		Target:              group.name,
		State:               TargetSkipped,
		Err:                 err,
		ConsecutiveFailures: state.consecutiveFailures,
		LastSuccess:         state.lastSuccess,
	}
}

// logProbeFailure logs a probe that failed in this cycle with the failure
// count that applies to it: its target's when the target failed, its own
// when it backs off on its own.  attrs say which.
func (r Runner) logProbeFailure(ctx context.Context, collectorID string, run probeRun, failures int, attrs ...any) {
	r.log().WarnContext(ctx, "probe failed", append([]any{
		"collector", collectorID,
		"target", run.item.Target.Name,
		"probe", run.item.Definition.Name,
		"error", run.err,
		"consecutive_failures", failures,
	}, attrs...)...)
}

// failTarget records a cycle in which every probe of the target that ran
// failed: it logs each failure, and the target backs off as a whole.
func (r Runner) failTarget(ctx context.Context, collector collectorconfig.CollectorRuntimeConfig, group targetGroup, tracker *targetTracker, cycleStart time.Time, runs []probeRun, err error) TargetResult {
	state, backoff := tracker.fail(group.name, cycleStart, collector.ScrapeInterval, err)
	for _, run := range runs {
		if run.err != nil {
			r.logProbeFailure(ctx, collector.ID, run, state.consecutiveFailures, "target_state", TargetFailed)
		}
	}
	r.log().WarnContext(ctx, "target failed",
		"collector", collector.ID,
		"target", group.name,
		"consecutive_failures", state.consecutiveFailures,
		"backoff", backoff.String(),
		"next_attempt", state.nextAttempt)
	result := TargetResult{
		Target:              group.name,
		State:               TargetFailed,
		Err:                 err,
		ConsecutiveFailures: state.consecutiveFailures,
		LastSuccess:         state.lastSuccess,
		NextAttempt:         state.nextAttempt,
	}
	r.recordHealth(collector.ID, group, result, tracker)
	return result
}

// recordHealth exports the self-observability series of one target.
//
// tracker, when set, gives the probe_up a skipped probe keeps: that of its
// last run.
func (r Runner) recordHealth(collectorID string, group targetGroup, result TargetResult, tracker *targetTracker) {
	labels := func() map[string]string {
		return map[string]string{"collector": collectorID, "environment": group.environment, "target": group.name}
	}
	up := 0.0
	if result.State == TargetOK || result.State == TargetBlocked || result.State == TargetDegraded {
		up = 1
	}
	samples := []collectorexport.Sample{
		{Metric: MetricTargetUp, Help: "Whether the collector's last cycle for the target succeeded, waiting for downstream or with some probes failing included (1), or failed or is backing off (0).", Value: up, Labels: labels()},
		{Metric: MetricTargetConsecutiveFailures, Help: "Failed collector cycles for the target since its last success.", Value: float64(result.ConsecutiveFailures), Labels: labels()},
	}
	failing, skipped := map[string]bool{}, map[string]bool{}
	for _, probe := range result.Probes {
		if probe.State == ProbeSkipped {
			skipped[probe.Probe] = true
		} else {
			failing[probe.Probe] = true
		}
	}
	for _, item := range group.items {
		probeUp := up
		// A skipped probe did not run: its probe_up keeps the value of its
		// last run, as its other series do.
		if skipped[item.Definition.Name] && tracker != nil {
			probeUp = tracker.probeLastUp(group.name, item.Definition.Name)
		}
		if failing[item.Definition.Name] {
			probeUp = 0
		}
		probeLabels := labels()
		probeLabels["probe"] = item.Definition.Name
		samples = append(samples, collectorexport.Sample{
			Metric: MetricProbeUp,
			Help:   "Whether the probe's last execution succeeded, waiting for downstream included (1), or it failed, backs off on its own, or its target failed or backs off (0).",
			Value:  probeUp,
			Labels: probeLabels,
		})
	}
	if !result.LastSuccess.IsZero() {
		samples = append(samples, collectorexport.Sample{
			Metric: MetricTargetLastSuccess,
			Help:   "Unix time of the target's last successful collector cycle, waiting for downstream or with some probes failing included.",
			Value:  float64(result.LastSuccess.UnixNano()) / float64(time.Second),
			Labels: labels(),
		})
	}
	if sysadmin, ok := r.targetSysadmin(group); ok {
		samples = append(samples, collectorexport.Sample{
			Metric: MetricTargetLoginSysadmin,
			Help:   "Whether the collector's login for the target is sysadmin-equivalent, a member of sysadmin or holding CONTROL SERVER (1), or not (0); checked when the target's connection pool is created and about every 10 minutes after.",
			Value:  sysadmin,
			Labels: labels(),
		})
	}
	r.recordScope(collectorexport.Scope{Collector: collectorID, Target: group.name, Probe: healthScopeProbe}, samples)
}

// targetSysadmin returns 1 or 0 for whether the login of group's target is
// sysadmin-equivalent, and false when the executor cannot tell.
func (r Runner) targetSysadmin(group targetGroup) (float64, bool) {
	reporter, ok := r.executor.(SysadminReporter)
	if !ok || len(group.items) == 0 {
		return 0, false
	}
	sysadmin, ok := reporter.TargetSysadmin(group.items[0].Target)
	if !ok {
		return 0, false
	}
	if sysadmin {
		return 1, true
	}
	return 0, true
}

// recordCycle exports the duration of one collector cycle.
func (r Runner) recordCycle(collectorID string, duration time.Duration) {
	r.recordScope(collectorexport.Scope{Collector: collectorID, Probe: cycleScopeProbe}, []collectorexport.Sample{{
		Metric: MetricCycleDuration,
		Help:   "Wall time of the collector's last scrape cycle in seconds.",
		Value:  duration.Seconds(),
		Labels: map[string]string{"collector": collectorID},
	}})
}

// recordScope writes samples for scope, replacing the scope's previous series
// when the exporter supports scopes.  Errors are logged, never dropped.
func (r Runner) recordScope(scope collectorexport.Scope, samples []collectorexport.Sample) {
	if r.exporter == nil {
		return
	}
	var err error
	attrs := scopeAttrs(scope)
	panicErr := r.protect("record samples", attrs, func() {
		if scoped, ok := r.exporter.(collectorexport.ScopedRecorder); ok {
			err = scoped.RecordScope(scope, samples)
			return
		}
		err = r.exporter.Record(samples)
	})
	if err != nil && panicErr == nil {
		r.log().Error("record samples failed", append(attrs, "error", err)...)
	}
}

// clearScope deletes the series of scope when the exporter supports scopes.
func (r Runner) clearScope(scope collectorexport.Scope) {
	scoped, ok := r.exporter.(collectorexport.ScopedRecorder)
	if !ok {
		return
	}
	_ = r.protect("clear samples", scopeAttrs(scope), func() { scoped.ClearScope(scope) })
}

// initCollector creates the probe error counters of collector and tells an
// executor that implements [CollectorObserver].
func (r Runner) initCollector(collector collectorconfig.CollectorRuntimeConfig) {
	attrs := []any{"collector", collector.ID}
	_ = r.protect("init probe metrics", attrs, func() { r.metrics.initCollector(collector) })
	if observer, ok := r.executor.(CollectorObserver); ok {
		_ = r.protect("init executor", attrs, func() { observer.InitCollector(collector) })
	}
}

// forgetCollector deletes every probe metric series of collectorID, and
// every exported series of it when the exporter supports scopes, and tells an
// executor that implements [CollectorObserver].
func (r Runner) forgetCollector(collectorID string) {
	attrs := []any{"collector", collectorID}
	_ = r.protect("forget probe metrics", attrs, func() { r.metrics.forgetCollector(collectorID) })
	if observer, ok := r.executor.(CollectorObserver); ok {
		_ = r.protect("forget executor", attrs, func() { observer.ForgetCollector(collectorID) })
	}
	scoped, ok := r.exporter.(collectorexport.ScopedRecorder)
	if !ok {
		return
	}
	_ = r.protect("forget collector", attrs, func() { scoped.ForgetCollector(collectorID) })
}

// publish forwards evidence of one target to the sink, serialising calls.
// Errors are logged, never dropped.
func (r Runner) publish(collectorID, target string, evidence []collectormetadata.Evidence) {
	if r.sink == nil || len(evidence) == 0 {
		return
	}
	mu := r.sinkMu
	if mu == nil {
		mu = &fallbackSinkMu
	}
	var err error
	attrs := []any{"collector", collectorID, "target", target}
	panicErr := r.protect("publish evidence", attrs, func() {
		mu.Lock()
		defer mu.Unlock()
		err = r.sink.Publish(evidence)
	})
	if err != nil && panicErr == nil {
		r.log().Error("publish evidence failed", append(attrs, "error", err)...)
	}
}

// protect runs fn and converts a panic into an error.  The panic is logged
// with its stack together with attrs.
func (r Runner) protect(operation string, attrs []any, fn func()) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%s panicked: %v", operation, recovered)
			args := append([]any{"operation", operation, "panic", fmt.Sprint(recovered), "stack", string(debug.Stack())}, attrs...)
			r.log().Error("recovered panic", args...)
		}
	}()
	fn()
	return nil
}

// scopeAttrs returns the log attributes identifying scope.
func scopeAttrs(scope collectorexport.Scope) []any {
	return []any{"collector", scope.Collector, "target", scope.Target, "probe", scope.Probe}
}

// log returns the configured logger or [slog.Default].
func (r Runner) log() *slog.Logger {
	if r.logger == nil {
		return slog.Default()
	}
	return r.logger
}

// concurrency returns the effective per-collector target concurrency.
func (r Runner) concurrency() int {
	if r.maxConcurrent < 1 {
		return DefaultMaxConcurrentTargets
	}
	return r.maxConcurrent
}

// grace returns the effective abandon grace.
func (r Runner) grace() time.Duration {
	if r.abandonGrace > 0 {
		return r.abandonGrace
	}
	return DefaultAbandonGrace
}

// probeCalls returns the shared probe call registry.
func (r Runner) probeCalls() *probeCalls {
	if r.calls == nil {
		return fallbackProbeCalls
	}
	return r.calls
}

// LoggingEvidenceSink is a no-op [EvidenceSink] used in the default wiring.
type LoggingEvidenceSink struct{}

// Publish implements [EvidenceSink].  It is a no-op.
func (LoggingEvidenceSink) Publish([]collectormetadata.Evidence) error { return nil }

// SQLExecutor is a [ProbeExecutor] that opens a SQL Server connection via its
// Manager, resolves the probe definition from its Catalog, and executes the
// probe query.
type SQLExecutor struct {
	Manager connector.Manager
	Catalog catalogsqlserver.Catalog
}

// NewSQLExecutor returns a SQLExecutor backed by the given connection manager
// and the default SQL Server probe catalog.
func NewSQLExecutor(manager connector.Manager) SQLExecutor {
	return SQLExecutor{Manager: manager, Catalog: catalogsqlserver.DefaultCatalog()}
}

// RunProbe implements [ProbeExecutor].  It opens a connection to the target
// database, looks up the probe in the catalog, executes the SQL query within
// a per-probe deadline derived from [timeoutFor], and decodes the result rows
// into metric samples and optional evidence records.
func (e SQLExecutor) RunProbe(ctx context.Context, item collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
	db, cleanup, err := e.Manager.Open(ctx, item.Target)
	if err != nil {
		return nil, nil, err
	}
	defer cleanup()
	probe, ok := e.Catalog.Get(item.Definition.Name)
	if !ok {
		return nil, nil, fmt.Errorf("unknown probe %s", item.Definition.Name)
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeoutFor(item))
	defer cancel()
	probeCtx, span := tracer.Start(probeCtx, "db.query", trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(
		attribute.String("db.system.name", "microsoft.sql_server"), attribute.String("db.query.summary", item.Definition.Name)))
	defer span.End()
	rows, err := db.QueryContext(probeCtx, probeQuery(item, probe))
	if err != nil {
		span.SetStatus(codes.Error, "query failed")
		return nil, nil, fmt.Errorf("query probe: %w", err)
	}
	// Closing rows early cancels the query: the driver tells the server to
	// stop sending the rows readRows left unread.
	defer rows.Close()
	maps, err := readRows(rows, maxRowsFor(item))
	if err != nil {
		span.SetStatus(codes.Error, "reading rows failed")
		return nil, nil, err
	}
	span.SetAttributes(attribute.Int("db.response.returned_rows", len(maps)))
	return decodeRows(item, probe, maps), buildEvidence(item, probe, maps), nil
}

// TargetSysadmin implements [SysadminReporter] from the Manager's last login
// check for target's connection pool.
func (e SQLExecutor) TargetSysadmin(target collectormetadata.DatabaseTarget) (sysadmin, ok bool) {
	return e.Manager.Sysadmin(target)
}

// probeQuery returns the batch to run for item: its query_template override,
// or the catalog query, prefixed with the collector session settings so the
// probe never waits long on a lock or wins a deadlock.
func probeQuery(item collectormetadata.ScheduledProbe, probe catalogsqlserver.Probe) string {
	query := item.Definition.QueryTemplate
	if query == "" {
		query = probe.QueryTemplate
	}
	return connector.WithSessionSettings(query)
}

// timeoutFor returns the effective timeout for one probe execution based on
// the probe's own override and its assignment interval.  See [probeTimeout].
func timeoutFor(item collectormetadata.ScheduledProbe) time.Duration {
	return probeTimeout(item.Definition.TimeoutMS, time.Duration(item.Assignment.IntervalSeconds)*time.Second)
}

// probeTimeout returns the effective timeout for one probe execution.
//
// A probe-specific timeout wins but is capped at the scrape interval, so a
// single probe can never outlive its cycle, and at
// [collectorconfig.MaxProbeTimeout], which connectors' socket deadlines are
// sized to outlast; probe definitions loaded from the metadata store are not
// validated against it.  Without an override the default is half the
// interval, capped at maxDefaultProbeTimeout, leaving room for the target's
// other probes.  fallbackProbeTimeout applies when no interval is known.
func probeTimeout(timeoutMS int, interval time.Duration) time.Duration {
	if timeoutMS > 0 {
		timeout := min(time.Duration(timeoutMS)*time.Millisecond, collectorconfig.MaxProbeTimeout)
		if interval > 0 && timeout > interval {
			return interval
		}
		return timeout
	}
	if interval <= 0 {
		return fallbackProbeTimeout
	}
	return min(interval/2, maxDefaultProbeTimeout)
}

// maxRowsFor returns the most rows item may return: its max_rows, or
// [collectorconfig.DefaultProbeMaxRows], capped at
// [collectorconfig.MaxProbeMaxRows] because probe definitions loaded from the
// metadata store are not validated against it.
func maxRowsFor(item collectormetadata.ScheduledProbe) int {
	if item.Definition.MaxRows <= 0 {
		return collectorconfig.DefaultProbeMaxRows
	}
	return min(item.Definition.MaxRows, collectorconfig.MaxProbeMaxRows)
}

// resultRows is the part of [sql.Rows] that readRows uses.
type resultRows interface {
	Columns() ([]string, error)
	Next() bool
	Scan(dest ...any) error
	Err() error
}

var _ resultRows = (*sql.Rows)(nil)

// readRows materialises the query result set into a slice of column maps.
// It reads at most maxRows rows: when a row follows them, it stops without
// scanning that row and returns [ErrTooManyRows], so a probe whose query
// returns far more rows than expected cannot exhaust the collector's memory
// or the exported series.
//
// Values are kept as driver-returned types except for []byte, which is
// normalised to string so downstream decoding can treat text consistently.
func readRows(rows resultRows, maxRows int) ([]map[string]any, error) {
	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("read columns: %w", err)
	}
	var out []map[string]any
	for rows.Next() {
		if len(out) == maxRows {
			return nil, fmt.Errorf("%w (%d): narrow the query, or raise the probe's max_rows (at most %d)", ErrTooManyRows, maxRows, collectorconfig.MaxProbeMaxRows)
		}
		values := make([]any, len(columns))
		scans := make([]any, len(columns))
		for i := range values {
			scans[i] = &values[i]
		}
		if err := rows.Scan(scans...); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		item := map[string]any{}
		for i, col := range columns {
			item[col] = normalizeValue(values[i])
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rows: %w", err)
	}
	return out, nil
}

// normalizeValue converts driver-returned byte slices into strings.
func normalizeValue(value any) any {
	switch v := value.(type) {
	case []byte:
		return string(v)
	default:
		return v
	}
}

// decodeRows maps the SQL result set into Prometheus samples.
//
// The probe catalog defines which column carries the numeric value, its
// metric type and unit scale, and which columns become labels. Rows that do
// not contain the configured value column, or hold NULL in it, are ignored.
func decodeRows(item collectormetadata.ScheduledProbe, probe catalogsqlserver.Probe, rows []map[string]any) []collectorexport.Sample {
	var samples []collectorexport.Sample
	for _, row := range rows {
		for _, metric := range probe.Metrics {
			rawValue, ok := row[metric.ValueColumn]
			if !ok {
				continue
			}
			metricValue, ok := toFloat64(rawValue)
			if !ok {
				continue
			}
			labels := map[string]string{
				"environment": item.Target.EnvironmentSlug,
				"target":      item.Target.Name,
			}
			for _, labelColumn := range metric.LabelColumns {
				if value, ok := row[labelColumn]; ok {
					labels[labelColumn] = fmt.Sprint(value)
				}
			}
			samples = append(samples, collectorexport.Sample{
				Metric: metric.Name,
				Help:   metric.Help,
				Value:  metric.Convert(metricValue),
				Type:   metric.Type,
				Labels: labels,
			})
		}
	}
	return samples
}

// buildEvidence emits a lightweight snapshot for probe categories that feed
// investigation or alerting workflows.
//
// Evidence is currently produced only for blocking and session probes and only
// when at least one row is returned.
func buildEvidence(item collectormetadata.ScheduledProbe, probe catalogsqlserver.Probe, rows []map[string]any) []collectormetadata.Evidence {
	if probe.Category != "blocking" && probe.Category != "sessions" {
		return nil
	}
	if len(rows) == 0 {
		return nil
	}
	return []collectormetadata.Evidence{{
		Kind:  probe.Category,
		Title: fmt.Sprintf("%s snapshot for %s", probe.Category, item.Target.Name),
		Metadata: map[string]string{
			"rows": strconv.Itoa(len(rows)),
		},
	}}
}

// toFloat64 normalises driver values into a float64 when possible.
func toFloat64(value any) (float64, bool) {
	switch v := value.(type) {
	case int64:
		return float64(v), true
	case int32:
		return float64(v), true
	case int:
		return float64(v), true
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case []byte:
		parsed, err := strconv.ParseFloat(string(v), 64)
		return parsed, err == nil
	case string:
		parsed, err := strconv.ParseFloat(v, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

// Poller repeatedly runs scrape cycles for a single collector on the interval
// defined by Collector.ScrapeInterval.  Per-target backoff state lives for the
// duration of one Start call.
type Poller struct {
	Runner    Runner
	Collector collectorconfig.CollectorRuntimeConfig
	// Report, when set, receives the result of every completed cycle.
	Report func(CycleResult)
}

// scheduledProbes expands a collector into the per-target, per-probe work
// items that Runner executes.
func scheduledProbes(collector collectorconfig.CollectorRuntimeConfig) []collectormetadata.ScheduledProbe {
	var items []collectormetadata.ScheduledProbe
	for _, target := range collector.Targets {
		if len(collector.TargetNames) > 0 && !contains(collector.TargetNames, target.Name) {
			continue
		}
		for _, probe := range target.Probes {
			items = append(items, collectormetadata.ScheduledProbe{
				CollectorID: collector.ID,
				Target: collectormetadata.DatabaseTarget{
					EnvironmentSlug: target.EnvironmentSlug,
					Name:            target.Name,
					Engine:          target.Engine,
					Host:            target.Host,
					Port:            target.Port,
					DatabaseName:    target.DatabaseName,
					CredentialRef:   target.CredentialRef,
				},
				Definition: collectormetadata.ProbeDefinition{
					Name:          probe.Name,
					QueryTemplate: probe.QueryTemplate,
					TimeoutMS:     probe.TimeoutMS,
					MaxRows:       probe.MaxRows,
					Lag:           probe.Lag,
				},
				Assignment: collectormetadata.ProbeAssignment{
					IntervalSeconds: int(collector.ScrapeInterval / time.Second),
				},
				OutSystemsLogs: collector.OutSystemsLogs,
			})
		}
	}
	return items
}

// contains reports whether needle appears in values.
func contains(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

// Start runs an initial scrape immediately and then ticks at
// Collector.ScrapeInterval until ctx is cancelled.  Probe errors and panics
// never stop the poller: they are logged, reported, and handled by per-target
// backoff.  Start creates the collector's probe error counters at 0, returns
// ctx.Err() once ctx is cancelled, after deleting every series the collector
// exported, and an error only if the scrape interval is not positive.  It
// tells an executor that implements [CollectorObserver] when it starts and
// stops.
func (p Poller) Start(ctx context.Context) error {
	interval := p.Collector.ScrapeInterval
	if interval <= 0 {
		return fmt.Errorf("collector %s: scrape interval must be positive, got %s", p.Collector.ID, interval)
	}
	tracker := newTargetTracker()
	defer p.Runner.forgetCollector(p.Collector.ID)
	p.Runner.initCollector(p.Collector)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		p.cycle(ctx, tracker)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// cycle runs one scrape cycle and reports it.  A panic that escapes the
// cycle is logged and reported as a failure of every scheduled target.
func (p Poller) cycle(ctx context.Context, tracker *targetTracker) {
	started := tracker.now()
	if !tracker.lastCycleStart.IsZero() {
		p.Runner.metrics.observeStartDelay(p.Collector.ID, started.Sub(tracker.lastCycleStart)-p.Collector.ScrapeInterval)
	}
	tracker.lastCycleStart = started
	var result CycleResult
	attrs := []any{"collector", p.Collector.ID}
	if err := p.Runner.protect("collector cycle", attrs, func() {
		result = p.Runner.runCycle(ctx, p.Collector, tracker)
	}); err != nil {
		p.Runner.metrics.cyclePanicked(p.Collector.ID)
		result = failedCycle(p.Collector, tracker, started, err)
	}
	if p.Report == nil {
		return
	}
	_ = p.Runner.protect("report cycle", attrs, func() { p.Report(result) })
}

// failedCycle builds the result of a cycle that panicked: every scheduled
// target is marked failed with err.
func failedCycle(collector collectorconfig.CollectorRuntimeConfig, tracker *targetTracker, started time.Time, err error) CycleResult {
	result := CycleResult{CollectorID: collector.ID, Started: started, Finished: tracker.now()}
	for _, target := range collector.Targets {
		if len(collector.TargetNames) > 0 && !contains(collector.TargetNames, target.Name) {
			continue
		}
		state := tracker.get(target.Name)
		result.Targets = append(result.Targets, TargetResult{
			Target:              target.Name,
			State:               TargetFailed,
			Err:                 err,
			ConsecutiveFailures: state.consecutiveFailures,
			LastSuccess:         state.lastSuccess,
			NextAttempt:         state.nextAttempt,
		})
	}
	return result
}
