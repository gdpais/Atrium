package outsystems

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	collectorconfig "atrium/internal/config"
)

// startCollector tells the metrics that collector id starts, with a series
// budget of maxSeries (0 for the default).
func (d *derivedRun) startCollector(id string, maxSeries int) {
	d.metrics.initCollector(collectorconfig.CollectorRuntimeConfig{
		ID: id, Kind: collectorconfig.KindOutSystemsLogs, Enabled: true,
		OutSystemsLogs: collectorconfig.OutSystemsLogsConfig{MaxDerivedSeries: maxSeries},
	})
}

// overflow returns the overflow counter of collector and metric.
func (d *derivedRun) overflow(collector, metric string) float64 {
	d.t.Helper()
	return testutil.ToFloat64(d.metrics.derived.overflow.WithLabelValues(collector, metric))
}

// held returns the derived series collector holds, as its gauge says.
func (d *derivedRun) held(collector string) float64 {
	d.t.Helper()
	return testutil.ToFloat64(d.metrics.derived.series.WithLabelValues(collector))
}

// stored counts the derived series of collector as Prometheus stores them:
// one per counter, and a histogram's buckets, +Inf, sum and count.
func (d *derivedRun) stored(collector string) int {
	d.t.Helper()
	families, err := d.registry.Gather()
	if err != nil {
		d.t.Fatal(err)
	}
	n := 0
	for _, family := range families {
		if !isDerivedMetric(family.GetName()) {
			continue
		}
		for _, metric := range family.Metric {
			if labelValue(metric, "collector") != collector {
				continue
			}
			if family.GetType() == dto.MetricType_HISTOGRAM {
				n += len(metric.Histogram.Bucket) + 3
			} else {
				n++
			}
		}
	}
	return n
}

func isDerivedMetric(name string) bool {
	for _, metric := range derivedMetrics {
		if name == metric {
			return true
		}
	}
	return false
}

func labelValue(metric *dto.Metric, name string) string {
	for _, label := range metric.Label {
		if label.GetName() == name {
			return label.GetValue()
		}
	}
	return ""
}

func screenRow(screen string) map[string]any {
	return map[string]any{"Application_Name": "Shop", "Espace_Name": "Shop_CS", "Screen": screen}
}

// Past the budget, a row that needs a new series is dropped and counted;
// series that exist keep counting.
func TestDerivedSeriesBudgetStopsNewSeries(t *testing.T) {
	d := newDerivedRun(t)
	// Room for three screens: each one's operations counter has two series,
	// one per outcome.
	d.startCollector("c", 6)
	d.observe("screen", screenRow("Home"), screenRow("List"), screenRow("Detail"), screenRow("Edit"), screenRow("Search"))

	if got := d.count(MetricOperations); got != 6 {
		t.Fatalf("%d operations series, want 6: three screens, two outcomes each", got)
	}
	if got := d.overflow("c", MetricOperations); got != 2 {
		t.Fatalf("overflow{metric=operations} = %v, want 2: Edit and Search", got)
	}
	if got := d.held("c"); got != 6 {
		t.Fatalf("derived series = %v, want 6", got)
	}
	for _, screen := range []string{"Edit", "Search"} {
		if _, ok := d.series(MetricOperations, map[string]string{"application": "Shop", "module": "Shop_CS", "log_type": "screen", "operation": screen, "outcome": "success"}); ok {
			t.Fatalf("%s has a series past the budget", screen)
		}
	}

	// A screen that has its series keeps counting, and the budget does not
	// move; a new one is dropped again.
	d.observe("screen", screenRow("Home"), screenRow("Home"), screenRow("Edit"))
	home := map[string]string{"application": "Shop", "module": "Shop_CS", "log_type": "screen", "operation": "Home", "outcome": "success"}
	if got := d.value(MetricOperations, home); got != 3 {
		t.Fatalf("operations{operation=Home} = %v, want 3", got)
	}
	if got := d.overflow("c", MetricOperations); got != 3 {
		t.Fatalf("overflow{metric=operations} = %v, want 3", got)
	}
	if got, stored := d.held("c"), d.stored("c"); got != 6 || stored != 6 {
		t.Fatalf("derived series = %v, %d stored, want 6", got, stored)
	}
}

// A histogram costs its buckets, +Inf, sum and count, and each metric is
// admitted on its own: a row whose duration series exists still updates it
// when its new operation does not fit.
func TestDerivedSeriesBudgetCountsHistogramSeries(t *testing.T) {
	d := newDerivedRun(t)
	// Home's row needs 2 operations, 1 operation seconds and 13 duration
	// series (10 buckets, +Inf, sum and count).
	d.startCollector("c", 16)
	timed := func(screen string) map[string]any {
		row := screenRow(screen)
		row["Duration"] = int64(500)
		return row
	}
	d.observe("screen", timed("Home"), timed("List"))

	duration := map[string]string{"application": "Shop", "module": "Shop_CS", "log_type": "screen", "outcome": "success"}
	if got := d.value(MetricOperationDuration, duration); got != 2 {
		t.Fatalf("duration count = %v, want 2: List's row updates the module's existing histogram", got)
	}
	if got := d.count(MetricOperations); got != 2 {
		t.Fatalf("%d operations series, want Home's 2", got)
	}
	for metric, want := range map[string]float64{MetricOperations: 1, MetricOperationSeconds: 1, MetricOperationDuration: 0} {
		if got := d.overflow("c", metric); got != want {
			t.Fatalf("overflow{metric=%s} = %v, want %v", metric, got, want)
		}
	}
	if got, stored := d.held("c"), d.stored("c"); got != 16 || stored != 16 {
		t.Fatalf("derived series = %v, %d stored, want 16", got, stored)
	}

	// A histogram that does not fit whole is not created.
	d.observe("cyclic_job", map[string]any{"Application_Name": "Shop", "Espace_Name": "Shop_BL", "Action_Name": "Sync", "Should_Have_Run_At": t0.Add(-time.Second)})
	if got := d.count(MetricTimerDelay); got != 0 {
		t.Fatalf("%d timer delay series past the budget, want 0", got)
	}
	if got := d.overflow("c", MetricTimerDelay); got != 1 {
		t.Fatalf("overflow{metric=timer delay} = %v, want 1", got)
	}
}

// The budget counts exactly the series Prometheus stores, for every derived
// metric.
func TestDerivedSeriesBudgetMatchesTheStoredSeries(t *testing.T) {
	d := newDerivedRun(t)
	d.startCollector("c", 0)
	d.observe("screen", map[string]any{
		"Application_Name": "Shop", "Espace_Name": "Shop_CS", "Screen": "Checkout", "Duration": int64(842),
		"Server": "FE-01", "Session_Bytes": int64(18432), "Viewstate_Bytes": int64(65120),
	})
	d.observe("mobile_request", map[string]any{"Application_Name": "Shop", "Espace_Name": "ShopApp", "Action_Name": "GetCart", "Duration": int64(10), "Error_Id": int64(1)})
	d.observe("cyclic_job", map[string]any{"Application_Name": "Shop", "Espace_Name": "Shop_BL", "Action_Name": "Sync", "Duration": int64(10), "Should_Have_Run_At": t0.Add(-time.Second)})
	d.observe("integration",
		map[string]any{"Application_Name": "Shop", "Espace_Name": "Shop_IS", "Type": "REST", "Is_Expose": false, "Duration": int64(10)},
		map[string]any{"Application_Name": "Shop", "Espace_Name": "Shop_API", "Action_Name": "GetOrder", "Type": "SOAP", "Is_Expose": true, "Duration": int64(10), "Server": "FE-02"})
	d.observe("error", map[string]any{"Application_Name": "Shop", "Espace_Name": "Shop_CS"})
	d.observe("general", map[string]any{"Application_Name": "Shop", "Espace_Name": "Shop_CS", "Module_Name": "SLOWSQL"})

	stored := d.stored("c")
	for _, metric := range derivedMetrics {
		if testutil.CollectAndCount(d.registry, metric) == 0 {
			t.Fatalf("no %s series; the rows should touch every derived metric", metric)
		}
	}
	if got := d.held("c"); got != float64(stored) {
		t.Fatalf("derived series = %v, want the %d Prometheus stores", got, stored)
	}
}

// Stopping a collector deletes its series and frees its budget, so the
// collector started again, by a reload that changes its budget too, counts
// from 0; another collector's budget is its own.
func TestDerivedSeriesBudgetIsFreedWhenTheCollectorStops(t *testing.T) {
	d := newDerivedRun(t)
	d.startCollector("c", 4)
	d.startCollector("other", 4)
	d.observe("screen", screenRow("Home"), screenRow("List"), screenRow("Detail"))
	d.observeFrom("other", "screen", screenRow("Home"))
	if got := d.overflow("c", MetricOperations); got != 1 {
		t.Fatalf("overflow = %v, want 1 before the restart", got)
	}
	if got := d.held("other"); got != 2 {
		t.Fatalf("other collector holds %v series, want its own 2", got)
	}

	d.metrics.forgetCollector("c")
	if got := testutil.CollectAndCount(d.registry, MetricDerivedSeries, MetricDerivedSeriesOverflow); got != 1+len(derivedMetrics) {
		t.Fatalf("%d budget series after forgetting collector c, want the other collector's %d", got, 1+len(derivedMetrics))
	}
	d.startCollector("c", 6)
	if got := d.overflow("c", MetricOperations); got != 0 {
		t.Fatalf("overflow = %v after the restart, want 0", got)
	}
	d.observe("screen", screenRow("Search"), screenRow("Edit"), screenRow("History"), screenRow("Approve"))
	if got := d.count(MetricOperations); got != 2+6 {
		t.Fatalf("%d operations series, want the other collector's 2 and the restarted one's 6", got)
	}
	if got := d.overflow("c", MetricOperations); got != 1 {
		t.Fatalf("overflow = %v, want 1: the new budget holds three screens", got)
	}
	if got, stored := d.held("c"), d.stored("c"); got != 6 || stored != 6 {
		t.Fatalf("derived series = %v, %d stored, want 6", got, stored)
	}
}

// A collector the metrics were not told about gets the default budget.
func TestDerivedSeriesBudgetDefault(t *testing.T) {
	d := newDerivedRun(t)
	if got := d.metrics.derived.budget.limit("c"); got != collectorconfig.DefaultLogMaxDerivedSeries {
		t.Fatalf("limit = %d, want the default %d", got, collectorconfig.DefaultLogMaxDerivedSeries)
	}
	d.startCollector("c", 0)
	if got := d.metrics.derived.budget.limit("c"); got != collectorconfig.DefaultLogMaxDerivedSeries {
		t.Fatalf("limit with max_derived_series unset = %d, want the default", got)
	}
}

// A label value Prometheus rejects creates no series and takes no budget.
func TestDerivedSeriesBudgetIgnoresRejectedLabelValues(t *testing.T) {
	d := newDerivedRun(t)
	d.startCollector("c", 2)
	d.observe("screen", map[string]any{"Application_Name": "Shop\xff", "Espace_Name": "Shop_CS", "Screen": "Home"})
	if got := d.held("c"); got != 0 {
		t.Fatalf("derived series = %v after a rejected label value, want 0", got)
	}
	d.observe("screen", screenRow("Home"))
	if got := d.count(MetricOperations); got != 2 {
		t.Fatalf("%d operations series, want 2: the budget is still free", got)
	}
}

// Concurrent rows never create more series than the budget, and the count
// matches what Prometheus stores.
func TestDerivedSeriesBudgetHoldsUnderConcurrency(t *testing.T) {
	d := newDerivedRun(t)
	d.startCollector("c", 100)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 50 {
				d.observe("screen", screenRow(fmt.Sprintf("Screen%d_%d", g, i)))
			}
		}()
	}
	wg.Wait()
	if got := d.count(MetricOperations); got != 100 {
		t.Fatalf("%d operations series, want the budget's 100", got)
	}
	// 400 screens, 50 of which fit.
	if got := d.overflow("c", MetricOperations); got != 350 {
		t.Fatalf("overflow = %v, want 350", got)
	}
	if got, stored := d.held("c"), d.stored("c"); got != 100 || stored != 100 {
		t.Fatalf("derived series = %v, %d stored, want 100", got, stored)
	}
}

// The budget stops a label combination the caps allow: the same operation
// names in many modules, which the module and operation caps multiply.
func TestDerivedSeriesBudgetBoundsWhatTheCapsMultiply(t *testing.T) {
	d := newDerivedRun(t)
	d.startCollector("c", 1000)
	var rows []map[string]any
	for m := range maxModules {
		for o := range 20 {
			rows = append(rows, map[string]any{"Application_Name": "Shop", "Espace_Name": fmt.Sprintf("Module%03d", m), "Screen": fmt.Sprintf("Screen%02d", o)})
		}
	}
	d.observe("screen", rows...)
	if got := d.count(MetricOperations); got != 1000 {
		t.Fatalf("%d operations series, want the budget's 1000 of the 4000 the caps allow", got)
	}
	if got := d.overflow("c", MetricOperations); got != float64(len(rows)-500) {
		t.Fatalf("overflow = %v, want %d", got, len(rows)-500)
	}
}
