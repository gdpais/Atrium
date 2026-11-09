package outsystems

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	collectorconfig "atrium/internal/config"
	collectormetadata "atrium/services/db-collector/internal/metadata"
	catalogoutsystems "atrium/services/db-collector/internal/probes/outsystems"
)

// recountSource is a fakeSource that also recounts: the rows in (From, To],
// at most Limit of them in Instant order, by the minute that ends at From
// plus k minutes.  Rows whose key is in filtered are left out, as a
// query_template with a filter would.
type recountSource struct {
	*fakeSource
	recounts int
	err      error
	filtered map[string]bool
}

func (s *recountSource) Recount(_ context.Context, _ collectormetadata.DatabaseTarget, query string, params QueryParams) (map[int64]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recounts++
	if s.err != nil {
		return nil, s.err
	}
	if !strings.Contains(query, "GROUP BY b.recount_minute") {
		return nil, errors.New("not a recount query")
	}
	var instants []time.Time
	for _, row := range s.rows {
		if s.filtered[row.key] {
			continue
		}
		if row.instant.After(params.From) && !row.instant.After(params.To) {
			instants = append(instants, row.instant)
		}
	}
	slices.SortFunc(instants, func(a, b time.Time) int { return a.Compare(b) })
	if len(instants) > params.Limit {
		instants = instants[:params.Limit]
	}
	counts := map[int64]int64{}
	for _, instant := range instants {
		counts[int64(minuteEnd(instant).Sub(params.From)/time.Minute)]++
	}
	return counts, nil
}

// newRecountExtraction is an extraction with a 10-minute recount window.
func newRecountExtraction(t *testing.T, dir string, limit int) (*extraction, *recountSource) {
	t.Helper()
	x := newExtraction(t, dir, limit)
	source := &recountSource{fakeSource: x.source}
	x.executor.Source = source
	x.item.OutSystemsLogs.RecountWindow = 10 * time.Minute
	return x, source
}

// A row committed after the watermark passed its Instant is counted once as
// late, never sent, and never counted again by later recounts.
func TestRecountCountsRowsCommittedPastTheLagOnce(t *testing.T) {
	x, source := newRecountExtraction(t, t.TempDir(), 100)
	for i := 1; i <= 8; i++ {
		x.source.insert("on-time", at(time.Duration(i)*time.Minute))
	}
	x.mustRun() // reads up to now − lag = t0+9m, recounts it: nothing late
	if source.recounts != 1 || x.counter(x.metrics.lateRows) != 0 {
		t.Fatalf("after the first cycle: %d recounts, %v late rows, want 1 and 0", source.recounts, x.counter(x.metrics.lateRows))
	}
	// Committed now, behind the watermark.
	x.source.insert("late-1", at(2*time.Minute+30*time.Second))
	x.source.insert("late-2", at(2*time.Minute+31*time.Second))
	x.now = x.now.Add(15 * time.Second)
	x.mustRun() // not due yet: a minute between recounts
	if source.recounts != 1 {
		t.Fatalf("recounted %d times within the minute, want once", source.recounts)
	}
	for range 5 {
		x.now = x.now.Add(time.Minute)
		x.mustRun()
	}
	if got := x.counter(x.metrics.lateRows); got != 2 {
		t.Fatalf("late rows = %v after %d recounts, want 2, counted once", got, source.recounts)
	}
	if slices.ContainsFunc(x.exporter.keys(), func(k string) bool { return strings.HasPrefix(k, "late") }) {
		t.Fatalf("a late row was sent: %v", x.exporter.keys())
	}
	if got := x.counter(x.metrics.duplicateRows); got != 0 {
		t.Fatalf("duplicate rows = %v, want 0", got)
	}
	// The watermark is at t0+15m15s now. One more, in a minute recounted
	// before, and one beyond the 10-minute window, which goes uncounted.
	x.source.insert("late-3", at(11*time.Minute+40*time.Second))
	x.source.insert("too-late", at(2*time.Minute+40*time.Second))
	x.now = x.now.Add(time.Minute)
	x.mustRun()
	if got := x.counter(x.metrics.lateRows); got != 3 {
		t.Fatalf("late rows = %v, want 3", got)
	}
	if !strings.Contains(x.logs.String(), "late_rows=1") {
		t.Fatalf("the late row was not logged:\n%s", x.logs)
	}
}

// Full batches cut at an Instant, and rows sharing an Instant across a cut,
// are neither late nor duplicated.
func TestRecountMatchesFullBatches(t *testing.T) {
	x, source := newRecountExtraction(t, t.TempDir(), 3)
	for i := range 40 {
		instant := at(time.Duration(i/2) * 12 * time.Second) // two rows per Instant
		x.source.insert("row", instant)
	}
	for range 6 {
		x.mustRun()
		x.now = x.now.Add(time.Minute)
	}
	if len(x.exporter.keys()) != 40 {
		t.Fatalf("delivered %d rows, want all 40", len(x.exporter.keys()))
	}
	if source.recounts == 0 || x.counter(x.metrics.lateRows) != 0 || x.counter(x.metrics.duplicateRows) != 0 {
		t.Fatalf("%d recounts found %v late and %v duplicate rows, want none", source.recounts, x.counter(x.metrics.lateRows), x.counter(x.metrics.duplicateRows))
	}
}

// After a restart the counts start at the stored watermark: rows an earlier
// process delivered are not taken for late ones.
func TestRecountAfterARestartComparesOnlyItsOwnMinutes(t *testing.T) {
	dir := t.TempDir()
	first, _ := newRecountExtraction(t, dir, 100)
	for i := 1; i <= 8; i++ {
		first.source.insert("before-restart", at(time.Duration(i)*time.Minute))
	}
	first.mustRun()

	second, source := newRecountExtraction(t, dir, 100)
	second.source.rows = first.source.rows
	second.now = first.now.Add(2 * time.Minute)
	second.source.insert("after-restart", second.now.Add(-90*time.Second))
	for range 3 {
		second.mustRun()
		second.now = second.now.Add(time.Minute)
	}
	if source.recounts == 0 {
		t.Fatal("the restarted executor never recounted")
	}
	if got := second.counter(second.metrics.lateRows); got != 0 {
		t.Fatalf("late rows = %v after a restart, want 0: the first process delivered them", got)
	}
	if got := second.exporter.keys(); !slices.Equal(got, []string{"after-restart"}) {
		t.Fatalf("delivered %v after the restart, want only after-restart", got)
	}
}

// A failed recount is counted and logged, and fails nothing else.
func TestRecountFailureFailsOnlyTheRecount(t *testing.T) {
	x, source := newRecountExtraction(t, t.TempDir(), 100)
	source.err = errors.New("timeout")
	x.source.insert("on-time", at(time.Minute))
	x.mustRun()
	x.now = x.now.Add(time.Minute)
	x.mustRun()
	if got := x.counter(x.metrics.recountFailures); got != 2 {
		t.Fatalf("recount failures = %v, want 2", got)
	}
	if strings.Count(x.logs.String(), "outsystems_logs recount failed") != 1 {
		t.Fatalf("want the failure logged once within warnEvery:\n%s", x.logs)
	}
	if got := x.exporter.keys(); !slices.Equal(got, []string{"on-time"}) {
		t.Fatalf("delivered %v, want on-time", got)
	}
}

// recount_window 0s, or a Source that cannot recount, recounts nothing.
func TestRecountOff(t *testing.T) {
	x, source := newRecountExtraction(t, t.TempDir(), 100)
	x.item.OutSystemsLogs.RecountWindow = 0
	x.source.insert("on-time", at(time.Minute))
	x.mustRun()
	if source.recounts != 0 {
		t.Fatalf("recounted %d times with recount_window 0s", source.recounts)
	}
	y := newExtraction(t, t.TempDir(), 100)
	y.item.OutSystemsLogs.RecountWindow = 10 * time.Minute
	y.source.insert("on-time", at(time.Minute))
	y.mustRun() // fakeSource has no Recount
	if got := y.counter(y.metrics.recountFailures); got != 0 {
		t.Fatalf("recount failures = %v without a Recounter, want 0", got)
	}
}

// The comparison counts each minute's difference once, and a minute holding
// fewer rows than were delivered as duplicates.
func TestRecountCompare(t *testing.T) {
	var s recountState
	key := WatermarkKey{Collector: "c", Target: "t", LogType: "error"}
	s.start(key, Watermark{Instant: at(0), Source: "db"}, 10*time.Minute, "SELECT 1")
	s.delivered(key, []time.Time{at(30 * time.Second), at(time.Minute), at(90 * time.Second), at(90 * time.Second)})
	// Minute 1 is (t0, t0+1m], minute 2 (t0+1m, t0+2m].
	lo, hi, ok := s.window(key, at(2*time.Minute+10*time.Second), 10*time.Minute)
	if !ok || !lo.Equal(at(0)) || !hi.Equal(at(2*time.Minute)) {
		t.Fatalf("window = (%s, %s] %v, want (t0, t0+2m]", lo, hi, ok)
	}
	if late, duplicate := s.compare(key, lo, hi, map[int64]int64{1: 3, 2: 2}); late != 1 || duplicate != 0 {
		t.Fatalf("first recount: %d late, %d duplicate, want 1 and 0", late, duplicate)
	}
	if late, duplicate := s.compare(key, lo, hi, map[int64]int64{1: 3, 2: 2}); late != 0 || duplicate != 0 {
		t.Fatalf("same counts again: %d late, %d duplicate, want none", late, duplicate)
	}
	if late, duplicate := s.compare(key, lo, hi, map[int64]int64{1: 4, 2: 1}); late != 1 || duplicate != 1 {
		t.Fatalf("one more late, one fewer: %d late, %d duplicate, want 1 and 1", late, duplicate)
	}
	// Another database starts the counts again, from its watermark.
	s.start(key, Watermark{Instant: at(5 * time.Minute), Source: "other"}, 10*time.Minute, "SELECT 1")
	if _, _, ok := s.window(key, at(5*time.Minute+50*time.Second), 10*time.Minute); ok {
		t.Fatal("a window before the new floor")
	}
}

// A log type that delivers nothing, every row of it late, counts each
// minute once and keeps the counts of the window only.
func TestRecountKeepsOnlyTheWindowWithoutDeliveries(t *testing.T) {
	var s recountState
	key := WatermarkKey{Collector: "c", Target: "t", LogType: "cyclic_job"}
	window := 10 * time.Minute
	s.start(key, Watermark{Instant: at(0), Source: "db"}, window, "SELECT 1")
	var late int64
	for m := 1; m <= 120; m++ {
		lo, hi, ok := s.window(key, at(time.Duration(m)*time.Minute), window)
		if !ok {
			t.Fatalf("no window at t0+%dm", m)
		}
		counts := map[int64]int64{}
		for k := int64(1); k <= int64(hi.Sub(lo)/time.Minute); k++ {
			counts[k] = 1
		}
		n, _ := s.compare(key, lo, hi, counts)
		late += n
	}
	if late != 120 {
		t.Fatalf("late rows = %d, want 120, one a minute", late)
	}
	if n := len(s.logs[key].late); n > 10 {
		t.Fatalf("kept the late counts of %d minutes, want at most the window's 10", n)
	}
}

func TestMinuteEnd(t *testing.T) {
	for _, tc := range []struct{ in, want time.Duration }{
		{0, 0}, {time.Nanosecond, time.Minute}, {time.Minute, time.Minute}, {61 * time.Second, 2 * time.Minute},
	} {
		if got := minuteEnd(at(tc.in)); !got.Equal(at(tc.want)) {
			t.Errorf("minuteEnd(t0+%s) = %s, want t0+%s", tc.in, got, tc.want)
		}
	}
}

// Every built-in query can be recounted: it is a SELECT a derived table
// holds.
func TestBuiltInQueriesCanBeRecounted(t *testing.T) {
	catalog := catalogoutsystems.DefaultCatalog()
	for _, name := range catalog.Names() {
		query, _ := catalog.Get(name)
		statement, err := collectorconfig.DerivedTable(query.SQL)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if strings.HasSuffix(strings.TrimSpace(statement), ";") || !strings.Contains(recountSQL(statement), statement) {
			t.Errorf("%s: statement not wrapped as is", name)
		}
	}
}

// cycles runs n cycles a minute apart, a row committed on time each minute,
// and returns the late and duplicate rows counted.
func (x *extraction) cycles(n int, key string) (late, duplicate float64) {
	x.t.Helper()
	for range n {
		x.source.insert(key, x.now.Add(-90*time.Second))
		x.mustRun()
		x.now = x.now.Add(time.Minute)
	}
	return x.counter(x.metrics.lateRows), x.counter(x.metrics.duplicateRows)
}

// A reload that widens recount_window, turns the recount off and on again,
// or changes the query starts the counts again after the watermark: the
// minutes delivered before it are never compared under the new settings.
func TestRecountStartsAgainWhenItsSettingsChange(t *testing.T) {
	t.Run("wider window", func(t *testing.T) {
		x, _ := newRecountExtraction(t, t.TempDir(), 100)
		x.cycles(20, "row")
		x.item.OutSystemsLogs.RecountWindow = 30 * time.Minute
		if late, duplicate := x.cycles(5, "row"); late != 0 || duplicate != 0 {
			t.Fatalf("after widening the window: %v late, %v duplicate rows, want none", late, duplicate)
		}
	})
	t.Run("off and on again", func(t *testing.T) {
		x, source := newRecountExtraction(t, t.TempDir(), 100)
		x.cycles(12, "row")
		x.item.OutSystemsLogs.RecountWindow = 0
		before := source.recounts
		x.cycles(5, "row")
		if source.recounts != before {
			t.Fatalf("recounted %d times with the recount off", source.recounts-before)
		}
		x.item.OutSystemsLogs.RecountWindow = 10 * time.Minute
		if late, duplicate := x.cycles(5, "row"); late != 0 || duplicate != 0 {
			t.Fatalf("after turning it on again: %v late, %v duplicate rows, want none", late, duplicate)
		}
	})
	t.Run("new query", func(t *testing.T) {
		x, source := newRecountExtraction(t, t.TempDir(), 100)
		x.cycles(12, "filtered")
		// The new template filters those rows out; the source still returns
		// them to extraction, so only the comparison sees the change.
		x.item.Definition.QueryTemplate = "SELECT TOP (@limit) Instant, Message FROM dbo.oslog_Error WHERE Instant > @from AND Instant <= @to ORDER BY Instant"
		source.filtered = map[string]bool{"filtered": true}
		if late, duplicate := x.cycles(5, "kept"); late != 0 || duplicate != 0 {
			t.Fatalf("after a query change: %v late, %v duplicate rows, want none", late, duplicate)
		}
	})
}

// A log type is recounted once in each minute that has a poll, whatever the
// poll interval and however long each call takes.
func TestRecountOncePerMinute(t *testing.T) {
	x, source := newRecountExtraction(t, t.TempDir(), 100)
	// 15 minutes of polls on a 30-second ticker; each call takes 0 to 2
	// seconds, so a recount two polls on is sometimes 58 seconds later.
	base := x.now.Truncate(time.Minute)
	for i := range 30 {
		x.now = base.Add(time.Duration(i)*30*time.Second + []time.Duration{2 * time.Second, 0, time.Second}[i%3])
		x.mustRun()
	}
	if source.recounts < 15 || source.recounts > 16 {
		t.Fatalf("%d recounts in 15 minutes of 30s polls, want one a minute", source.recounts)
	}
	if got := x.counter(x.metrics.recounts); int(got) != source.recounts {
		t.Fatalf("recounts counter = %v, want %d", got, source.recounts)
	}
}
