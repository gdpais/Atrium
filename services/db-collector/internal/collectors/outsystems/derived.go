package outsystems

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"

	collectorconfig "atrium/internal/config"
	collectormetadata "atrium/services/db-collector/internal/metadata"
)

// Derived metrics, computed from every row delivered to the OTel Collector
// (docs/architecture/application-observability.md#derived-metrics).  Every
// one has the labels collector, environment and application; application is
// the row's service.name.  They count rows when they are extracted, so they
// trail real time by the lag plus the poll interval.
const (
	// MetricOperationDuration is the duration of entry points: screens,
	// Reactive server calls (mobile requests), timers and exposed APIs.
	// Labels: module, log_type, outcome.
	MetricOperationDuration = "atrium_outsystems_operation_duration_seconds"
	// MetricOperations counts entry points.  Labels: module, log_type,
	// operation, outcome.
	MetricOperations = "atrium_outsystems_operations_total"
	// MetricOperationSeconds sums entry-point durations, for the mean per
	// operation.  Labels: module, log_type, operation.
	MetricOperationSeconds = "atrium_outsystems_operation_seconds_total"
	// MetricIntegrationDuration is the duration of every integration call,
	// consumed or exposed.  Labels: direction, type, outcome.
	MetricIntegrationDuration = "atrium_outsystems_integration_duration_seconds"
	// MetricErrors counts error log rows.  Label: module.
	MetricErrors = "atrium_outsystems_errors_total"
	// MetricSlowEvents counts the slow SQL and slow extension rows the
	// platform writes to the general log.  Labels: module, kind.
	MetricSlowEvents = "atrium_outsystems_slow_events_total"
	// MetricTimerDelay is how late timers start against their schedule.
	// Label: operation.
	MetricTimerDelay = "atrium_outsystems_timer_delay_seconds"
	// MetricScreenState is the session and viewstate size of Traditional web
	// screen requests.  Labels: module, kind.
	MetricScreenState = "atrium_outsystems_screen_state_bytes"
	// MetricFrontEndOperations counts entry points per front-end server.
	// Labels: front_end, outcome.
	MetricFrontEndOperations = "atrium_outsystems_front_end_operations_total"
)

// derivedMetrics names every derived metric, the values of the metric label
// of [MetricDerivedSeriesOverflow].
var derivedMetrics = []string{
	MetricOperationDuration, MetricOperations, MetricOperationSeconds, MetricIntegrationDuration,
	MetricErrors, MetricSlowEvents, MetricTimerDelay, MetricScreenState, MetricFrontEndOperations,
}

// The series budget.  The label caps below bound each label, but they
// multiply: up to 1,000 applications, 100 modules each and 100 operations
// per application and log type, times log types and outcomes, and a
// histogram costs 11 or 13 series per label set.  So each collector also
// holds at most its max_derived_series derived series, counted as
// Prometheus stores them: 1 per counter label set, and a histogram's
// buckets, +Inf, sum and count per label set.  Past it, an observation that
// needs a new series is dropped and counted in
// [MetricDerivedSeriesOverflow]; series that exist keep updating.  The budget
// is freed with the series when the collector stops, which also happens when
// a reload changes it.
const (
	// MetricDerivedSeries is the number of derived series the collector
	// holds, as the budget counts them.  Label: collector.
	MetricDerivedSeries = "atrium_outsystems_derived_series"
	// MetricDerivedSeriesOverflow counts the observations the derived
	// metrics dropped because they needed a new series past the collector's
	// budget: one per row and metric.  Labels: collector, metric.
	MetricDerivedSeriesOverflow = "atrium_outsystems_derived_series_overflow_total"
)

// Label caps.  A value past its cap is counted under [otherValue], so a
// module, screen or server name the platform invents cannot grow the series
// without bound.  The first values seen since the collector started keep
// their own series; a restart can keep different ones.
const (
	// maxApplications caps application per collector.  It is a guard: a
	// factory rarely has more applications.
	maxApplications = 1000
	// maxModules caps module per collector and application.
	maxModules = 100
	// maxOperations caps operation per collector, application and log type.
	maxOperations = 100
	// maxFrontEnds caps front_end per collector.
	maxFrontEnds = 50
	// maxIntegrationTypes caps type per collector.
	maxIntegrationTypes = 20
	// otherValue is the value of a label past its cap.
	otherValue = "other"
)

// Outcomes and other fixed label values.
const (
	outcomeSuccess = "success"
	outcomeError   = "error"
)

// Entry-point log types: one row is one request or timer run.  Integration
// rows are entry points only when they are exposed APIs.
const (
	logTypeScreen        = "screen"
	logTypeMobileRequest = "mobile_request"
	logTypeCyclicJob     = "cyclic_job"
	logTypeIntegration   = "integration"
	logTypeGeneral       = "general"
)

// Columns the derived metrics read besides the ones records.go names.
const (
	columnDuration       = "Duration"
	columnScreen         = "Screen"
	columnActionName     = "Action_Name"
	columnErrorID        = "Error_Id"
	columnServer         = "Server"
	columnIsExpose       = "Is_Expose"
	columnType           = "Type"
	columnShouldHaveRun  = "Should_Have_Run_At"
	columnSessionBytes   = "Session_Bytes"
	columnViewstateBytes = "Viewstate_Bytes"
)

// slowEventKinds maps the general log's Module_Name values for slow events
// to the kind label.
var slowEventKinds = map[string]string{
	"SLOWSQL":       "sql",
	"SLOWEXTENSION": "extension",
}

// Histogram buckets.  The latency buckets hold the SLO thresholds the
// recording rules read (le="2" for requests).
var (
	latencyBuckets    = []float64{0.1, 0.25, 0.5, 1, 2, 3, 5, 10, 30, 60}
	timerDelayBuckets = []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600}
	// stateBuckets run from 1 KiB to 16 MiB, ×4.
	stateBuckets = prometheus.ExponentialBuckets(1024, 4, 8)
)

// derivedLabels are the labels every derived metric starts with.
var derivedLabels = []string{"collector", "environment", "application"}

func withDerivedLabels(extra ...string) []string {
	return append(append([]string{}, derivedLabels...), extra...)
}

// derived holds the derived metric vectors.
type derived struct {
	operationDuration   *prometheus.HistogramVec
	operations          *prometheus.CounterVec
	operationSeconds    *prometheus.CounterVec
	integrationDuration *prometheus.HistogramVec
	errors              *prometheus.CounterVec
	slowEvents          *prometheus.CounterVec
	timerDelay          *prometheus.HistogramVec
	screenState         *prometheus.HistogramVec
	frontEndOperations  *prometheus.CounterVec

	// series and overflow report the series budget.
	series   *prometheus.GaugeVec
	overflow *prometheus.CounterVec

	caps labelCaps

	// mu guards budget and makes creating a derived series and counting it
	// one step, and deleting a collector's series and freeing its budget
	// another, so a series is never created past the budget or left out of
	// it.
	mu     sync.Mutex
	budget seriesBudget
}

func newDerived() *derived {
	return &derived{
		operationDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    MetricOperationDuration,
			Help:    "Duration of OutSystems entry points (screens, Reactive server calls, timers, exposed APIs), from the logging database.",
			Buckets: latencyBuckets,
		}, withDerivedLabels("module", "log_type", "outcome")),
		operations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: MetricOperations,
			Help: "OutSystems entry points (screens, Reactive server calls, timers, exposed APIs) by operation, from the logging database.",
		}, withDerivedLabels("module", "log_type", "operation", "outcome")),
		operationSeconds: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: MetricOperationSeconds,
			Help: "Total duration of OutSystems entry points by operation; divide by the operations counter for the mean.",
		}, withDerivedLabels("module", "log_type", "operation")),
		integrationDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    MetricIntegrationDuration,
			Help:    "Duration of OutSystems integration calls, consumed and exposed, from the logging database.",
			Buckets: latencyBuckets,
		}, withDerivedLabels("direction", "type", "outcome")),
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: MetricErrors,
			Help: "OutSystems error log rows by module.",
		}, withDerivedLabels("module")),
		slowEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: MetricSlowEvents,
			Help: "Slow SQL and slow extension calls OutSystems reports in the general log.",
		}, withDerivedLabels("module", "kind")),
		timerDelay: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    MetricTimerDelay,
			Help:    "Seconds OutSystems timers started after their scheduled time.",
			Buckets: timerDelayBuckets,
		}, withDerivedLabels("operation")),
		screenState: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    MetricScreenState,
			Help:    "Session and viewstate size of OutSystems Traditional web screen requests, in bytes.",
			Buckets: stateBuckets,
		}, withDerivedLabels("module", "kind")),
		frontEndOperations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: MetricFrontEndOperations,
			Help: "OutSystems entry points by front-end server.",
		}, withDerivedLabels("front_end", "outcome")),
		series: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: MetricDerivedSeries,
			Help: "Series the derived OutSystems metrics hold for the collector, counted as Prometheus stores them; at most its max_derived_series.",
		}, []string{"collector"}),
		overflow: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: MetricDerivedSeriesOverflow,
			Help: "Rows a derived OutSystems metric (the metric label) did not count because they needed a new series past the collector's max_derived_series; series that exist keep updating.",
		}, []string{"collector", "metric"}),
		budget: newSeriesBudget(),
	}
}

func (d *derived) collectors() []prometheus.Collector {
	return []prometheus.Collector{
		d.operationDuration, d.operations, d.operationSeconds, d.integrationDuration,
		d.errors, d.slowEvents, d.timerDelay, d.screenState, d.frontEndOperations,
		d.series, d.overflow,
	}
}

// init sets the series budget of collector, its max_derived_series (the
// default when unset), and creates its budget series: the series count, and
// the overflow counters at 0, so increase() sees the first overflow.
func (d *derived) init(collector collectorconfig.CollectorRuntimeConfig) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.budget.limits[collector.ID] = collector.OutSystemsLogs.MaxDerivedSeries
	d.setSeriesLocked(collector.ID)
	for _, metric := range derivedMetrics {
		_, _ = d.overflow.GetMetricWithLabelValues(collector.ID, metric)
	}
}

// forget deletes every series and label cap of collectorID and frees its
// series budget.
func (d *derived) forget(collectorID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	labels := prometheus.Labels{"collector": collectorID}
	for _, vec := range []interface {
		DeletePartialMatch(prometheus.Labels) int
	}{
		d.operationDuration, d.operations, d.operationSeconds, d.integrationDuration,
		d.errors, d.slowEvents, d.timerDelay, d.screenState, d.frontEndOperations,
		d.series, d.overflow,
	} {
		vec.DeletePartialMatch(labels)
	}
	d.budget.forget(collectorID)
	d.caps.forget(collectorID)
}

// observe updates the derived metrics from rows of batch, which were
// delivered.  instants holds each row's Instant as a wall clock.
func (d *derived) observe(item collectormetadata.ScheduledProbe, batch Batch, rows [][]any, instants []time.Time) {
	columns := indexColumns(batch)
	logType := item.Definition.Name
	collector := item.CollectorID
	for r, row := range rows {
		get := func(name string) any { return columns.value(row, name) }
		application := d.caps.value(maxApplications, firstText(get(columnApplication), get(columnEspace), get(columnModule)), collector, "application")
		module := d.caps.value(maxModules, labelText(get(columnEspace)), collector, "module", application)
		base := []string{collector, item.Target.EnvironmentSlug, application}
		outcome := rowOutcome(get(columnErrorID), get(columnSeverity), logType)
		duration, hasDuration := seconds(get(columnDuration))

		switch logType {
		case errorLogType:
			d.count(d.errors, MetricErrors, append(base, module), 1)
		case logTypeGeneral:
			if kind, ok := slowEventKinds[strings.ToUpper(strings.TrimSpace(optionalText(get(columnModule))))]; ok {
				d.count(d.slowEvents, MetricSlowEvents, append(base, module, kind), 1)
			}
		case logTypeIntegration:
			direction := "consume"
			if exposed, _ := get(columnIsExpose).(bool); exposed {
				direction = "expose"
			}
			kind := d.caps.value(maxIntegrationTypes, labelText(get(columnType)), collector, "type")
			if hasDuration {
				d.observeValue(d.integrationDuration, MetricIntegrationDuration, latencySeries, append(base, direction, kind, outcome), duration)
			}
		case logTypeCyclicJob:
			scheduled, ok := get(columnShouldHaveRun).(time.Time)
			if ok && !scheduled.IsZero() {
				delay := max(instants[r].Sub(scheduled).Seconds(), 0)
				operation := d.caps.value(maxOperations, labelText(get(columnActionName)), collector, "operation", application, logType)
				d.observeValue(d.timerDelay, MetricTimerDelay, timerDelaySeries, append(base, operation), delay)
			}
		case logTypeScreen:
			for _, state := range []struct{ column, kind string }{{columnSessionBytes, "session"}, {columnViewstateBytes, "viewstate"}} {
				if size, ok := number(get(state.column)); ok && size >= 0 {
					d.observeValue(d.screenState, MetricScreenState, stateSeries, append(base, module, state.kind), size)
				}
			}
		}

		if !isEntryPoint(logType, get(columnIsExpose)) {
			continue
		}
		operationColumn := columnActionName
		if logType == logTypeScreen {
			operationColumn = columnScreen
		}
		operation := d.caps.value(maxOperations, labelText(get(operationColumn)), collector, "operation", application, logType)
		d.countOutcome(d.operations, MetricOperations, append(base, module, logType, operation), outcome)
		if hasDuration {
			d.observeValue(d.operationDuration, MetricOperationDuration, latencySeries, append(base, module, logType, outcome), duration)
			d.count(d.operationSeconds, MetricOperationSeconds, append(base, module, logType, operation), duration)
		}
		if server := labelText(get(columnServer)); server != unknownService {
			frontEnd := d.caps.value(maxFrontEnds, server, collector, "front_end")
			d.countOutcome(d.frontEndOperations, MetricFrontEndOperations, append(base, frontEnd), outcome)
		}
	}
}

// isEntryPoint reports whether a row of logType is a request or timer run:
// screens, Reactive server calls, timers, and integrations that are exposed
// APIs.
func isEntryPoint(logType string, isExpose any) bool {
	switch logType {
	case logTypeScreen, logTypeMobileRequest, logTypeCyclicJob:
		return true
	case logTypeIntegration:
		exposed, _ := isExpose.(bool)
		return exposed
	}
	return false
}

// rowOutcome is error when the row links to an error log row or its
// severity is ERROR or above, otherwise success.
func rowOutcome(errorID, rawSeverity any, logType string) string {
	switch id := errorID.(type) {
	case nil:
	case int64, int32, int16, int:
		if fmt.Sprint(id) != "0" {
			return outcomeError
		}
	default:
		if text := strings.TrimSpace(textOf(id)); text != "" && text != "0" {
			return outcomeError
		}
	}
	if level, _ := severity(rawSeverity, logType); level >= logspb.SeverityNumber_SEVERITY_NUMBER_ERROR {
		return outcomeError
	}
	return outcomeSuccess
}

// seconds returns a Duration value, in milliseconds, as seconds.  NULL,
// non-numeric and negative values give false.
func seconds(v any) (float64, bool) {
	ms, ok := number(v)
	if !ok || ms < 0 {
		return 0, false
	}
	return ms / 1000, true
}

// number returns a numeric column value as a float64: integers, floats,
// and decimal or numeric values, which the driver returns as text bytes.
func number(v any) (float64, bool) {
	switch v := v.(type) {
	case int64:
		return float64(v), true
	case int32:
		return float64(v), true
	case int16:
		return float64(v), true
	case int8:
		return float64(v), true
	case uint8:
		return float64(v), true
	case int:
		return float64(v), true
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case []byte:
		f, err := strconv.ParseFloat(strings.TrimSpace(string(v)), 64)
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f, err == nil
	}
	return 0, false
}

// labelText returns a column value as a label value: trimmed text, or
// unknownService when it is NULL or blank.
func labelText(v any) string {
	if text := strings.TrimSpace(optionalText(v)); text != "" {
		return text
	}
	return unknownService
}

// Series per histogram label set, as Prometheus stores them: the buckets,
// +Inf, sum and count.
var (
	latencySeries    = len(latencyBuckets) + 3
	timerDelaySeries = len(timerDelayBuckets) + 3
	stateSeries      = len(stateBuckets) + 3
)

// The writes below create a series only within its collector's budget, and
// skip label values Prometheus rejects.  values are the series' label
// values, which start with the collector ID, as every derived metric's
// labels do.  Each metric is admitted on its own, so a row that arrives as
// the budget fills can be counted in some metrics and not in others.

// count adds v to the counter of metric with values.
func (d *derived) count(vec *prometheus.CounterVec, metric string, values []string, v float64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fresh, ok := d.admitLocked(metric, 1, values)
	if !ok {
		return
	}
	if counter, err := vec.GetMetricWithLabelValues(values...); err == nil {
		counter.Add(v)
	} else {
		d.releaseLocked(metric, 1, fresh, values)
	}
}

// countOutcome increments the counter of metric with values and outcome.
// The series of both outcomes are created together, so an error ratio reads
// 0 rather than nothing before the first error.
func (d *derived) countOutcome(vec *prometheus.CounterVec, metric string, values []string, outcome string) {
	withOutcome := func(o string) []string {
		return append(append(make([]string, 0, len(values)+1), values...), o)
	}
	sets := [][]string{withOutcome(outcomeSuccess), withOutcome(outcomeError)}
	d.mu.Lock()
	defer d.mu.Unlock()
	fresh, ok := d.admitLocked(metric, 1, sets...)
	if !ok {
		return
	}
	for i, set := range sets {
		counter, err := vec.GetMetricWithLabelValues(set...)
		if err != nil {
			d.releaseLocked(metric, 1, fresh&(1<<i), set)
			continue
		}
		if set[len(set)-1] == outcome {
			counter.Inc()
		}
	}
}

// observeValue observes v in the histogram of metric with values, which
// costs series series per label set.
func (d *derived) observeValue(vec *prometheus.HistogramVec, metric string, series int, values []string, v float64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fresh, ok := d.admitLocked(metric, series, values)
	if !ok {
		return
	}
	if histogram, err := vec.GetMetricWithLabelValues(values...); err == nil {
		histogram.Observe(v)
	} else {
		d.releaseLocked(metric, series, fresh, values)
	}
}

// admitLocked reports whether the series of metric with each label set in
// sets may be written: the collector holds it already, or the new ones,
// cost series each, fit in its budget together and are counted from now
// on; fresh has bit i set when sets[i] is new.  Otherwise nothing is
// written, and the overflow counter of metric grows by one.  d.mu must be
// held.
func (d *derived) admitLocked(metric string, cost int, sets ...[]string) (fresh uint64, ok bool) {
	collectorID := sets[0][0]
	fresh, ok = d.budget.admit(collectorID, metric, cost, sets...)
	switch {
	case !ok:
		if counter, err := d.overflow.GetMetricWithLabelValues(collectorID, metric); err == nil {
			counter.Inc()
		}
	case fresh != 0:
		d.setSeriesLocked(collectorID)
	}
	return fresh, ok
}

// releaseLocked frees the budget of a series admitLocked counted (fresh not
// 0) but Prometheus did not create.  d.mu must be held.
func (d *derived) releaseLocked(metric string, cost int, fresh uint64, values []string) {
	if fresh == 0 {
		return
	}
	d.budget.release(values[0], metric, cost, values)
	d.setSeriesLocked(values[0])
}

// setSeriesLocked exports the series collectorID holds.  d.mu must be held.
func (d *derived) setSeriesLocked(collectorID string) {
	if gauge, err := d.series.GetMetricWithLabelValues(collectorID); err == nil {
		gauge.Set(float64(d.budget.used(collectorID)))
	}
}

// seriesBudget is the bookkeeping of the series budget: per collector, its
// limit, and the label sets of the derived series it holds with what they
// cost.  It is not safe for concurrent use; derived.mu guards it.
type seriesBudget struct {
	limits map[string]int
	held   map[string]*heldSeries
	// key is reused to build series keys, so looking up a series that is
	// held already does not allocate.
	key []byte
}

// heldSeries are the derived series one collector holds.
type heldSeries struct {
	keys map[string]struct{}
	// series is what keys cost, counted as Prometheus stores them.
	series int
}

func newSeriesBudget() seriesBudget {
	return seriesBudget{limits: map[string]int{}, held: map[string]*heldSeries{}}
}

// limit returns collectorID's budget: its max_derived_series, or the
// default for a collector it was not told about.
func (b *seriesBudget) limit(collectorID string) int {
	if n := b.limits[collectorID]; n > 0 {
		return n
	}
	return collectorconfig.DefaultLogMaxDerivedSeries
}

// used returns the series collectorID holds.
func (b *seriesBudget) used(collectorID string) int {
	if h := b.held[collectorID]; h != nil {
		return h.series
	}
	return 0
}

// admit reports whether collectorID may write the series of metric with
// each label set in sets: those it holds already, and the others if, at
// cost series each, they fit in its budget together.  It then holds them,
// and fresh has bit i set for each new sets[i].  At most 64 sets.
func (b *seriesBudget) admit(collectorID, metric string, cost int, sets ...[]string) (fresh uint64, ok bool) {
	h := b.held[collectorID]
	need := 0
	for i, set := range sets {
		if h != nil {
			if _, held := h.keys[string(b.keyOf(metric, set))]; held {
				continue
			}
		}
		fresh |= 1 << i
		need += cost
	}
	if fresh == 0 {
		return 0, true
	}
	if b.used(collectorID)+need > b.limit(collectorID) {
		return 0, false
	}
	if h == nil {
		h = &heldSeries{keys: map[string]struct{}{}}
		b.held[collectorID] = h
	}
	for i, set := range sets {
		if fresh&(1<<i) != 0 {
			h.keys[string(b.keyOf(metric, set))] = struct{}{}
		}
	}
	h.series += need
	return fresh, true
}

// release stops holding the series of metric with values, which cost cost.
func (b *seriesBudget) release(collectorID, metric string, cost int, values []string) {
	h := b.held[collectorID]
	if h == nil {
		return
	}
	key := b.keyOf(metric, values)
	if _, held := h.keys[string(key)]; held {
		delete(h.keys, string(key))
		h.series -= cost
	}
}

// forget drops collectorID's limit and frees what it holds.
func (b *seriesBudget) forget(collectorID string) {
	delete(b.limits, collectorID)
	delete(b.held, collectorID)
}

// keyOf builds the key of the series of metric with values into b.key and
// returns it; it is valid until the next call.  Label values that are not
// valid UTF-8, which could contain the separator, are rejected by
// Prometheus and their fresh series released.
func (b *seriesBudget) keyOf(metric string, values []string) []byte {
	key := append(b.key[:0], metric...)
	for _, v := range values {
		key = append(key, 0xff)
		key = append(key, v...)
	}
	b.key = key
	return key
}

// labelCaps keeps the values each capped label has seen, per scope.  It is
// safe for concurrent use.
type labelCaps struct {
	mu   sync.Mutex
	seen map[string]map[string]struct{}
}

// value returns v when it is already known in scope or scope has room for
// it, and otherValue when scope is full.  scope is the collector ID, the
// label name, then whatever the cap is per (application, log type).
func (c *labelCaps) value(limit int, v string, scope ...string) string {
	key := strings.Join(scope, "\x00")
	c.mu.Lock()
	defer c.mu.Unlock()
	values, ok := c.seen[key]
	if !ok {
		if c.seen == nil {
			c.seen = map[string]map[string]struct{}{}
		}
		values = map[string]struct{}{}
		c.seen[key] = values
	}
	if _, ok := values[v]; ok {
		return v
	}
	if len(values) >= limit {
		return otherValue
	}
	values[v] = struct{}{}
	return v
}

// forget drops every scope of collectorID.
func (c *labelCaps) forget(collectorID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prefix := collectorID + "\x00"
	for key := range c.seen {
		if key == collectorID || strings.HasPrefix(key, prefix) {
			delete(c.seen, key)
		}
	}
}

// columnIndex maps a column name to its first position in a batch.
type columnIndex map[string]int

func indexColumns(batch Batch) columnIndex {
	index := columnIndex{}
	for i, column := range batch.Columns {
		if _, seen := index[column.Name]; !seen {
			index[column.Name] = i
		}
	}
	return index
}

// value returns row's value of the column name, or nil when the batch has no
// such column.
func (c columnIndex) value(row []any, name string) any {
	if i, ok := c[name]; ok {
		return row[i]
	}
	return nil
}
