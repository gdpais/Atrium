package outsystems

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	collectorconfig "heartbeat/internal/config"
	"heartbeat/internal/otlp"
	collectormetadata "heartbeat/services/db-collector/internal/metadata"
)

// t0 is the base time of the extraction tests.
var t0 = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

// fakeRow is a row of the fake log table.
type fakeRow struct {
	instant time.Time
	key     string
}

// fakeSource is a log table that answers queries as the contract asks:
// Instant in (From, To], ascending, at most Limit rows.  Its knobs break the
// contract on purpose.
type fakeSource struct {
	mu     sync.Mutex
	rows   []fakeRow
	params []QueryParams
	err    error
	// instantType is the Instant column's database type (default DATETIME2).
	instantType string
	// ignoreLimit, ignoreFrom and reverse break the contract.
	ignoreLimit, ignoreFrom, reverse bool
	// noInstant names the time column Created instead.
	noInstant bool
}

func (s *fakeSource) insert(key string, instant time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, fakeRow{instant: instant, key: key})
}

func (s *fakeSource) Query(_ context.Context, _ collectormetadata.DatabaseTarget, query string, params QueryParams) (Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.params = append(s.params, params)
	if s.err != nil {
		return Batch{}, s.err
	}
	instantType := s.instantType
	if instantType == "" {
		instantType = "DATETIME2"
	}
	instantColumn := "Instant"
	if s.noInstant {
		instantColumn = "Created"
	}
	batch := Batch{Columns: []Column{{instantColumn, instantType}, {"Message", "NVARCHAR"}, {"Request_Key", "NVARCHAR"}, {"Application_Name", "NVARCHAR"}}}
	var rows []fakeRow
	for _, row := range s.rows {
		if (s.ignoreFrom || row.instant.After(params.From)) && !row.instant.After(params.To) {
			rows = append(rows, row)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].instant.Before(rows[j].instant) })
	if !s.ignoreLimit && len(rows) > params.Limit {
		rows = rows[:params.Limit]
	}
	if s.reverse {
		slices.Reverse(rows)
	}
	values := make([][]any, len(rows))
	for i, row := range rows {
		values[i] = []any{row.instant, "error " + row.key, row.key, "Shop"}
	}
	// Read as SQLServerSource reads, so MaxBytes applies.
	if err := readRows(&fakeRows{n: len(values), row: func(i int) []any { return values[i] }}, &batch, params); err != nil {
		return Batch{}, err
	}
	return batch, nil
}

// fakeExporter records delivered Request_Keys, or fails.
type fakeExporter struct {
	mu        sync.Mutex
	delivered []string
	endpoints []string
	err       error
}

func (e *fakeExporter) Export(_ context.Context, endpoint string, resourceLogs []*logspb.ResourceLogs) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return e.err
	}
	e.endpoints = append(e.endpoints, endpoint)
	for _, rl := range resourceLogs {
		for _, sl := range rl.ScopeLogs {
			for _, record := range sl.LogRecords {
				e.delivered = append(e.delivered, attributes(record.Attributes)["Request_Key"].(string))
			}
		}
	}
	return nil
}

func (e *fakeExporter) keys() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.delivered)
}

// extraction is one executor under test with a fake clock.
type extraction struct {
	t        *testing.T
	source   *fakeSource
	exporter *fakeExporter
	executor *Executor
	metrics  *Metrics
	logs     *bytes.Buffer
	now      time.Time
	item     collectormetadata.ScheduledProbe
}

func newExtraction(t *testing.T, dir string, limit int) *extraction {
	t.Helper()
	metrics, err := NewMetrics(prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	x := &extraction{t: t, source: &fakeSource{}, exporter: &fakeExporter{}, metrics: metrics, logs: &bytes.Buffer{}, now: t0.Add(10 * time.Minute)}
	x.executor = NewExecutor(x.source, NewStore(dir), x.exporter, metrics, slog.New(slog.NewTextHandler(x.logs, nil)))
	x.executor.now = func() time.Time { return x.now }
	x.item = collectormetadata.ScheduledProbe{
		CollectorID: "oslogs",
		Target:      collectormetadata.DatabaseTarget{Name: "logging-db", EnvironmentSlug: "prod", Engine: collectorconfig.KindOutSystemsLogs, Host: "db", Port: 1433, DatabaseName: "OSLogging"},
		Definition:  collectormetadata.ProbeDefinition{Name: "error"},
		OutSystemsLogs: collectorconfig.OutSystemsLogsConfig{
			Lag: time.Minute, BatchLimit: limit, InitialLookback: 15 * time.Minute, LogTimezone: "UTC", LogsEndpoint: "http://otel:4318",
		},
	}
	return x
}

func (x *extraction) run() error {
	x.t.Helper()
	_, _, err := x.executor.RunProbe(context.Background(), x.item)
	return err
}

func (x *extraction) mustRun() {
	x.t.Helper()
	if err := x.run(); err != nil {
		x.t.Fatalf("RunProbe: %v", err)
	}
}

func (x *extraction) watermark() Watermark {
	x.t.Helper()
	watermark, found, err := x.executor.Store.Load(WatermarkKey{Collector: "oslogs", Target: "logging-db", LogType: "error"})
	if err != nil || !found {
		x.t.Fatalf("no watermark: %v", err)
	}
	return watermark
}

func (x *extraction) counter(vec *prometheus.CounterVec) float64 {
	return testutil.ToFloat64(vec.WithLabelValues("oslogs", "prod", "logging-db", "error"))
}

func (x *extraction) gauge(vec *prometheus.GaugeVec) float64 {
	return testutil.ToFloat64(vec.WithLabelValues("oslogs", "prod", "logging-db", "error"))
}

// at returns t0 plus d.
func at(d time.Duration) time.Time { return t0.Add(d) }

func TestExtractionStartsAtTheInitialLookbackAndStopsAtTheLag(t *testing.T) {
	x := newExtraction(t, t.TempDir(), 100)
	x.source.insert("too-old", at(-6*time.Minute))
	x.source.insert("a", at(-4*time.Minute))
	x.source.insert("b", at(9*time.Minute))
	x.source.insert("too-new", at(9*time.Minute+30*time.Second))
	x.mustRun()

	params := x.source.params[0]
	if !params.From.Equal(at(-5*time.Minute)) || !params.To.Equal(at(9*time.Minute)) || params.Limit != 100 || params.DateTime {
		t.Fatalf("query params %+v; want (now-15m, now-1m], limit 100, datetime2", params)
	}
	if got := x.exporter.keys(); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("delivered %v, want a and b", got)
	}
	if x.exporter.endpoints[0] != "http://otel:4318" {
		t.Fatalf("endpoint %s", x.exporter.endpoints[0])
	}
	// A batch that was not full read everything up to now - lag.
	if got := x.watermark(); !got.Instant.Equal(at(9*time.Minute)) || got.Source != "db:1433/OSLogging" || got.InstantType != "DATETIME2" {
		t.Fatalf("watermark %+v", got)
	}
	if !strings.Contains(x.logs.String(), "has no watermark for this log type; starting from initial_lookback") {
		t.Fatalf("no warning for the missing watermark:\n%s", x.logs)
	}
	if got := x.counter(x.metrics.rows); got != 2 {
		t.Fatalf("rows extracted = %v, want 2", got)
	}
	if got := x.gauge(x.metrics.lag); got != 60 {
		t.Fatalf("extract lag = %vs, want 60", got)
	}
	if got := x.gauge(x.metrics.logDelay); got != 60 {
		t.Fatalf("log delay = %vs, want 60 (newest row at now-1m)", got)
	}

	// The row committed late, within the lag, is read by the next cycle.
	x.now = x.now.Add(time.Minute)
	x.mustRun()
	if got := x.exporter.keys(); !slices.Equal(got, []string{"a", "b", "too-new"}) {
		t.Fatalf("delivered %v", got)
	}
}

// A log type with its own lag (probe lag) reads up to now minus that lag,
// not the collector's.
func TestExtractionUsesTheLogTypeLag(t *testing.T) {
	x := newExtraction(t, t.TempDir(), 100)
	x.item.Definition.Lag = 5 * time.Minute
	x.source.insert("done", at(4*time.Minute))
	x.source.insert("maybe-running", at(6*time.Minute)) // inside the collector's lag, not this log type's
	x.mustRun()

	if params := x.source.params[0]; !params.To.Equal(at(5 * time.Minute)) {
		t.Fatalf("read up to %s, want now-5m (the log type's lag)", params.To)
	}
	if got := x.exporter.keys(); !slices.Equal(got, []string{"done"}) {
		t.Fatalf("delivered %v, want done only", got)
	}
	if got := x.gauge(x.metrics.lag); got != 300 {
		t.Fatalf("extract lag = %vs, want 300", got)
	}
}

// A full batch leaves the rows of its last Instant for the next batch, which
// the same call reads at once.  Every row arrives exactly once.
func TestExtractionSplitsFullBatchesBetweenInstants(t *testing.T) {
	x := newExtraction(t, t.TempDir(), 3)
	for _, row := range []struct {
		key    string
		minute int
	}{{"a", 1}, {"b", 2}, {"c", 2}, {"d", 3}, {"e", 3}, {"f", 4}, {"g", 5}} {
		x.source.insert(row.key, at(time.Duration(row.minute)*time.Minute))
	}
	x.mustRun()
	// [a b c] -> a; [b c d] -> b c; [d e f] -> d e; [f g] -> f g.
	if got := x.exporter.keys(); !slices.Equal(got, []string{"a", "b", "c", "d", "e", "f", "g"}) {
		t.Fatalf("delivered %v, want every row once, in order", got)
	}
	var froms []time.Time
	for _, params := range x.source.params {
		froms = append(froms, params.From)
	}
	if want := []time.Time{at(-5 * time.Minute), at(time.Minute), at(2 * time.Minute), at(3 * time.Minute)}; !slices.Equal(froms, want) {
		t.Fatalf("batches read from %v, want %v", froms, want)
	}
	if got := x.watermark().Instant; !got.Equal(at(9 * time.Minute)) {
		t.Fatalf("watermark %s, want now - lag after the last, partial batch", got)
	}
	x.now = x.now.Add(time.Minute)
	x.mustRun()
	if got := x.exporter.keys(); len(got) != 7 {
		t.Fatalf("next cycle delivered again: %v", got)
	}
}

// When every row of a full batch has the same Instant, no watermark can
// separate them: the cycle fails, counts a stuck batch and sends nothing,
// until batch_limit is raised.
func TestExtractionFailsAStuckBatch(t *testing.T) {
	x := newExtraction(t, t.TempDir(), 2)
	x.source.insert("a", at(time.Minute))
	x.source.insert("b", at(time.Minute))
	x.source.insert("c", at(time.Minute))
	err := x.run()
	if !errors.Is(err, ErrBatchStuck) || !strings.Contains(err.Error(), "raise batch_limit above 2") {
		t.Fatalf("RunProbe error = %v, want a stuck batch", err)
	}
	if got := x.counter(x.metrics.stuck); got != 1 {
		t.Fatalf("stuck batches = %v, want 1", got)
	}
	if got := x.exporter.keys(); len(got) != 0 {
		t.Fatalf("delivered %v from a stuck batch", got)
	}
	if !x.watermark().Instant.Equal(at(-5 * time.Minute)) {
		t.Fatalf("watermark moved to %s", x.watermark().Instant)
	}
	x.item.OutSystemsLogs.BatchLimit = 4
	x.mustRun()
	if got := x.exporter.keys(); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("delivered %v after raising batch_limit", got)
	}
}

// A backlog is read a bounded number of batches per call.
func TestExtractionBoundsCatchUpBatches(t *testing.T) {
	x := newExtraction(t, t.TempDir(), 2)
	x.executor.MaxBatches = 2
	for i, key := range []string{"a", "b", "c", "d", "e", "f"} {
		x.source.insert(key, at(time.Duration(i+1)*time.Minute))
	}
	x.mustRun()
	// Each full batch of two distinct Instants delivers its first row.
	if got := x.exporter.keys(); !slices.Equal(got, []string{"a", "b"}) || len(x.source.params) != 2 {
		t.Fatalf("delivered %v in %d batches, want a and b in 2", got, len(x.source.params))
	}
	x.mustRun()
	if got := x.exporter.keys(); !slices.Equal(got, []string{"a", "b", "c", "d"}) {
		t.Fatalf("delivered %v", got)
	}
}

// A failed export leaves the watermark, so the next cycle sends the rows
// again: at least once.
func TestExtractionKeepsTheWatermarkWhenTheExportFails(t *testing.T) {
	dir := t.TempDir()
	x := newExtraction(t, dir, 100)
	x.source.insert("a", at(time.Minute))
	x.exporter.err = errors.New("collector unavailable")
	if err := x.run(); err == nil || !strings.Contains(err.Error(), "collector unavailable") {
		t.Fatalf("RunProbe error = %v", err)
	}
	if got := x.counter(x.metrics.exportFailures); got != 1 {
		t.Fatalf("export failures = %v, want 1", got)
	}
	if got := x.counter(x.metrics.rows); got != 0 {
		t.Fatalf("rows extracted = %v, want 0", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("a watermark was saved after a failed export: %v", entries)
	}
	x.exporter.err = nil
	x.mustRun()
	if got := x.exporter.keys(); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("delivered %v after recovery", got)
	}
}

func TestExtractionReportsQueryErrors(t *testing.T) {
	x := newExtraction(t, t.TempDir(), 100)
	x.source.err = errors.New("login failed")
	if err := x.run(); err == nil || !strings.Contains(err.Error(), "log type error: login failed") {
		t.Fatalf("RunProbe error = %v", err)
	}
	if got := x.counter(x.metrics.exportFailures); got != 0 {
		t.Fatalf("a query error counted as an export failure")
	}
}

func TestExtractionRejectsContractViolations(t *testing.T) {
	tests := []struct {
		name    string
		breaks  func(*fakeSource)
		wantErr string
	}{
		{"no Instant column", func(s *fakeSource) { s.noInstant = true }, "no column named exactly Instant"},
		{"more rows than @limit", func(s *fakeSource) { s.ignoreLimit = true }, "more than @limit (2) rows"},
		{"descending order", func(s *fakeSource) { s.reverse = true }, "out of order"},
		{"rows before @from", func(s *fakeSource) { s.ignoreFrom = true }, "outside (@from, @to]"},
		{"Instant is text", func(s *fakeSource) { s.instantType = "NVARCHAR" }, "Instant is NVARCHAR, want datetime or datetime2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			x := newExtraction(t, t.TempDir(), 2)
			x.source.insert("before", at(-10*time.Minute))
			x.source.insert("a", at(time.Minute))
			x.source.insert("b", at(2*time.Minute))
			x.source.insert("c", at(3*time.Minute))
			tt.breaks(x.source)
			err := x.run()
			if !errors.Is(err, ErrContract) || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("RunProbe error = %v, want %q", err, tt.wantErr)
			}
			if got := x.exporter.keys(); len(got) != 0 {
				t.Fatalf("delivered %v from a batch that breaks the contract", got)
			}
		})
	}
}

// A restart resumes from the watermark file: nothing is sent twice and
// nothing is skipped.
func TestExtractionResumesAfterARestart(t *testing.T) {
	dir := t.TempDir()
	first := newExtraction(t, dir, 100)
	first.source.insert("a", at(time.Minute))
	first.mustRun()

	second := newExtraction(t, dir, 100)
	second.source = first.source
	second.executor.Source = first.source
	second.now = first.now.Add(5 * time.Minute)
	first.source.insert("b", at(12*time.Minute))
	second.mustRun()
	if got := second.exporter.keys(); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("after the restart delivered %v, want only b", got)
	}
	if strings.Contains(second.logs.String(), "initial_lookback") {
		t.Fatalf("the restart did not use the watermark file:\n%s", second.logs)
	}
}

// A watermark belongs to the database it was read from; a corrupt file is
// ignored.  Both start over from initial_lookback with a warning.
func TestExtractionStartsOverWithoutAUsableWatermark(t *testing.T) {
	for _, tt := range []struct {
		name    string
		prepare func(x *extraction, dir string)
		warning string
	}{
		{"another database", func(x *extraction, _ string) {
			x.item.Target.DatabaseName = "OSLoggingNew"
		}, "watermark belongs to another database"},
		{"corrupt file", func(_ *extraction, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "oslogs.logging-db.error.json"), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "watermark file is unreadable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			first := newExtraction(t, dir, 100)
			first.mustRun()
			x := newExtraction(t, dir, 100)
			x.now = first.now.Add(time.Hour)
			tt.prepare(x, dir)
			x.mustRun()
			if !strings.Contains(x.logs.String(), tt.warning) {
				t.Fatalf("no %q warning:\n%s", tt.warning, x.logs)
			}
			if got := x.source.params[0].From; !got.Equal(x.now.Add(-15 * time.Minute)) {
				t.Fatalf("read from %s, want now - initial_lookback", got)
			}
		})
	}
}

// Once the Instant column is known to be datetime, the bounds are sent as
// datetime so they compare with the column's 1/300 s ticks exactly.
func TestExtractionSendsDatetimeBoundsForADatetimeInstant(t *testing.T) {
	x := newExtraction(t, t.TempDir(), 100)
	x.source.instantType = "DATETIME"
	x.mustRun()
	x.now = x.now.Add(time.Minute)
	x.mustRun()
	if x.source.params[0].DateTime || !x.source.params[1].DateTime {
		t.Fatalf("DateTime flags %v, %v; want false before the type is known, true after", x.source.params[0].DateTime, x.source.params[1].DateTime)
	}
	if got := x.watermark().InstantType; got != "DATETIME" {
		t.Fatalf("watermark instant type %q", got)
	}
}

// Bounds are wall clocks in log_timezone; the lag metric is real time.
func TestExtractionReadsInTheLogTimezone(t *testing.T) {
	x := newExtraction(t, t.TempDir(), 100)
	x.item.OutSystemsLogs.LogTimezone = "America/New_York"
	x.mustRun()
	params := x.source.params[0]
	// 12:10 UTC is 08:10 in New York (EDT); lag 1m, lookback 15m.
	want := func(h, m int) time.Time { return time.Date(2026, 10, 10, h, m, 0, 0, time.UTC) }
	if !params.From.Equal(want(7, 55)) || !params.To.Equal(want(8, 9)) {
		t.Fatalf("bounds (%s, %s], want (07:55, 08:09] New York time", params.From, params.To)
	}
	if got := x.gauge(x.metrics.lag); got != 60 {
		t.Fatalf("extract lag = %vs, want 60", got)
	}
}

// Delivered rows are not sent again when only the watermark file failed.
func TestExtractionKeepsGoingWhenTheWatermarkFileCannotBeWritten(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	x := newExtraction(t, filepath.Join(blocker, "dir"), 100)
	x.source.insert("a", at(time.Minute))
	if err := x.run(); err == nil || !strings.Contains(err.Error(), "rows delivered, but the watermark is only kept in memory") {
		t.Fatalf("RunProbe error = %v", err)
	}
	x.now = x.now.Add(time.Minute)
	_ = x.run()
	if got := x.exporter.keys(); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("delivered %v, want a once", got)
	}
}

func TestExtractionUsesTheQueryTemplateOverride(t *testing.T) {
	x := newExtraction(t, t.TempDir(), 100)
	recorder := &queryRecorder{Source: x.source}
	x.executor.Source = recorder
	x.mustRun()
	x.item.Definition.QueryTemplate = "SELECT TOP (@limit) Instant FROM dbo.custom WHERE Instant > @from AND Instant <= @to ORDER BY Instant"
	x.now = x.now.Add(time.Minute)
	x.mustRun()
	builtin, _ := x.executor.Catalog.Get("error")
	if recorder.queries[0] != builtin.SQL || recorder.queries[1] != x.item.Definition.QueryTemplate {
		t.Fatalf("queries %q", recorder.queries)
	}
	x.item.Definition.Name = "screen_typo"
	if err := x.run(); err == nil || !strings.Contains(err.Error(), `unknown OutSystems log type "screen_typo"`) {
		t.Fatalf("RunProbe error = %v", err)
	}
}

// queryRecorder records the query text a Source receives.
type queryRecorder struct {
	Source
	queries []string
}

func (r *queryRecorder) Query(ctx context.Context, target collectormetadata.DatabaseTarget, query string, params QueryParams) (Batch, error) {
	r.queries = append(r.queries, query)
	return r.Source.Query(ctx, target, query, params)
}

func TestMetricsFollowTheCollectorLifecycle(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics, err := NewMetrics(registry)
	if err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(&fakeSource{}, NewStore(t.TempDir()), &fakeExporter{}, metrics, nil)
	collector := collectorconfig.CollectorRuntimeConfig{
		ID: "oslogs", Kind: collectorconfig.KindOutSystemsLogs, Enabled: true, TargetNames: []string{"logging-db"},
		Targets: []collectorconfig.TargetRuntimeConfig{
			{Name: "logging-db", EnvironmentSlug: "prod", Probes: []collectorconfig.ProbeRuntimeConfig{{Name: "error"}, {Name: "general"}}},
			{Name: "unselected", EnvironmentSlug: "prod", Probes: []collectorconfig.ProbeRuntimeConfig{{Name: "error"}}},
		},
	}
	executor.InitCollector(collector)
	// Seven counters (rows, export failures, stuck batches, late rows,
	// duplicate rows, recounts, recount failures) and the export-blocked
	// gauge for each of the two selected log types, at 0; the collector's
	// derived series count, and its overflow counter for each of the nine
	// derived metrics.
	if got := testutil.CollectAndCount(registry); got != 16+1+9 {
		t.Fatalf("%d series after init, want 26", got)
	}
	executor.ForgetCollector("oslogs")
	if got := testutil.CollectAndCount(registry); got != 0 {
		t.Fatalf("%d series after forget, want 0", got)
	}
}

// The rows reach an OTLP/HTTP receiver as protobuf, after a transient
// failure is retried.
func TestExtractionDeliversOverOTLPHTTP(t *testing.T) {
	var mu sync.Mutex
	var received []*logspb.LogsData
	failures := 1
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if failures > 0 {
			failures--
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("gzip: %v", err)
			return
		}
		raw, _ := io.ReadAll(zr)
		var data logspb.LogsData
		if err := proto.Unmarshal(raw, &data); err != nil {
			t.Errorf("decode: %v", err)
		}
		received = append(received, &data)
	}))
	defer server.Close()

	x := newExtraction(t, t.TempDir(), 100)
	client := otlp.NewClient()
	client.InitialBackoff = time.Millisecond
	x.executor.Exporter = client
	x.item.OutSystemsLogs.LogsEndpoint = server.URL
	x.source.insert("a", at(time.Minute))
	x.source.insert("b", at(2*time.Minute))
	x.mustRun()

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 || len(received[0].ResourceLogs) != 1 {
		t.Fatalf("received %v", received)
	}
	rl := received[0].ResourceLogs[0]
	if service := attributes(rl.Resource.Attributes)["service.name"]; service != "Shop" {
		t.Fatalf("service.name = %v", service)
	}
	records := rl.ScopeLogs[0].LogRecords
	if len(records) != 2 || records[0].Body.GetStringValue() != "error a" || records[0].TimeUnixNano != uint64(at(time.Minute).UnixNano()) {
		t.Fatalf("records %v", records)
	}
}
