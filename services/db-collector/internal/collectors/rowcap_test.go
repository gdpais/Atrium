package collectors

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	collectorconfig "atrium/internal/config"
	collectorexport "atrium/services/db-collector/internal/export"
	collectormetadata "atrium/services/db-collector/internal/metadata"
)

// countingRows is a result set of total one-column rows that counts how far
// it was read.
type countingRows struct {
	total int
	nexts int
	scans int
}

func (r *countingRows) Columns() ([]string, error) { return []string{"value"}, nil }

func (r *countingRows) Next() bool {
	if r.nexts >= r.total {
		return false
	}
	r.nexts++
	return true
}

func (r *countingRows) Scan(dest ...any) error {
	r.scans++
	*dest[0].(*any) = int64(r.scans)
	return nil
}

func (r *countingRows) Err() error { return nil }

func TestReadRowsFailsPastMaxRowsWithoutReadingTheRest(t *testing.T) {
	for _, tc := range []struct {
		name    string
		maxRows int
	}{
		{"configured max_rows", 3},
		{"default max_rows", maxRowsFor(collectormetadata.ScheduledProbe{})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := &countingRows{total: tc.maxRows * 10}
			got, err := readRows(rows, tc.maxRows)
			if !errors.Is(err, ErrTooManyRows) {
				t.Fatalf("readRows error = %v, want ErrTooManyRows", err)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("(%d)", tc.maxRows)) || !strings.Contains(err.Error(), "max_rows") {
				t.Fatalf("error %q should name the limit and max_rows", err)
			}
			if got != nil {
				t.Fatalf("readRows returned %d rows with the error, want none", len(got))
			}
			// It stops at the first row past the limit, without scanning it.
			if rows.nexts != tc.maxRows+1 || rows.scans != tc.maxRows {
				t.Fatalf("read %d rows and scanned %d, want %d and %d", rows.nexts, rows.scans, tc.maxRows+1, tc.maxRows)
			}
		})
	}
}

func TestReadRowsReturnsUpToMaxRows(t *testing.T) {
	rows := &countingRows{total: 3}
	got, err := readRows(rows, 3)
	if err != nil {
		t.Fatalf("readRows: %v", err)
	}
	if len(got) != 3 || got[2]["value"] != int64(3) {
		t.Fatalf("rows = %v, want 3 rows", got)
	}
}

func TestMaxRowsFor(t *testing.T) {
	for _, tc := range []struct {
		maxRows, want int
	}{
		{0, collectorconfig.DefaultProbeMaxRows},
		{-1, collectorconfig.DefaultProbeMaxRows},
		{50, 50},
		// Definitions from the metadata store are not validated.
		{collectorconfig.MaxProbeMaxRows + 1, collectorconfig.MaxProbeMaxRows},
	} {
		item := collectormetadata.ScheduledProbe{Definition: collectormetadata.ProbeDefinition{MaxRows: tc.maxRows}}
		if got := maxRowsFor(item); got != tc.want {
			t.Errorf("maxRowsFor(max_rows %d) = %d, want %d", tc.maxRows, got, tc.want)
		}
	}
	if collectorconfig.DefaultProbeMaxRows != 10000 {
		t.Errorf("DefaultProbeMaxRows = %d, want 10000 (documented)", collectorconfig.DefaultProbeMaxRows)
	}
}

func TestScheduledProbesCarryMaxRows(t *testing.T) {
	target := testTarget("core-db", "sessions")
	target.Probes[0].MaxRows = 25
	items := scheduledProbes(testCollector(time.Minute, target))
	if len(items) != 1 || items[0].Definition.MaxRows != 25 {
		t.Fatalf("scheduled probes = %+v, want one with MaxRows 25", items)
	}
}

// A probe past its max_rows fails through the usual probe-failure path: the
// target is down, its series are cleared, and the error is counted with
// reason "error".
func TestProbePastMaxRowsFails(t *testing.T) {
	metrics, reg := newTestProbeMetrics(t)
	executor := funcExecutor(func(_ context.Context, item collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
		limit := maxRowsFor(item)
		if _, err := readRows(&countingRows{total: limit + 1}, limit); err != nil {
			return nil, nil, err
		}
		return sampleFor(item), nil, nil
	})
	exporter := collectorexport.NewInMemoryExporter()
	runner := NewRunner(executor, exporter, nil).WithLogger(slog.New(slog.DiscardHandler)).WithProbeMetrics(metrics)
	target := testTarget("core-db", "sessions")
	target.Probes[0].MaxRows = 2
	result, err := runner.RunOnce(context.Background(), testCollector(time.Minute, target))
	if err == nil || !errors.Is(result.Targets[0].Err, ErrTooManyRows) {
		t.Fatalf("RunOnce error = %v, target error = %v, want ErrTooManyRows", err, result.Targets[0].Err)
	}
	if got, want := nonZero(probeSeries(t, reg, MetricProbeErrors)), []string{"sql-prod/core-db/sessions/error=1"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("probe errors = %v, want %v", got, want)
	}
	if up, _ := exporter.Value(MetricTargetUp, healthLabels("core-db")); up != 0 {
		t.Fatalf("%s = %v, want 0", MetricTargetUp, up)
	}
	if _, found := exporter.Value(testMetric, sampleLabels("core-db", "sessions")); found {
		t.Fatal("a probe past max_rows exported samples")
	}
}
