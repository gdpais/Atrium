package outsystems

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"

	collectorconfig "atrium/internal/config"
	"atrium/internal/otlp"
	"atrium/services/db-collector/internal/collectors"
	collectormetadata "atrium/services/db-collector/internal/metadata"
)

// gatedExporter is an OTel Collector that holds every export until it is
// released, as the chart's Collector does while Loki has not taken the batch
// before.  It honours the context, as otlp.Client does.
type gatedExporter struct {
	fakeExporter
	mu      sync.Mutex
	gate    chan struct{}
	fail    error
	started int
	// sourceOpen, when set, reports whether a query is still running; an
	// export that starts while one is fails the test.
	sourceOpen func() bool
	t          *testing.T
}

func newGatedExporter(t *testing.T) *gatedExporter {
	return &gatedExporter{gate: make(chan struct{}), t: t}
}

func (g *gatedExporter) Export(ctx context.Context, endpoint string, resourceLogs []*logspb.ResourceLogs) error {
	g.mu.Lock()
	g.started++
	gate, fail := g.gate, g.fail
	g.mu.Unlock()
	if g.sourceOpen != nil && g.sourceOpen() {
		g.t.Errorf("an export started while the query was still open")
	}
	select {
	case <-gate:
	case <-ctx.Done():
		return ctx.Err()
	}
	if fail != nil {
		return fail
	}
	return g.fakeExporter.Export(ctx, endpoint, resourceLogs)
}

// release lets the held exports, and every later one, through.
func (g *gatedExporter) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	close(g.gate)
}

// eventually reports whether cond holds within a second.
func eventually(cond func() bool) bool {
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return cond()
}

func (g *gatedExporter) exports() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.started
}

// gated returns x with a gated exporter.
func gated(x *extraction) *gatedExporter {
	g := newGatedExporter(x.t)
	x.executor.Exporter = g
	return g
}

// runFor runs one call with a probe deadline of d.
func (x *extraction) runFor(d time.Duration) error {
	x.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	_, _, err := x.executor.RunProbe(ctx, x.item)
	return err
}

// settle waits for every background export to end.
func (x *extraction) settle() {
	x.t.Helper()
	done := make(chan struct{})
	go func() {
		x.executor.exports.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		x.t.Fatal("background exports did not end")
	}
}

func (x *extraction) savedWatermark() (Watermark, bool) {
	x.t.Helper()
	watermark, found, err := NewStore(x.executor.Store.Dir()).Load(WatermarkKey{Collector: "oslogs", Target: "logging-db", LogType: "error"})
	if err != nil {
		x.t.Fatal(err)
	}
	return watermark, found
}

// A Collector that holds a batch past the probe deadline is backpressure:
// the call returns collectors.ErrBlocked, not a failure, the log type reads
// nothing more while it waits, and once the Collector accepts the batch the
// watermark moves and extraction resumes where it stopped, in order.
func TestExtractionWaitsForABlockedCollectorWithoutFailing(t *testing.T) {
	x := newExtraction(t, t.TempDir(), 2)
	g := gated(x)
	for i, key := range []string{"a", "b", "c", "d"} {
		x.source.insert(key, at(time.Duration(i+1)*time.Minute))
	}

	err := x.runFor(50 * time.Millisecond)
	if !errors.Is(err, collectors.ErrBlocked) || !strings.Contains(err.Error(), "1 rows waiting for the OTel Collector at http://otel:4318") {
		t.Fatalf("RunProbe error = %v, want ErrBlocked", err)
	}
	if got := x.gauge(x.metrics.exportBlocked); got != 1 {
		t.Fatalf("export blocked = %v, want 1", got)
	}
	if got := x.counter(x.metrics.exportFailures); got != 0 {
		t.Fatalf("export failures = %v, want 0: waiting is not failing", got)
	}
	if _, found := x.savedWatermark(); found {
		t.Fatal("the watermark moved before the Collector accepted the batch")
	}
	if !strings.Contains(x.logs.String(), "waiting for the OTel Collector; this log type is paused") {
		t.Fatalf("no warning when the export blocked:\n%s", x.logs)
	}

	// While it waits, later calls neither read nor export, and return at once.
	started := time.Now()
	if err := x.runFor(5 * time.Second); !errors.Is(err, collectors.ErrBlocked) {
		t.Fatalf("second call: %v, want ErrBlocked", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("a call with a blocked export took %s, want it to return at once", elapsed)
	}
	if len(x.source.params) != 1 || g.exports() != 1 {
		t.Fatalf("%d queries and %d exports while blocked, want 1 and 1", len(x.source.params), g.exports())
	}

	g.release()
	x.settle()
	if got := x.gauge(x.metrics.exportBlocked); got != 0 {
		t.Fatalf("export blocked = %v after the export, want 0", got)
	}
	if watermark, found := x.savedWatermark(); !found || !watermark.Instant.Equal(at(time.Minute)) {
		t.Fatalf("watermark %+v (found %v), want the accepted batch's end", watermark, found)
	}
	if got := x.counter(x.metrics.rows); got != 1 {
		t.Fatalf("rows extracted = %v, want 1", got)
	}
	if !strings.Contains(x.logs.String(), "accepted by the OTel Collector; extraction resumes") {
		t.Fatalf("no log when the export was accepted:\n%s", x.logs)
	}
	x.mustRun()
	if got := x.exporter.keys(); len(got) != 0 {
		t.Fatalf("the plain exporter was used: %v", got)
	}
	if got := g.keys(); !slices.Equal(got, []string{"a", "b", "c", "d"}) {
		t.Fatalf("delivered %v, want every row once, in order", got)
	}
}

// The query has returned and released its connection before the export
// starts, so nothing is held open on SQL Server while the export waits.
func TestExtractionClosesTheQueryBeforeExporting(t *testing.T) {
	x := newExtraction(t, t.TempDir(), 100)
	open := &openSource{fakeSource: x.source}
	x.executor.Source = open
	g := gated(x)
	g.sourceOpen = open.isOpen
	x.source.insert("a", at(time.Minute))
	if err := x.runFor(50 * time.Millisecond); !errors.Is(err, collectors.ErrBlocked) {
		t.Fatalf("RunProbe: %v", err)
	}
	if open.isOpen() {
		t.Fatal("the query is still open while the export waits")
	}
	g.release()
	x.settle()
}

// openSource reports whether a query is running.
type openSource struct {
	*fakeSource
	mu   sync.Mutex
	open bool
}

func (s *openSource) Query(ctx context.Context, target collectormetadata.DatabaseTarget, query string, params QueryParams) (Batch, error) {
	s.mu.Lock()
	s.open = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.open = false
		s.mu.Unlock()
	}()
	return s.fakeSource.Query(ctx, target, query, params)
}

func (s *openSource) isOpen() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.open
}

// Once one log type is blocked, the Collector is known to be busy: the
// target's other log types start their export and return at once instead of
// waiting until the deadline, so every log type still runs in the cycle.
func TestABlockedExportStopsOtherLogTypesFromWaiting(t *testing.T) {
	x := newExtraction(t, t.TempDir(), 100)
	g := gated(x)
	x.source.insert("a", at(time.Minute))
	if err := x.runFor(50 * time.Millisecond); !errors.Is(err, collectors.ErrBlocked) {
		t.Fatalf("error log type: %v", err)
	}
	general := x.item
	general.Definition.Name = "general"
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := x.executor.RunProbe(ctx, general)
	if !errors.Is(err, collectors.ErrBlocked) || time.Since(started) > time.Second {
		t.Fatalf("general log type: %v after %s; want ErrBlocked at once", err, time.Since(started))
	}
	if !eventually(func() bool { return g.exports() == 2 }) {
		t.Fatalf("%d exports, want one per log type", g.exports())
	}
	g.release()
	x.settle()
}

// An export the Collector rejects for good fails the log type's next call,
// which counts it; the call after reads the batch again.
func TestAFailedBackgroundExportFailsTheNextCall(t *testing.T) {
	dir := t.TempDir()
	x := newExtraction(t, dir, 100)
	g := gated(x)
	g.fail = errors.New("receiver rejected the request: 400 Bad Request")
	x.source.insert("a", at(time.Minute))
	if err := x.runFor(50 * time.Millisecond); !errors.Is(err, collectors.ErrBlocked) {
		t.Fatalf("first call: %v", err)
	}
	g.release()
	x.settle()
	if got := x.counter(x.metrics.exportFailures); got != 1 {
		t.Fatalf("export failures = %v, want 1", got)
	}
	err := x.run()
	if err == nil || errors.Is(err, collectors.ErrBlocked) || !strings.Contains(err.Error(), "400 Bad Request") {
		t.Fatalf("second call: %v, want the export's error", err)
	}
	if got := x.gauge(x.metrics.exportBlocked); got != 0 {
		t.Fatalf("export blocked = %v after the export ended, want 0", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("a watermark was saved after a failed export: %v", entries)
	}
	g.mu.Lock()
	g.fail = nil
	g.mu.Unlock()
	x.mustRun()
	if got := g.keys(); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("delivered %v after the failure", got)
	}
}

// The batches held while their exports wait are bounded: once less than
// minBatchBytes of MaxHeldBytes is left, another log type reads nothing and
// is blocked too, until an export ends.
func TestHeldBatchesBlockOtherReads(t *testing.T) {
	x := newExtraction(t, t.TempDir(), 100)
	x.executor.MaxHeldBytes = 2 * minBatchBytes
	x.executor.MaxBatchBytes = 2 * minBatchBytes
	g := gated(x)
	// Rows of about 32 KiB each (their key, cut to maxScannedValueBytes, is
	// both Message and Request_Key): more than the 2 MiB budget.
	for i := range 100 {
		x.source.insert(strings.Repeat("k", maxValueBytes), at(time.Minute+time.Duration(i)*time.Second))
	}
	if err := x.runFor(50 * time.Millisecond); !errors.Is(err, collectors.ErrBlocked) {
		t.Fatalf("first log type: %v", err)
	}
	if held := x.executor.exports.heldBytes(); held < 2*minBatchBytes {
		t.Fatalf("held %d bytes, want the waiting batch's, over 2 MiB", held)
	}
	general := x.item
	general.Definition.Name = "general"
	err := x.runForItem(general, 50*time.Millisecond)
	if !errors.Is(err, collectors.ErrBlocked) || !strings.Contains(err.Error(), "batches of other log types") {
		t.Fatalf("second log type: %v, want blocked by the held budget", err)
	}
	if len(x.source.params) != 1 {
		t.Fatalf("%d queries, want none past the budget", len(x.source.params))
	}
	if got := testutil.ToFloat64(x.metrics.exportBlocked.WithLabelValues("oslogs", "prod", "logging-db", "general")); got != 1 {
		t.Fatalf("export blocked = %v for the log type waiting for the budget, want 1", got)
	}
	g.release()
	x.settle()
	if held := x.executor.exports.heldBytes(); held != 0 {
		t.Fatalf("held %d bytes after every export ended", held)
	}
	if err := x.runForItem(general, time.Second); err != nil {
		t.Fatalf("second log type after the export: %v", err)
	}
	if got := x.source.params[1].MaxBytes; got != 2*minBatchBytes {
		t.Fatalf("second read allowed %d bytes, want the whole budget back", got)
	}
	if got := testutil.ToFloat64(x.metrics.exportBlocked.WithLabelValues("oslogs", "prod", "logging-db", "general")); got != 0 {
		t.Fatalf("export blocked = %v once the log type reads again, want 0", got)
	}
}

// reserve grants what is left, down to minBatchBytes, and then nothing.
func TestReserveStopsAtTheFloor(t *testing.T) {
	var s exportState
	const max = 10 * minBatchBytes
	if got := s.reserve(8*minBatchBytes, max); got != 8*minBatchBytes {
		t.Fatalf("first grant %d", got)
	}
	if got := s.reserve(8*minBatchBytes, max); got != 2*minBatchBytes {
		t.Fatalf("second grant %d, want what is left", got)
	}
	if got := s.reserve(8*minBatchBytes, max); got != 0 || s.heldBytes() != max {
		t.Fatalf("third grant %d, held %d; want nothing past the budget", got, s.heldBytes())
	}
	s.release(max)
	if got := s.reserve(minBatchBytes/2, max); got != minBatchBytes/2 {
		t.Fatalf("a small batch got %d", got)
	}
}

func (x *extraction) runForItem(item collectormetadata.ScheduledProbe, d time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	_, _, err := x.executor.RunProbe(ctx, item)
	return err
}

// A reload stops a changed collector and starts it again: its exports carry
// on.  A collector that does not come back has its exports cancelled after
// the grace, leaving the watermark.
func TestExportsOutliveAReloadButNotARemoval(t *testing.T) {
	x := newExtraction(t, t.TempDir(), 100)
	x.executor.orphanAfter = 50 * time.Millisecond
	g := gated(x)
	collector := collectorconfig.CollectorRuntimeConfig{ID: "oslogs", Kind: collectorconfig.KindOutSystemsLogs, Enabled: true}
	x.executor.InitCollector(collector)
	x.source.insert("a", at(time.Minute))
	if err := x.runFor(50 * time.Millisecond); !errors.Is(err, collectors.ErrBlocked) {
		t.Fatalf("RunProbe: %v", err)
	}
	// Reload: stopped and started again within the grace.
	x.executor.ForgetCollector("oslogs")
	x.executor.InitCollector(collector)
	time.Sleep(150 * time.Millisecond)
	if err := x.run(); !errors.Is(err, collectors.ErrBlocked) {
		t.Fatalf("after a reload: %v, want the export still waiting", err)
	}
	if got := x.gauge(x.metrics.exportBlocked); got != 1 {
		t.Fatalf("export blocked = %v after a reload, want 1", got)
	}
	// Removal: not started again.
	x.executor.ForgetCollector("oslogs")
	x.settle()
	if _, found := x.savedWatermark(); found {
		t.Fatal("a cancelled export moved the watermark")
	}
	if got := g.keys(); len(got) != 0 {
		t.Fatalf("delivered %v", got)
	}
}

// Close waits for the exports until its context ends, then cancels them;
// a cancelled export leaves the watermark.
func TestCloseCancelsExportsThatDoNotEnd(t *testing.T) {
	x := newExtraction(t, t.TempDir(), 100)
	gated(x)
	x.source.insert("a", at(time.Minute))
	if err := x.runFor(50 * time.Millisecond); !errors.Is(err, collectors.ErrBlocked) {
		t.Fatalf("RunProbe: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	x.executor.Close(ctx)
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("Close took %s", elapsed)
	}
	if _, found := x.savedWatermark(); found {
		t.Fatal("a cancelled export moved the watermark")
	}
}

// delayedExporter accepts every export after delay.
type delayedExporter struct {
	fakeExporter
	delay time.Duration
}

func (d *delayedExporter) Export(ctx context.Context, endpoint string, resourceLogs []*logspb.ResourceLogs) error {
	select {
	case <-time.After(d.delay):
	case <-ctx.Done():
		return ctx.Err()
	}
	return d.fakeExporter.Export(ctx, endpoint, resourceLogs)
}

// The calls of one target wait for exports at most half the poll interval in
// all, so a slow Collector that accepts each batch just within a call cannot
// use up the cycle and leave the target's last log types unstarted.
//
// The wait-budget tests run in a synctest bubble: the exporter's delay, the
// budget's timer and the elapsed times are on its fake clock, so a slow or
// loaded machine (-race on CI) cannot push a wait past the budget.
func TestATargetWaitsAtMostHalfTheInterval(t *testing.T) {
	synctest.Test(t, testATargetWaitsAtMostHalfTheInterval)
}

func testATargetWaitsAtMostHalfTheInterval(t *testing.T) {
	x := newExtraction(t, t.TempDir(), 100)
	// In a cleanup, so a failed check still ends the delayed export before
	// the bubble does; a goroutine left blocked would panic the whole run.
	t.Cleanup(x.settle)
	x.executor.Exporter = &delayedExporter{delay: 700 * time.Millisecond}
	x.item.Assignment.IntervalSeconds = 2
	x.source.insert("a", at(time.Minute))
	if err := x.runFor(5 * time.Second); err != nil {
		t.Fatalf("first log type: %v, want its batch accepted within the budget", err)
	}
	general := x.item
	general.Definition.Name = "general"
	started := time.Now()
	err := x.runForItem(general, 5*time.Second)
	if elapsed := time.Since(started); !errors.Is(err, collectors.ErrBlocked) || elapsed != 300*time.Millisecond {
		t.Fatalf("second log type: %v after %s; want ErrBlocked after the 300 ms left of the target's 1 s budget", err, elapsed)
	}
}

// The wait budget belongs to the target's cycle, which the call of its first
// log type starts, not to a window that drifts across cycles: a cycle never
// gets the rest of an earlier window on top of its own.
func TestTheWaitBudgetFollowsTheCycle(t *testing.T) {
	synctest.Test(t, testTheWaitBudgetFollowsTheCycle)
}

func testTheWaitBudgetFollowsTheCycle(t *testing.T) {
	x := newExtraction(t, t.TempDir(), 100)
	t.Cleanup(x.settle)
	x.executor.Exporter = &delayedExporter{delay: 700 * time.Millisecond}
	x.item.Assignment.IntervalSeconds = 2
	x.executor.InitCollector(collectorconfig.CollectorRuntimeConfig{
		ID: "oslogs", Kind: collectorconfig.KindOutSystemsLogs, Enabled: true,
		Targets: []collectorconfig.TargetRuntimeConfig{{Name: "logging-db", EnvironmentSlug: "prod",
			Probes: []collectorconfig.ProbeRuntimeConfig{{Name: "error"}, {Name: "general"}}}},
	})
	general := x.item
	general.Definition.Name = "general"
	// An earlier cycle: general waits a little, early in a window.
	x.source.insert("a", at(time.Minute))
	if err := x.runForItem(general, 5*time.Second); err != nil {
		t.Fatalf("earlier cycle: %v", err)
	}
	time.Sleep(800 * time.Millisecond)
	// This cycle: error spends most of the budget; general gets the rest.
	x.now = x.now.Add(time.Minute)
	x.source.insert("b", at(9*time.Minute+30*time.Second))
	if err := x.runFor(5 * time.Second); err != nil {
		t.Fatalf("error log type: %v", err)
	}
	started := time.Now()
	err := x.runForItem(general, 5*time.Second)
	if elapsed := time.Since(started); !errors.Is(err, collectors.ErrBlocked) || elapsed != 300*time.Millisecond {
		t.Fatalf("general: %v after %s; want ErrBlocked after the 300 ms left of this cycle's 1 s", err, elapsed)
	}
}

// A blocked export says why the Collector does not take it, and an attempt
// that failed for another reason than waiting is a warning: an unreachable
// or failing Collector is not taken for a busy one.  The lag keeps growing.
func TestABlockedExportReportsItsLastError(t *testing.T) {
	x := newExtraction(t, t.TempDir(), 100)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "queue storage failed", http.StatusInternalServerError)
	}))
	defer server.Close()
	client := otlp.NewClient()
	client.InitialBackoff, client.MaxBackoff = time.Millisecond, 10*time.Millisecond
	x.executor.Exporter = client
	x.item.OutSystemsLogs.LogsEndpoint = server.URL
	x.source.insert("a", at(time.Minute))
	if err := x.runFor(200 * time.Millisecond); !errors.Is(err, collectors.ErrBlocked) {
		t.Fatalf("first call: %v", err)
	}
	x.now = x.now.Add(10 * time.Minute)
	err := x.run()
	if !errors.Is(err, collectors.ErrBlocked) || !strings.Contains(err.Error(), "the last: receiver answered 500 Internal Server Error") {
		t.Fatalf("RunProbe error = %v, want the last attempt's error", err)
	}
	if !strings.Contains(x.logs.String(), "export failed; retrying until the OTel Collector accepts it") || strings.Count(x.logs.String(), "export failed; retrying") != 1 {
		t.Fatalf("want one warning for the failing attempts:\n%s", x.logs)
	}
	if got := x.gauge(x.metrics.lag); got != (25 * time.Minute).Seconds() {
		t.Fatalf("extract lag = %vs while blocked, want 1500 (now minus the initial watermark)", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	x.executor.Close(ctx)
}

// While a batch's export waits, the held budget counts the built records and
// their body as well as the row data, and the export releases all of it.
func TestHeldBytesCountTheBuiltRecords(t *testing.T) {
	x := newExtraction(t, t.TempDir(), 100)
	g := gated(x)
	for i := range 10 {
		x.source.insert(strings.Repeat("k", 4<<10), at(time.Minute+time.Duration(i)*time.Second))
	}
	if err := x.runFor(50 * time.Millisecond); !errors.Is(err, collectors.ErrBlocked) {
		t.Fatalf("first call: %v, want blocked on the export", err)
	}
	// Each row holds about 8 KiB of text; its record encodes both values
	// again, counted twice.
	const rowData = 10 * 8 << 10
	if held := x.executor.exports.heldBytes(); held < 3*rowData {
		t.Fatalf("held %d bytes, want the rows and their records, over %d", held, 3*rowData)
	}
	g.release()
	x.settle()
	if held := x.executor.exports.heldBytes(); held != 0 {
		t.Fatalf("held %d bytes after the export ended", held)
	}
}
