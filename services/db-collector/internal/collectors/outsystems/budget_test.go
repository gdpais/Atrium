package outsystems

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"

	collectorconfig "heartbeat/internal/config"
	"heartbeat/internal/otlp"
	collectormetadata "heartbeat/services/db-collector/internal/metadata"
	catalogoutsystems "heartbeat/services/db-collector/internal/probes/outsystems"
)

// fakeRows is a rowReader of n rows that row makes on demand, as the driver
// does when it scans.
type fakeRows struct {
	n    int
	row  func(i int) []any
	next int
	read int
	err  error
}

func (r *fakeRows) Next() bool {
	if r.next >= r.n {
		return false
	}
	r.next++
	return true
}

func (r *fakeRows) Scan(dest ...any) error {
	r.read++
	for i, v := range r.row(r.next - 1) {
		*dest[i].(*any) = v
	}
	return nil
}

func (r *fakeRows) Err() error { return r.err }

// instantRows returns rows of (Instant, Text) with the given Instant offsets
// in seconds after t0 and texts of size bytes.
func instantRows(size int, seconds ...int) *fakeRows {
	return &fakeRows{n: len(seconds), row: func(i int) []any {
		return []any{at(time.Duration(seconds[i]) * time.Second), strings.Repeat("x", size)}
	}}
}

var instantColumns = []Column{{"Instant", "DATETIME2"}, {"Text", "NVARCHAR"}}

func TestReadRowsStopsAtTheByteBudgetWhereInstantChanges(t *testing.T) {
	// Each row holds 1000 bytes of text plus 64 of overhead: the budget is
	// spent after the second row.
	for _, tt := range []struct {
		name    string
		seconds []int
		limit   int
		budget  int64
		want    []int
		partial bool
	}{
		{"no budget", []int{1, 2, 3, 4}, 10, 0, []int{1, 2, 3, 4}, false},
		{"under budget", []int{1, 2, 3, 4}, 10, 1 << 20, []int{1, 2, 3, 4}, false},
		// The third row starts a new Instant: kept as the row the caller drops.
		{"stops at a new Instant", []int{1, 2, 3, 4}, 10, 2000, []int{1, 2, 3}, true},
		// Rows that share the last Instant are read past the budget.
		{"never splits an Instant", []int{1, 2, 2, 2, 3, 4}, 10, 2000, []int{1, 2, 2, 2, 3}, true},
		{"all rows at one Instant", []int{1, 1, 1, 1}, 10, 100, []int{1, 1, 1, 1}, false},
		// At @limit rows the batch is full; the next row only detects a query
		// that ignores @limit.
		{"full before partial", []int{1, 2, 3, 4}, 2, 2000, []int{1, 2, 3}, false},
		{"budget spent by the last row", []int{1, 2}, 10, 2000, []int{1, 2}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rows := instantRows(1000, tt.seconds...)
			batch := Batch{Columns: instantColumns}
			if err := readRows(rows, &batch, QueryParams{Limit: tt.limit, MaxBytes: tt.budget}); err != nil {
				t.Fatal(err)
			}
			var got []int
			for _, row := range batch.Rows {
				got = append(got, int(row[0].(time.Time).Sub(t0)/time.Second))
			}
			if !slices.Equal(got, tt.want) || batch.Partial != tt.partial {
				t.Fatalf("read %v partial=%v, want %v partial=%v", got, batch.Partial, tt.want, tt.partial)
			}
			if rows.read != len(got) {
				t.Fatalf("scanned %d rows, kept %d", rows.read, len(got))
			}
		})
	}
}

func TestReadRowsWithoutAnInstantColumnIgnoresTheBudget(t *testing.T) {
	batch := Batch{Columns: []Column{{"Created", "DATETIME2"}, {"Text", "NVARCHAR"}}}
	if err := readRows(instantRows(1000, 1, 2, 3), &batch, QueryParams{Limit: 10, MaxBytes: 1}); err != nil {
		t.Fatal(err)
	}
	if len(batch.Rows) != 3 || batch.Partial {
		t.Fatalf("read %d rows partial=%v; the contract check rejects this batch, not the budget", len(batch.Rows), batch.Partial)
	}
}

func TestReadRowsReportsIterationErrors(t *testing.T) {
	rows := instantRows(10, 1)
	rows.err = errors.New("connection reset")
	batch := Batch{Columns: instantColumns}
	if err := readRows(rows, &batch, QueryParams{Limit: 10}); err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("err = %v", err)
	}
}

func TestRowBytesCountsTextAndOverhead(t *testing.T) {
	if got := rowBytes([]any{"abc", []byte{1, 2}, int64(7), nil, at(0)}); got != 5*32+5 {
		t.Fatalf("rowBytes = %d, want %d", got, 5*32+5)
	}
}

func TestPartialBatchesAreHandledLikeFullOnes(t *testing.T) {
	x := newExtraction(t, t.TempDir(), 100)
	// A fake row holds about 140 bytes: one Instant per batch.
	x.executor.MaxBatchBytes = 150
	var want []string
	for i, second := range []int{1, 2, 2, 2, 3, 4, 5, 5, 6} {
		key := fmt.Sprintf("r%d", i)
		x.source.insert(key, at(time.Duration(second)*time.Second))
		want = append(want, key)
	}
	x.mustRun()
	// Every row once, in order; the rows sharing second 2 in one batch.
	if got := x.exporter.keys(); !slices.Equal(got, want) {
		t.Fatalf("delivered %v, want %v", got, want)
	}
	if len(x.source.params) < 4 {
		t.Fatalf("%d queries; the budget should have split the rows into several batches", len(x.source.params))
	}
	for _, params := range x.source.params {
		if params.MaxBytes != 150 {
			t.Fatalf("MaxBytes = %d, want 150", params.MaxBytes)
		}
	}
	if got := x.watermark(); !got.Instant.Equal(at(9 * time.Minute)) {
		t.Fatalf("watermark %s, want now - lag once the backlog is read", got.Instant)
	}
	if got := x.counter(x.metrics.stuck); got != 0 {
		t.Fatalf("stuck batches = %v", got)
	}
}

func TestDefaultBatchBudgetApplies(t *testing.T) {
	x := newExtraction(t, t.TempDir(), 100)
	x.source.insert("a", at(time.Second))
	x.mustRun()
	if got := x.source.params[0].MaxBytes; got != DefaultMaxBatchBytes {
		t.Fatalf("MaxBytes = %d, want %d", got, DefaultMaxBatchBytes)
	}
}

// errorBacklog is an error log of n rows, 10 ms apart from t0 − 10m, with
// the columns of queries/error.sql, a Stack of about stack bytes and 2 KB of
// EnvironmentInformation.  It reads them through readRows, making each row's
// values as it is scanned, like SQLServerSource.
type errorBacklog struct{ n, stack int }

func (b errorBacklog) instant(i int) time.Time {
	return t0.Add(-10*time.Minute + time.Duration(i)*10*time.Millisecond)
}

func (b errorBacklog) Query(_ context.Context, _ collectormetadata.DatabaseTarget, _ string, params QueryParams) (Batch, error) {
	batch := Batch{Columns: []Column{
		{"Instant", "DATETIME2"}, {"Message", "NVARCHAR"}, {"Severity", "VARCHAR"}, {"Application_Name", "NVARCHAR"},
		{"Espace_Name", "NVARCHAR"}, {"Module_Name", "NVARCHAR"}, {"Screen", "NVARCHAR"}, {"Action_Name", "NVARCHAR"},
		{"Request_Key", "NVARCHAR"}, {"Session_Id", "NVARCHAR"}, {"User_Id", "INT"}, {"Username", "NVARCHAR"},
		{"Tenant_Id", "INT"}, {"Server", "NVARCHAR"}, {"Stack", "NVARCHAR"}, {"EnvironmentInformation", "NVARCHAR"},
	}}
	first := 0
	for first < b.n && !b.instant(first).After(params.From) {
		first++
	}
	last := first
	for last < b.n && !b.instant(last).After(params.To) {
		last++
	}
	rows := &fakeRows{n: last - first, row: func(i int) []any {
		i += first
		return []any{
			b.instant(i), fmt.Sprintf("Timeout calling GetOrders for order %d", i), "ERROR", "Shop",
			"ShopCore", "ShopCore", "Checkout", "GetOrders", fmt.Sprintf("req-%06d", i), fmt.Sprintf("session-%06d", i),
			int64(i), "jdoe", int64(1), "fe-01",
			strings.Repeat("   at OutSystems.HubEdition.Frame(Context ctx)\n", b.stack/48),
			strings.Repeat("eSpaceVer: 42 (Id=7, PubId=9, CompiledWith=11.0) ", 2<<10/50),
		}
	}}
	err := readRows(rows, &batch, params)
	return batch, err
}

// heapExporter measures the live heap while an export holds the batch: after
// building each request body, as otlp.Client does.
type heapExporter struct {
	mu       sync.Mutex
	baseline uint64
	peak     uint64
	records  int
}

func (e *heapExporter) Export(_ context.Context, _ string, resourceLogs []*logspb.ResourceLogs) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, request := range otlp.Split(resourceLogs, otlp.DefaultMaxRequestBytes) {
		body, err := proto.Marshal(&logspb.LogsData{ResourceLogs: request})
		if err != nil {
			return err
		}
		runtime.GC()
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		e.peak = max(e.peak, stats.HeapAlloc-min(stats.HeapAlloc, e.baseline))
		runtime.KeepAlive(body)
	}
	for _, rl := range resourceLogs {
		for _, sl := range rl.ScopeLogs {
			e.records += len(sl.LogRecords)
		}
	}
	return nil
}

// extractBacklog extracts backlog with budget as MaxBatchBytes and returns
// the peak live heap during an export and the records delivered.
func extractBacklog(t *testing.T, backlog errorBacklog, budget int64) (peak uint64, records int) {
	t.Helper()
	exporter := &heapExporter{}
	executor := &Executor{
		Source: backlog, Catalog: catalogoutsystems.DefaultCatalog(), Store: NewStore(t.TempDir()),
		Exporter: exporter, MaxBatchBytes: budget, MaxBatches: 100, now: func() time.Time { return t0 },
	}
	item := collectormetadata.ScheduledProbe{
		CollectorID: "oslogs",
		Target:      collectormetadata.DatabaseTarget{Name: "logging-db", Engine: collectorconfig.KindOutSystemsLogs, Host: "db", Port: 1433, DatabaseName: "OSLogging"},
		Definition:  collectormetadata.ProbeDefinition{Name: "error"},
		OutSystemsLogs: collectorconfig.OutSystemsLogsConfig{
			Lag: time.Minute, BatchLimit: collectorconfig.DefaultLogBatchLimit, InitialLookback: 15 * time.Minute, LogTimezone: "UTC", LogsEndpoint: "http://otel:4318",
		},
	}
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	exporter.baseline = stats.HeapAlloc
	if _, _, err := executor.RunProbe(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	return exporter.peak, exporter.records
}

// A 5,000-row error batch (the default batch_limit) with 8 KB stacks and 2 KB
// of EnvironmentInformation is about 50 MiB of row data.  Eight targets
// extracting one at once would exceed the chart's 256Mi limit; the byte
// budget keeps each export well under heapBudget.
func TestErrorBacklogStaysWithinTheHeapBudget(t *testing.T) {
	const heapBudget = 24 << 20
	backlog := errorBacklog{n: 5000, stack: 8 << 10}
	peak, records := extractBacklog(t, backlog, 0)
	t.Logf("peak live heap with the default %d MiB budget: %.1f MiB", DefaultMaxBatchBytes>>20, float64(peak)/(1<<20))
	if records != 5000 {
		t.Fatalf("delivered %d records, want all 5000", records)
	}
	if peak > heapBudget {
		t.Fatalf("peak live heap %.1f MiB, want at most %d MiB", float64(peak)/(1<<20), heapBudget>>20)
	}
	// Without the budget the same backlog is one batch: the test must be
	// able to tell.
	unbounded, records := extractBacklog(t, backlog, 1<<40)
	t.Logf("peak live heap without a budget: %.1f MiB", float64(unbounded)/(1<<20))
	if records != 5000 || unbounded <= heapBudget {
		t.Fatalf("without a budget: %d records, peak %.1f MiB; want 5000 and more than %d MiB", records, float64(unbounded)/(1<<20), heapBudget>>20)
	}
}

// oslog_Error returns oslog_Error_Detail's FullStackTrace, a varchar(max),
// as Stack.  Rows with 128 KB stacks are held whole until their records are
// built, so the byte budget stops each batch after fewer of them, and the
// records keep maxValueBytes of each.
func TestVarcharMaxStacksStayWithinTheHeapBudget(t *testing.T) {
	const heapBudget = 24 << 20
	peak, records := extractBacklog(t, errorBacklog{n: 500, stack: 128 << 10}, 0)
	t.Logf("peak live heap with 128 KB stacks: %.1f MiB", float64(peak)/(1<<20))
	if records != 500 {
		t.Fatalf("delivered %d records, want all 500", records)
	}
	if peak > heapBudget {
		t.Fatalf("peak live heap %.1f MiB, want at most %d MiB", float64(peak)/(1<<20), heapBudget>>20)
	}
}
