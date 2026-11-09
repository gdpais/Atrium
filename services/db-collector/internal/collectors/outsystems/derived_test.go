package outsystems

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	collectormetadata "atrium/services/db-collector/internal/metadata"
)

// derivedRun feeds rows of one log type to a fresh Metrics.
type derivedRun struct {
	t        *testing.T
	registry *prometheus.Registry
	metrics  *Metrics
}

func newDerivedRun(t *testing.T) *derivedRun {
	t.Helper()
	registry := prometheus.NewRegistry()
	metrics, err := NewMetrics(registry)
	if err != nil {
		t.Fatal(err)
	}
	return &derivedRun{t: t, registry: registry, metrics: metrics}
}

// observe passes rows, given as column → value maps, as one batch of
// logType from collector "c" in environment "prod".  Every row's Instant
// is t0.
func (d *derivedRun) observe(logType string, rows ...map[string]any) {
	d.t.Helper()
	d.observeFrom("c", logType, rows...)
}

func (d *derivedRun) observeFrom(collector, logType string, rows ...map[string]any) {
	d.t.Helper()
	var batch Batch
	position := map[string]int{}
	for _, row := range rows {
		for name := range row {
			if _, ok := position[name]; !ok {
				position[name] = len(batch.Columns)
				batch.Columns = append(batch.Columns, Column{Name: name, DatabaseType: "NVARCHAR"})
			}
		}
	}
	instants := make([]time.Time, len(rows))
	for r, row := range rows {
		values := make([]any, len(batch.Columns))
		for name, v := range row {
			values[position[name]] = v
		}
		batch.Rows = append(batch.Rows, values)
		instants[r] = t0
	}
	item := collectormetadata.ScheduledProbe{
		CollectorID: collector,
		Target:      collectormetadata.DatabaseTarget{Name: "logging-db", EnvironmentSlug: "prod"},
		Definition:  collectormetadata.ProbeDefinition{Name: logType},
	}
	d.metrics.observeRows(item, batch, batch.Rows, instants)
}

// value returns the counter, or the histogram's sample count, of the series
// of name with exactly labels (collector="c", environment="prod" added); it
// fails the test when there is no such series.
func (d *derivedRun) value(name string, labels map[string]string) float64 {
	d.t.Helper()
	metric, ok := d.series(name, labels)
	if !ok {
		d.t.Fatalf("no series %s%v", name, labels)
	}
	if metric.Histogram != nil {
		return float64(metric.Histogram.GetSampleCount())
	}
	return metric.Counter.GetValue()
}

// sum returns the histogram sum of a series, as value finds it.
func (d *derivedRun) sum(name string, labels map[string]string) float64 {
	d.t.Helper()
	metric, ok := d.series(name, labels)
	if !ok || metric.Histogram == nil {
		d.t.Fatalf("no histogram %s%v", name, labels)
	}
	return metric.Histogram.GetSampleSum()
}

func (d *derivedRun) series(name string, labels map[string]string) (*dto.Metric, bool) {
	d.t.Helper()
	want := map[string]string{"collector": "c", "environment": "prod"}
	for k, v := range labels {
		want[k] = v
	}
	families, err := d.registry.Gather()
	if err != nil {
		d.t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
	metrics:
		for _, metric := range family.Metric {
			if len(metric.Label) != len(want) {
				continue
			}
			for _, label := range metric.Label {
				if want[label.GetName()] != label.GetValue() {
					continue metrics
				}
			}
			return metric, true
		}
	}
	return nil, false
}

// count returns the number of series of name.
func (d *derivedRun) count(name string) int {
	d.t.Helper()
	return testutil.CollectAndCount(d.registry, name)
}

func TestDerivedMetricsCountScreenRequests(t *testing.T) {
	d := newDerivedRun(t)
	row := map[string]any{
		"Application_Name": "Shop", "Espace_Name": "Shop_CS", "Screen": "Checkout", "Action_Name": "btnPlaceOrder_Click",
		"Duration": int64(842), "Server": "FE-01", "Session_Bytes": int64(18432), "Viewstate_Bytes": int64(65120),
	}
	d.observe("screen", row, row)

	op := map[string]string{"application": "Shop", "module": "Shop_CS", "log_type": "screen", "operation": "Checkout"}
	if got := d.value(MetricOperations, merge(op, "outcome", "success")); got != 2 {
		t.Fatalf("operations{outcome=success} = %v, want 2", got)
	}
	// The error series exists at 0, so an error ratio is 0, not missing.
	if got := d.value(MetricOperations, merge(op, "outcome", "error")); got != 0 {
		t.Fatalf("operations{outcome=error} = %v, want 0", got)
	}
	if got := d.value(MetricOperationSeconds, op); got != 1.684 {
		t.Fatalf("operation seconds = %v, want 1.684", got)
	}
	duration := map[string]string{"application": "Shop", "module": "Shop_CS", "log_type": "screen", "outcome": "success"}
	if got, sum := d.value(MetricOperationDuration, duration), d.sum(MetricOperationDuration, duration); got != 2 || sum != 1.684 {
		t.Fatalf("operation duration count %v sum %v, want 2 and 1.684", got, sum)
	}
	if got := d.value(MetricFrontEndOperations, map[string]string{"application": "Shop", "front_end": "FE-01", "outcome": "success"}); got != 2 {
		t.Fatalf("front-end operations = %v, want 2", got)
	}
	for kind, size := range map[string]float64{"session": 18432, "viewstate": 65120} {
		labels := map[string]string{"application": "Shop", "module": "Shop_CS", "kind": kind}
		if got, sum := d.value(MetricScreenState, labels), d.sum(MetricScreenState, labels); got != 2 || sum != 2*size {
			t.Fatalf("screen state %s count %v sum %v", kind, got, sum)
		}
	}
}

func TestDerivedMetricsOutcome(t *testing.T) {
	for _, tc := range []struct {
		name     string
		errorID  any
		severity any
		want     string
	}{
		{"no error", nil, nil, "success"},
		{"error id text", "e0000000-0000-4000-8000-000000000002", nil, "error"},
		{"error id integer", int64(17), nil, "error"},
		{"error id 0", int64(0), nil, "success"},
		{"blank error id", "  ", nil, "success"},
		{"error severity", nil, "ERROR", "error"},
		{"fatal severity", nil, "fatal", "error"},
		{"warning severity", nil, "WARN", "success"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := rowOutcome(tc.errorID, tc.severity, "mobile_request"); got != tc.want {
				t.Fatalf("outcome %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDerivedMetricsReactiveServerCallsAndTimers(t *testing.T) {
	d := newDerivedRun(t)
	d.observe("mobile_request", map[string]any{
		"Application_Name": "Shop", "Espace_Name": "ShopApp", "Screen": "Cart", "Action_Name": "ActionGetCart",
		"Duration": []byte("1250.000"), "Error_Id": int64(4711), "Severity": "ERROR",
	})
	if got := d.value(MetricOperations, map[string]string{"application": "Shop", "module": "ShopApp", "log_type": "mobile_request", "operation": "ActionGetCart", "outcome": "error"}); got != 1 {
		t.Fatalf("mobile request errors = %v, want 1", got)
	}
	if got := d.sum(MetricOperationDuration, map[string]string{"application": "Shop", "module": "ShopApp", "log_type": "mobile_request", "outcome": "error"}); got != 1.25 {
		t.Fatalf("decimal duration = %v s, want 1.25", got)
	}

	// Started 90 s after its schedule (every Instant here is t0).
	d.observe("cyclic_job", map[string]any{
		"Application_Name": "Shop", "Espace_Name": "Shop_BL", "Action_Name": "ProcessOrders",
		"Duration": int64(4000), "Should_Have_Run_At": t0.Add(-90 * time.Second),
	})
	timer := map[string]string{"application": "Shop", "operation": "ProcessOrders"}
	if got := d.sum(MetricTimerDelay, timer); got != 90 {
		t.Fatalf("timer delay = %v s, want 90", got)
	}
	if got := d.value(MetricOperations, map[string]string{"application": "Shop", "module": "Shop_BL", "log_type": "cyclic_job", "operation": "ProcessOrders", "outcome": "success"}); got != 1 {
		t.Fatalf("timer runs = %v, want 1", got)
	}
	// A run that starts early counts as no delay, not a negative one.
	d.observe("cyclic_job", map[string]any{
		"Application_Name": "Shop", "Action_Name": "ProcessOrders", "Should_Have_Run_At": t0.Add(time.Second),
	})
	if got, sum := d.value(MetricTimerDelay, timer), d.sum(MetricTimerDelay, timer); got != 2 || sum != 90 {
		t.Fatalf("timer delay count %v sum %v, want 2 and 90", got, sum)
	}
}

func TestDerivedMetricsIntegrations(t *testing.T) {
	d := newDerivedRun(t)
	d.observe("integration",
		map[string]any{"Application_Name": "Shop", "Espace_Name": "Shop_IS", "Action_Name": "Charge", "Type": "REST", "Is_Expose": false, "Duration": int64(2300), "Error_Id": "e1"},
		map[string]any{"Application_Name": "Shop", "Espace_Name": "Shop_API", "Action_Name": "GetOrder", "Type": "REST", "Is_Expose": true, "Duration": int64(120), "Server": "FE-02"},
	)
	if got := d.value(MetricIntegrationDuration, map[string]string{"application": "Shop", "direction": "consume", "type": "REST", "outcome": "error"}); got != 1 {
		t.Fatalf("consumed integration calls = %v, want 1", got)
	}
	if got := d.value(MetricIntegrationDuration, map[string]string{"application": "Shop", "direction": "expose", "type": "REST", "outcome": "success"}); got != 1 {
		t.Fatalf("exposed integration calls = %v, want 1", got)
	}
	// Only the exposed API is an entry point.
	if got := d.count(MetricOperations); got != 2 {
		t.Fatalf("%d operations series, want the exposed API's two outcomes", got)
	}
	if got := d.value(MetricOperations, map[string]string{"application": "Shop", "module": "Shop_API", "log_type": "integration", "operation": "GetOrder", "outcome": "success"}); got != 1 {
		t.Fatalf("exposed API operations = %v, want 1", got)
	}
}

func TestDerivedMetricsErrorsAndSlowEvents(t *testing.T) {
	d := newDerivedRun(t)
	d.observe("error",
		map[string]any{"Application_Name": "Shop", "Espace_Name": "Shop_CS", "Message": "boom"},
		map[string]any{"Application_Name": "Shop", "Espace_Name": "Shop_CS", "Message": "boom again"},
	)
	if got := d.value(MetricErrors, map[string]string{"application": "Shop", "module": "Shop_CS"}); got != 2 {
		t.Fatalf("errors = %v, want 2", got)
	}
	d.observe("general",
		map[string]any{"Application_Name": "Shop", "Espace_Name": "Shop_CS", "Module_Name": "SLOWSQL", "Duration": int64(3456)},
		map[string]any{"Application_Name": "Shop", "Espace_Name": "Shop_CS", "Module_Name": "slowextension "},
		map[string]any{"Application_Name": "Shop", "Espace_Name": "Shop_CS", "Module_Name": "Orders"},
	)
	for _, kind := range []string{"sql", "extension"} {
		if got := d.value(MetricSlowEvents, map[string]string{"application": "Shop", "module": "Shop_CS", "kind": kind}); got != 1 {
			t.Fatalf("slow %s events = %v, want 1", kind, got)
		}
	}
	if got := d.count(MetricSlowEvents); got != 2 {
		t.Fatalf("%d slow event series, want 2: other general rows are not slow events", got)
	}
	// Neither log type is an entry point.
	if got := d.count(MetricOperations); got != 0 {
		t.Fatalf("%d operations series from error and general rows, want 0", got)
	}
}

func TestDerivedMetricsSkipMissingDurations(t *testing.T) {
	d := newDerivedRun(t)
	d.observe("screen",
		map[string]any{"Application_Name": "Shop", "Espace_Name": "Shop_CS", "Screen": "Home", "Duration": nil},
		map[string]any{"Application_Name": "Shop", "Espace_Name": "Shop_CS", "Screen": "Home", "Duration": int64(-5)},
	)
	if got := d.value(MetricOperations, map[string]string{"application": "Shop", "module": "Shop_CS", "log_type": "screen", "operation": "Home", "outcome": "success"}); got != 2 {
		t.Fatalf("operations = %v, want 2: rows without a usable duration still count", got)
	}
	if got := d.count(MetricOperationDuration); got != 0 {
		t.Fatalf("%d duration series, want 0", got)
	}
}

func TestDerivedMetricsCapLabelValues(t *testing.T) {
	d := newDerivedRun(t)
	var rows []map[string]any
	for i := range maxOperations + 5 {
		rows = append(rows, map[string]any{"Application_Name": "Shop", "Espace_Name": "Shop_CS", "Screen": fmt.Sprintf("Screen%03d", i)})
	}
	d.observe("screen", rows...)
	if got := d.count(MetricOperations); got != 2*(maxOperations+1) {
		t.Fatalf("%d operations series, want %d (each kept screen and other, two outcomes each)", got, 2*(maxOperations+1))
	}
	other := map[string]string{"application": "Shop", "module": "Shop_CS", "log_type": "screen", "operation": "other", "outcome": "success"}
	if got := d.value(MetricOperations, other); got != 5 {
		t.Fatalf("operations{operation=other} = %v, want 5", got)
	}
	// A kept value stays kept; the cap is per application and log type.
	d.observe("screen", map[string]any{"Application_Name": "Shop", "Espace_Name": "Shop_CS", "Screen": "Screen000"})
	d.observe("mobile_request", map[string]any{"Application_Name": "Shop", "Espace_Name": "Shop_CS", "Action_Name": "Screen200"})
	if got := d.value(MetricOperations, map[string]string{"application": "Shop", "module": "Shop_CS", "log_type": "screen", "operation": "Screen000", "outcome": "success"}); got != 2 {
		t.Fatalf("operations{operation=Screen000} = %v, want 2", got)
	}
	if got := d.value(MetricOperations, map[string]string{"application": "Shop", "module": "Shop_CS", "log_type": "mobile_request", "operation": "Screen200", "outcome": "success"}); got != 1 {
		t.Fatalf("another log type has its own cap: got %v, want 1", got)
	}
}

func TestDerivedMetricsAreForgottenWithTheirCollector(t *testing.T) {
	d := newDerivedRun(t)
	row := map[string]any{"Application_Name": "Shop", "Espace_Name": "Shop_CS", "Screen": "Home", "Duration": int64(10)}
	d.observe("screen", row)
	d.observeFrom("other", "screen", row)
	d.metrics.forgetCollector("c")
	if got := d.count(MetricOperations); got != 2 {
		t.Fatalf("%d operations series after forgetting collector c, want the other collector's 2", got)
	}
	if got := d.count(MetricOperationDuration); got != 1 {
		t.Fatalf("%d duration series after forgetting collector c, want 1", got)
	}
}

// Rows are counted once, when they are delivered: a failed export counts
// nothing, and the retry counts the rows.
func TestExtractionCountsDerivedMetricsWhenRowsAreDelivered(t *testing.T) {
	x := newExtraction(t, t.TempDir(), 10)
	x.item.Definition.Name = "screen"
	x.source.insert("a", at(-3*time.Minute))
	x.exporter.err = errors.New("collector down")
	if err := x.run(); err == nil {
		t.Fatal("RunProbe succeeded with a failing exporter")
	}
	if got := testutil.CollectAndCount(x.metrics.derived.operations); got != 0 {
		t.Fatalf("%d operations series after a failed export, want 0", got)
	}
	x.exporter.err = nil
	x.mustRun()
	// The fake source's rows have no Screen column: the operation is unknown.
	counter := x.metrics.derived.operations.WithLabelValues(x.item.CollectorID, x.item.Target.EnvironmentSlug, "Shop", unknownService, "screen", unknownService, "success")
	if got := testutil.ToFloat64(counter); got != 1 {
		t.Fatalf("operations = %v, want 1", got)
	}
}

func merge(labels map[string]string, key, value string) map[string]string {
	out := map[string]string{key: value}
	for k, v := range labels {
		out[k] = v
	}
	return out
}
