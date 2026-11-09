package outsystems

// The recount (ADR 0008): once a minute, each log type counts again, per
// minute of Instant, the rows its query returns in the recount_window behind
// its watermark, and compares them with the rows it delivered there.
//
//   - More rows in the database than delivered: rows the log service
//     committed after the watermark had passed them, later than the lag.
//     They are never read; [MetricLateRows] counts them.
//   - Fewer: rows delivered more often than the database holds them, read
//     twice, which the extraction must never do, or deleted after they were
//     sent; [MetricDuplicateRows] counts them.  A batch sent again after
//     the Collector took it is delivered once here, so it is not counted.
//
// It only counts: nothing is read again or sent, so the recount cannot
// duplicate a row in Loki.  Each minute's difference is counted once, the
// first time a recount sees it, however many recounts cover that minute.
//
// A minute is (end − 1m, end], the bounds the query contract filters with,
// so the window (lo, hi], both whole minutes, holds whole minutes only.  The
// delivered counts live in memory and start again, after the watermark the
// call starts at, whenever they could no longer be compared: after a
// restart, once the watermark belongs to another database or moved back,
// and when a reload changes the recount_window or the query.  Only the
// minutes after that watermark are compared, which no earlier process or
// setting delivered into.  A log type with the recount off keeps no counts.
//
// The count wraps the log type's own query, query_template included, as a
// derived table, so it counts what extraction reads, with the same filters
// and joins; SQL Server reads only the Instant column of its index for it.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	collectorconfig "atrium/internal/config"
	collectormetadata "atrium/services/db-collector/internal/metadata"
)

// recountEvery is how often a log type is recounted: once in each minute of
// the clock that has a poll, whatever the poll interval.
const recountEvery = time.Minute

// RecountLimit bounds the rows one recount counts (its @limit).  A window
// that holds more is not compared: shorten recount_window.
const RecountLimit = 1_000_000

// Recounter counts a log type's rows by minute; [SQLServerSource] implements
// it.  Without it, a Source is never recounted.
type Recounter interface {
	// Recount runs query, a recount query (recountSQL), with params, and
	// returns its rows: minute k, ending at params.From + k minutes, to the
	// rows the log type's query returns in that minute.
	Recount(ctx context.Context, target collectormetadata.DatabaseTarget, query string, params QueryParams) (map[int64]int64, error)
}

// recountSQL wraps statement, a log-type query as a derived table
// ([collectorconfig.DerivedTable]), in the count of its rows per minute of
// Instant after @from: minute k holds Instant in (@from + k − 1, @from + k].
func recountSQL(statement string) string {
	return "SELECT b.recount_minute, COUNT_BIG(*) AS recount_rows FROM (\n" +
		"SELECT DATEDIFF(MINUTE, @from, q.Instant)\n" +
		"  + CASE WHEN q.Instant > DATEADD(MINUTE, DATEDIFF(MINUTE, @from, q.Instant), @from) THEN 1 ELSE 0 END AS recount_minute\n" +
		"FROM (\n" + statement + "\n) AS q\n" +
		") AS b\nGROUP BY b.recount_minute"
}

// minuteEnd returns the end of the minute (end − 1m, end] that holds t.
func minuteEnd(t time.Time) time.Time {
	end := t.Truncate(time.Minute)
	if end.Before(t) {
		end = end.Add(time.Minute)
	}
	return end
}

// recountState keeps what each log type delivered, and what its recounts
// found, by the end of the minute of Instant.
type recountState struct {
	mu   sync.Mutex
	logs map[WatermarkKey]*recountLog
}

type recountLog struct {
	// source is the database the counts are of (Watermark.Source).
	source string
	// floor is where the counts start: a minute ending after it was
	// delivered by this process alone.
	floor time.Time
	// high is the furthest watermark seen; one behind it starts again.
	high time.Time
	// window and query are the settings the counts were taken with; others
	// start them again.
	window time.Duration
	// delivered, late and duplicate count rows by the end of their minute;
	// late and duplicate are what earlier recounts found there.
	delivered, late, duplicate map[time.Time]int64
	// last is the minute of the last recount; warned when its last failure
	// was logged.
	last, warned time.Time
	// query and statement cache the log type's query as a derived table,
	// or why it cannot be one (statementErr).
	query, statement string
	statementErr     error
}

// start keeps key's counts for a call that starts at watermark with window
// and query, or starts them again when there are none, they are of another
// database, the watermark moved back, or window or query changed.  It runs
// only once no export of key is pending, so every row up to watermark was
// counted under the settings before.
func (s *recountState) start(key WatermarkKey, watermark Watermark, window time.Duration, query string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.logs == nil {
		s.logs = map[WatermarkKey]*recountLog{}
	}
	log := s.logs[key]
	if log != nil && log.source == watermark.Source && !watermark.Instant.Before(log.high) &&
		log.window == window && log.query == query {
		log.high = watermark.Instant
		return
	}
	s.logs[key] = &recountLog{
		source: watermark.Source, floor: minuteEnd(watermark.Instant), high: watermark.Instant,
		window: window, query: query,
		delivered: map[time.Time]int64{}, late: map[time.Time]int64{}, duplicate: map[time.Time]int64{},
	}
}

// forget drops key's counts: its recount is off.
func (s *recountState) forget(key WatermarkKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.logs, key)
}

// delivered counts instants, delivered to the Collector, and drops the
// counts that fell out of the window behind the newest.
func (s *recountState) delivered(key WatermarkKey, instants []time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	log := s.logs[key]
	if log == nil || len(instants) == 0 {
		return
	}
	for _, instant := range instants {
		log.delivered[minuteEnd(instant)]++
	}
	log.prune(minuteEnd(instants[len(instants)-1]).Add(-log.window - time.Minute))
}

// prune drops the counts of the minutes ending at or before oldest.
func (l *recountLog) prune(oldest time.Time) {
	for _, counts := range []map[time.Time]int64{l.delivered, l.late, l.duplicate} {
		for end := range counts {
			if !end.After(oldest) {
				delete(counts, end)
			}
		}
	}
}

// due reports whether key is due a recount at now, its first in now's
// minute, and if so takes it.
func (s *recountState) due(key WatermarkKey, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	minute := now.Truncate(recountEvery)
	log := s.logs[key]
	if log == nil || !minute.After(log.last) {
		return false
	}
	log.last = minute
	return true
}

// window returns the minutes a recount of key at watermark compares, (lo,
// hi]: those the watermark has passed, at most window back, after the
// floor.  ok is false when there are none.
func (s *recountState) window(key WatermarkKey, watermark time.Time, window time.Duration) (lo, hi time.Time, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	log := s.logs[key]
	if log == nil {
		return time.Time{}, time.Time{}, false
	}
	hi = watermark.Truncate(time.Minute)
	lo = hi.Add(-window)
	if lo.Before(log.floor) {
		lo = log.floor
	}
	return lo, hi, hi.After(lo)
}

// compare takes a recount of key over (lo, hi] and returns the rows it
// newly found late and duplicated.
func (s *recountState) compare(key WatermarkKey, lo, hi time.Time, counts map[int64]int64) (late, duplicate int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	log := s.logs[key]
	if log == nil {
		return 0, 0
	}
	for end := lo.Add(time.Minute); !end.After(hi); end = end.Add(time.Minute) {
		extra := counts[int64(end.Sub(lo)/time.Minute)] - log.delivered[end]
		switch {
		case extra > log.late[end]:
			late += extra - log.late[end]
			log.late[end] = extra
		case -extra > log.duplicate[end]:
			duplicate += -extra - log.duplicate[end]
			log.duplicate[end] = -extra
		}
	}
	// No later recount compares a minute at or before lo: the watermark
	// only moves on, and the counts start again when it moves back.  So a
	// log type that delivers nothing, every row late, keeps the window only.
	log.prune(lo)
	return late, duplicate
}

// statement returns key's query as a derived table, cached.
func (s *recountState) statement(key WatermarkKey) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	log := s.logs[key]
	if log == nil {
		return "", errors.New("no counts")
	}
	if log.statement == "" && log.statementErr == nil {
		log.statement, log.statementErr = collectorconfig.DerivedTable(log.query)
	}
	return log.statement, log.statementErr
}

// warn reports whether a failure of key at now is to be logged: the first,
// then at most every warnEvery.
func (s *recountState) warn(key WatermarkKey, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	log := s.logs[key]
	if log == nil || now.Sub(log.warned) < warnEvery {
		return false
	}
	log.warned = now
	return true
}

// recount compares key's rows in the window behind its stored watermark
// with what it delivered there, when it is due and the deadline leaves time.
// A failure fails only the recount: it is counted and logged, and the
// extraction goes on.
func (e *Executor) recount(ctx context.Context, item collectormetadata.ScheduledProbe, key WatermarkKey) {
	window := item.OutSystemsLogs.RecountWindow
	recounter, ok := e.Source.(Recounter)
	// A second at least: the count reads the Instant index of the window.
	if window <= 0 || !ok || !timeLeft(ctx, time.Second) || !e.recounts.due(key, e.clock()) {
		return
	}
	e.Metrics.recountRan(item)
	err := e.runRecount(ctx, item, key, window, recounter)
	if err == nil {
		return
	}
	e.Metrics.recountFailed(item)
	if e.recounts.warn(key, e.clock()) {
		e.logger().Warn("outsystems_logs recount failed; rows committed past the lag are not counted meanwhile",
			"collector", key.Collector, "target", key.Target, "log_type", key.LogType, "error", err)
	}
}

func (e *Executor) runRecount(ctx context.Context, item collectormetadata.ScheduledProbe, key WatermarkKey, window time.Duration, recounter Recounter) error {
	watermark, found, err := e.Store.Load(key)
	if err != nil || !found {
		// Nothing delivered or saved yet: nothing to compare.
		return err
	}
	lo, hi, ok := e.recounts.window(key, watermark.Instant, window)
	if !ok {
		return nil
	}
	statement, err := e.recounts.statement(key)
	if err != nil {
		return fmt.Errorf("the query cannot be counted: it %w; set recount_window: 0s for this collector", err)
	}
	counts, err := recounter.Recount(ctx, item.Target, recountSQL(statement), QueryParams{
		From: lo, To: hi, Limit: RecountLimit, DateTime: watermark.InstantType == "DATETIME",
	})
	if err != nil {
		return fmt.Errorf("log type %s: %w", item.Definition.Name, err)
	}
	var total int64
	for _, n := range counts {
		total += n
	}
	if total >= RecountLimit {
		return fmt.Errorf("log type %s: %d or more rows in the last %s behind the watermark, more than one recount counts; shorten recount_window", item.Definition.Name, RecountLimit, window)
	}
	late, duplicate := e.recounts.compare(key, lo, hi, counts)
	e.Metrics.recounted(item, late, duplicate)
	if late > 0 || duplicate > 0 {
		e.logger().Warn("outsystems_logs recount found rows the extraction missed or sent twice",
			"collector", key.Collector, "target", key.Target, "log_type", key.LogType,
			"late_rows", late, "duplicate_rows", duplicate, "from", lo.Format(wallLayout), "to", hi.Format(wallLayout))
	}
	return nil
}
