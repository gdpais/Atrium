package outsystems

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"

	collectorconfig "heartbeat/internal/config"
	"heartbeat/internal/otlp"
	"heartbeat/services/db-collector/internal/collectors"
	collectormetadata "heartbeat/services/db-collector/internal/metadata"
)

// Waiting for the OTel Collector.
//
// The chart's Collector keeps the order of the batches across a restart only
// when its logs export queue holds a single request (values.yaml,
// otlp_http/logs): a larger queue sends the batches it was sending when it
// stopped after the rest, and Loki rejects their rows when it already has
// rows of the same stream more than an hour newer.  With a single request,
// the Collector accepts the next batch only once Loki has taken the previous
// one, so an export can take as long as Loki takes, or as long as Loki is
// down.  That is backpressure, not a failure of the logging database, so the
// export does not run under the probe deadline and does not fail the target:
//
//   - A batch is exported in the background, under the executor's own
//     context rather than the probe's, and the OTLP client retries it with a
//     bounded backoff until the Collector accepts it (otlp.Client).  The
//     watermark moves when it is accepted, as before.
//   - While a log type's export is pending, its extraction stops: no new
//     batch is read for it.  The rows were read, the result set closed and
//     the connection returned to the pool before the export started, so no
//     query, transaction or lock is held on SQL Server while it waits.
//   - A call waits for the batch it read until the probe deadline, so a
//     Collector that keeps up is never seen as blocked.  An export that is
//     still pending at the end of the call blocks the log type: the call
//     returns [collectors.ErrBlocked], the target stays up, and
//     heartbeat_outsystems_export_blocked is 1 until the export ends.  The
//     error names the last failed attempt, and an attempt that failed for
//     another reason than waiting (a wrong endpoint, a 5xx) is logged as a
//     warning, so a Collector that cannot be reached is not mistaken for a
//     busy one.
//   - Waiting is bounded so the target's other log types still run within
//     the cycle: in each cycle, which the call of the target's first log
//     type starts, its calls wait at most half the poll interval in all, and
//     once an export to a Collector is blocked, calls stop waiting for
//     theirs to the same Collector.
//   - An export that ends with an error (a request the Collector rejected
//     for good) fails the next call of its log type, which reads the batch
//     again, as a failed export always has.
//
// The batches held at once, read, being exported or pending, are bounded by
// [Executor.MaxHeldBytes] of row data, so a backlog in every log type while
// the Collector is blocked cannot exhaust the collector's memory: past it, a
// log type reads a smaller batch, and once less than [minBatchBytes] is
// left, it reads nothing and is blocked too until an export ends.

// DefaultMaxHeldBytes bounds the row data of every batch the executor holds
// at once: eight batches of [DefaultMaxBatchBytes], as many as eight targets
// extracting at once.
const DefaultMaxHeldBytes = 8 * DefaultMaxBatchBytes

// minBatchBytes is the least row data a batch is allowed to read when the
// held budget is nearly spent; with less left, the log type waits.
const minBatchBytes = 1 << 20

// warnEvery spaces the warnings of one export's failed attempts.
const warnEvery = 5 * time.Minute

// orphanGrace is how long the exports of a stopped collector keep trying
// before they are cancelled.  A reload stops a changed collector and starts
// it again at once; its exports carry on.  A removed collector's exports are
// cancelled after the grace, and its rows are read again if it comes back.
const orphanGrace = time.Minute

// export is one batch being delivered in the background.
type export struct {
	logType  string
	endpoint string
	rows     int
	started  time.Time
	// done is closed when the export ends; err is set before.
	done chan struct{}
	err  error
	// The fields below are guarded by exportState.mu.
	// blocked reports that the export outlived the call that started it.
	blocked bool
	// lastErr is the error of the last failed attempt, and attempts their
	// number.
	lastErr  error
	attempts int
	// warned is when a failed attempt was last logged as a warning.
	warned time.Time
}

// exportState is the executor's bookkeeping of background exports.
type exportState struct {
	once   sync.Once
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu sync.Mutex
	// pending holds each log type's export while it runs, and after it
	// failed, until the next call of the log type reports the failure.
	pending map[WatermarkKey]*export
	// held is the row data of the batches held, in [rowBytes].
	held int64
	// collectors holds a context per collector for its exports.
	collectors map[string]*collectorExports
	// waits holds each target's time spent waiting for exports in the
	// current cycle.
	waits map[targetKey]*waitWindow
	// firsts holds each target's first log type, whose call starts the
	// target's cycle.
	firsts map[targetKey]string
}

// targetKey is a target of a collector.
type targetKey struct{ collector, target string }

// waitWindow is the time a target's calls spent waiting since start.
type waitWindow struct {
	start time.Time
	used  time.Duration
}

// collectorExports scopes the exports of one collector.
type collectorExports struct {
	ctx    context.Context
	cancel context.CancelFunc
	// orphaned cancels the exports orphanGrace after the collector stopped,
	// unless it starts again first.
	orphaned *time.Timer
}

func (s *exportState) init() {
	s.once.Do(func() {
		s.ctx, s.cancel = context.WithCancel(context.Background())
		s.pending = map[WatermarkKey]*export{}
		s.collectors = map[string]*collectorExports{}
		s.waits = map[targetKey]*waitWindow{}
		s.firsts = map[targetKey]string{}
	})
}

// collectorContext returns the context of collectorID's exports.  s.mu must
// be held.
func (s *exportState) collectorContext(collectorID string) context.Context {
	c := s.collectors[collectorID]
	if c == nil {
		ctx, cancel := context.WithCancel(s.ctx)
		c = &collectorExports{ctx: ctx, cancel: cancel}
		s.collectors[collectorID] = c
	}
	return c.ctx
}

// adopt keeps collectorID's exports going: the collector started (again).
// It also records each target's first log type: the Runner calls a target's
// log types in order, so a call of the first one starts the target's cycle.
func (s *exportState) adopt(collector collectorconfig.CollectorRuntimeConfig) {
	collectorID := collector.ID
	s.init()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, target := range collector.Targets {
		if len(target.Probes) > 0 {
			s.firsts[targetKey{collectorID, target.Name}] = target.Probes[0].Name
		}
	}
	if c := s.collectors[collectorID]; c != nil && c.orphaned != nil {
		c.orphaned.Stop()
		c.orphaned = nil
	}
}

// orphan cancels collectorID's exports after orphanGrace, and forgets its
// failed ones, unless the collector starts again first.
func (s *exportState) orphan(collectorID string, grace time.Duration) {
	s.init()
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.collectors[collectorID]
	if c == nil {
		return
	}
	if c.orphaned != nil {
		c.orphaned.Stop()
	}
	c.orphaned = time.AfterFunc(grace, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.collectors[collectorID] != c || c.orphaned == nil {
			return
		}
		c.cancel()
		delete(s.collectors, collectorID)
		for key := range s.waits {
			if key.collector == collectorID {
				delete(s.waits, key)
			}
		}
		for key := range s.firsts {
			if key.collector == collectorID {
				delete(s.firsts, key)
			}
		}
		for key, p := range s.pending {
			if key.Collector == collectorID && isDone(p) {
				delete(s.pending, key)
			}
		}
	})
}

// reserve takes up to want bytes of the held budget max and returns how
// much a batch may read: want when the budget allows, else what is left, or
// 0, reserving nothing, when less than minBatchBytes (or want) is left.
func (s *exportState) reserve(want, max int64) int64 {
	s.init()
	s.mu.Lock()
	defer s.mu.Unlock()
	grant := min(want, max-s.held)
	if grant < min(want, minBatchBytes) {
		return 0
	}
	s.held += grant
	return grant
}

// release returns n bytes to the held budget.
func (s *exportState) release(n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held -= n
}

// live reports whether collectorID is running: started and not stopped
// since.  The exports of a stopped collector update no metrics, whose series
// were deleted when it stopped.
func (s *exportState) live(collectorID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.collectors[collectorID]
	return c != nil && c.orphaned == nil
}

// heldBytes returns the row data held now.
func (s *exportState) heldBytes() int64 {
	s.init()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held
}

func isDone(e *export) bool {
	select {
	case <-e.done:
		return true
	default:
		return false
	}
}

// startExport sends logs in the background and, once the Collector accepts
// them, counts the rows and saves next as key's watermark.  The batch's held
// bytes are released when the export ends.
func (e *Executor) startExport(callCtx context.Context, item collectormetadata.ScheduledProbe, key WatermarkKey, logs []*logspb.ResourceLogs, batch Batch, rows [][]any, instants []time.Time, next Watermark, location *time.Location) *export {
	s := &e.exports
	s.init()
	endpoint := item.OutSystemsLogs.LogsEndpoint
	pending := &export{logType: key.LogType, endpoint: endpoint, rows: len(rows), started: time.Now(), done: make(chan struct{})}
	s.mu.Lock()
	// Under the executor's context rather than the call's, which ends first, but
	// in the same trace.
	ctx := trace.ContextWithSpanContext(s.collectorContext(item.CollectorID), trace.SpanContextFromContext(callCtx))
	ctx = otlp.WithRetryObserver(ctx, func(attempt int, err error) {
		e.attemptFailed(key, pending, attempt, err)
	})
	s.pending[key] = pending
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		// The span alone for now: the metric waits until it is known that the
		// collector did not stop meanwhile, or it would bring its series back.
		began := time.Now()
		exportCtx, exported := (*Metrics)(nil).stage(ctx, item, stageExport)
		err := e.Exporter.Export(exportCtx, endpoint, logs)
		exported(err)
		metrics := e.Metrics
		if !s.live(item.CollectorID) {
			metrics = nil
		}
		metrics.observeStage(item, stageExport, began)
		if err != nil {
			metrics.exportFailed(item)
			err = fmt.Errorf("log type %s: %w", key.LogType, err)
		} else {
			now := e.clock()
			// Counted before the watermark is saved, so a recount that reads
			// the watermark also sees the rows behind it.
			e.recounts.delivered(key, instants)
			metrics.addRows(item, len(rows))
			_, derived := metrics.stage(ctx, item, stageDerive)
			metrics.observeRows(item, batch, rows, instants)
			derived(nil)
			metrics.setLogDelay(item, now.Sub(Real(instants[len(instants)-1], location)))
			metrics.setLag(item, now.Sub(Real(next.Instant, location)))
			_, saved := metrics.stage(ctx, item, stageWatermark)
			saveErr := e.Store.Save(key, next, now)
			saved(saveErr)
			if saveErr != nil {
				err = fmt.Errorf("log type %s: rows delivered, but the watermark is only kept in memory: %w", key.LogType, saveErr)
			}
		}
		elapsed := time.Since(pending.started)
		s.mu.Lock()
		s.held -= batch.Bytes
		blocked := pending.blocked
		pending.err = err
		close(pending.done)
		// A failure stays for the log type's next call to report, unless
		// the collector is gone: it reads the batch again if it returns.
		if c := s.collectors[item.CollectorID]; (err == nil || c == nil || c.orphaned != nil) && s.pending[key] == pending {
			delete(s.pending, key)
		}
		s.mu.Unlock()
		if blocked {
			metrics.setExportBlocked(item, false)
			attrs := []any{"collector", key.Collector, "target", key.Target, "log_type", key.LogType, "rows", len(rows), "waited", elapsed.Round(time.Millisecond).String()}
			if err != nil {
				e.logger().Warn("outsystems_logs export stopped waiting for the OTel Collector", append(attrs, "error", err)...)
			} else {
				e.logger().Info("outsystems_logs export accepted by the OTel Collector; extraction resumes", attrs...)
			}
		}
	}()
	return pending
}

// attemptFailed records a failed attempt of pending, which the OTLP client
// retries, and logs it as a warning unless the attempt timed out waiting for
// a busy Collector, at most every warnEvery.
func (e *Executor) attemptFailed(key WatermarkKey, pending *export, attempt int, err error) {
	s := &e.exports
	now := time.Now()
	s.mu.Lock()
	pending.lastErr, pending.attempts = err, attempt
	warn := !isTimeout(err) && now.Sub(pending.warned) >= warnEvery
	if warn {
		pending.warned = now
	}
	s.mu.Unlock()
	if warn {
		e.logger().Warn("outsystems_logs export failed; retrying until the OTel Collector accepts it",
			"collector", key.Collector, "target", key.Target, "log_type", key.LogType, "endpoint", pending.endpoint, "attempt", attempt, "error", err)
	}
}

// isTimeout reports whether err is, or wraps, a network timeout: an attempt
// that waited for room in a busy Collector's queue.
func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// startCall notes a call for item: a call of the target's first log type
// starts a new cycle, with the whole wait budget.
func (s *exportState) startCall(item collectormetadata.ScheduledProbe, now time.Time) {
	s.init()
	s.mu.Lock()
	defer s.mu.Unlock()
	key := targetKey{item.CollectorID, item.Target.Name}
	if first, ok := s.firsts[key]; ok && first == item.Definition.Name {
		s.waits[key] = &waitWindow{start: now}
	}
}

// waitBudget returns how long a call for item may wait for an export now:
// what is left of half the poll interval in the target's current cycle.
// The cycle starts with the call of its first log type (startCall); for a
// collector the executor was not told about, an interval after the last
// cycle started.  Without an interval it is unbounded.  s.mu must be held.
func (s *exportState) waitBudget(item collectormetadata.ScheduledProbe, now time.Time) time.Duration {
	interval := time.Duration(item.Assignment.IntervalSeconds) * time.Second
	if interval <= 0 {
		return time.Duration(1<<63 - 1)
	}
	key := targetKey{item.CollectorID, item.Target.Name}
	w := s.waits[key]
	if _, known := s.firsts[key]; w == nil || (!known && now.Sub(w.start) >= interval) {
		w = &waitWindow{start: now}
		s.waits[key] = w
	}
	return max(interval/2-w.used, 0)
}

// awaitExport waits for key's pending export, if any.  With wait false, while
// another export to the same endpoint is blocked, or once the target's calls
// have waited half the poll interval, it only checks.  It returns nil when no
// export is pending or the pending one was accepted, the export's error when
// it failed, and an [collectors.ErrBlocked] error when it is still running,
// which blocks the log type.
func (e *Executor) awaitExport(ctx context.Context, item collectormetadata.ScheduledProbe, key WatermarkKey, wait bool) error {
	s := &e.exports
	s.init()
	s.mu.Lock()
	pending := s.pending[key]
	if pending == nil {
		s.mu.Unlock()
		return nil
	}
	var budget time.Duration
	if wait {
		budget = s.waitBudget(item, time.Now())
		for other, p := range s.pending {
			if other != key && p.blocked && p.endpoint == pending.endpoint {
				budget = 0
				break
			}
		}
	}
	s.mu.Unlock()
	if budget > 0 {
		started := time.Now()
		timer := time.NewTimer(budget)
		select {
		case <-pending.done:
		case <-ctx.Done():
		case <-timer.C:
		}
		timer.Stop()
		s.mu.Lock()
		if w := s.waits[targetKey{item.CollectorID, item.Target.Name}]; w != nil {
			w.used += time.Since(started)
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	if isDone(pending) {
		if s.pending[key] == pending {
			delete(s.pending, key)
		}
		s.mu.Unlock()
		return pending.err
	}
	first := !pending.blocked
	pending.blocked = true
	lastErr, attempts := pending.lastErr, pending.attempts
	// Set on every call, since a reload recreates the gauge at 0, and under
	// the lock, so the export ending sets 0 after it.
	e.Metrics.setExportBlocked(item, true)
	s.mu.Unlock()
	waited := time.Since(pending.started).Round(time.Second)
	if first {
		// Info, not a warning: a busy Collector is normal during a backlog,
		// and the lag alert covers a long wait.
		e.logger().Info("outsystems_logs export is waiting for the OTel Collector; this log type is paused until it is accepted",
			"collector", key.Collector, "target", key.Target, "log_type", key.LogType, "rows", pending.rows, "endpoint", pending.endpoint)
	}
	if lastErr != nil {
		return fmt.Errorf("log type %s: %d rows waiting for the OTel Collector at %s to accept them, for %s (%d attempts failed, the last: %v): %w",
			key.LogType, pending.rows, pending.endpoint, waited, attempts, lastErr, collectors.ErrBlocked)
	}
	return fmt.Errorf("log type %s: %d rows waiting for the OTel Collector at %s to accept them, for %s: %w",
		key.LogType, pending.rows, pending.endpoint, waited, collectors.ErrBlocked)
}

// Close stops the background exports: it waits for them until ctx ends, then
// cancels the rest and waits for them to return.  A cancelled export leaves
// its watermark, so its rows are read again after a restart.  Call it after
// every poller has stopped.
func (e *Executor) Close(ctx context.Context) {
	s := &e.exports
	s.init()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		s.cancel()
		<-done
	}
	s.cancel()
}
