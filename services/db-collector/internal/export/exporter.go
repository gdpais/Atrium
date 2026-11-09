// Package export converts probe result samples into observable metrics.
//
// The [Recorder] interface is the basic write surface.  [ScopedRecorder]
// extends it with scoped replacement semantics: every series is owned by the
// [Scope] (collector, target, probe) that wrote it, and a new write for the
// same scope deletes the series that scope no longer reports.  This keeps
// label sets such as blocking session IDs, dropped databases, or removed
// targets from being exported forever with stale values.
//
// Two implementations are provided:
//
//   - [PrometheusExporter] registers one collector per unique metric name in
//     a Prometheus registry and exposes every series as a gauge or counter,
//     depending on the sample's [MetricType].  It is used in production and
//     is safe for concurrent use.
//
//   - [InMemoryExporter] stores recorded values in memory.  It is intended
//     for unit tests.
package export

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// MetricType is the Prometheus type a [Sample] is exposed as.  The zero value
// is [Gauge], so samples that do not set a type stay gauges.
type MetricType int

const (
	// Gauge is a value that can go up and down, exposed as is.
	Gauge MetricType = iota
	// Counter is a cumulative value maintained by the source, such as SQL
	// Server's wait time since startup.  The exporter exposes the value the
	// source reports instead of incrementing its own counter, so a lower
	// value after the source restarts or clears its statistics is exported as
	// is; rate() and increase() treat that drop as a counter reset.  Counter
	// values must not be negative.
	Counter
)

// String returns the Prometheus type name.
func (t MetricType) String() string {
	switch t {
	case Gauge:
		return "gauge"
	case Counter:
		return "counter"
	default:
		return fmt.Sprintf("MetricType(%d)", int(t))
	}
}

// Sample represents a single metric observation produced by a probe execution.
type Sample struct {
	// Metric is the fully-qualified Prometheus metric name
	// (e.g. "atrium_sqlserver_sessions").
	Metric string
	// Help is the human-readable description registered with the metric.
	// Falls back to Metric when empty.
	Help string
	// Value is the numeric value to record, already in the metric's base
	// unit (seconds, bytes).
	Value float64
	// Type selects how the metric is exposed.  The zero value is [Gauge].
	Type MetricType
	// Labels is the set of label key-value pairs associated with this
	// observation (e.g. {"environment": "production", "target": "db01"}).
	Labels map[string]string
}

// Recorder persists a batch of [Sample] values to an underlying store.
type Recorder interface {
	Record([]Sample) error
}

// Scope identifies the producer of a group of series: one probe of one
// target of one collector.  Collector-level series use an empty Target.
type Scope struct {
	Collector string
	Target    string
	Probe     string
}

// ScopedRecorder is a [Recorder] that tracks which series each [Scope] wrote
// so series that disappear from a probe's result are deleted instead of
// being exported with their last value forever.
//
// A gauge series written by several scopes is deleted only once no scope
// reports it any more.  A counter series has a single owner: implementations
// may reject a write from another scope while its owner still reports it,
// because two sources alternating on one counter would read as resets.
// Series written through plain Record are not owned by any scope, so
// ClearScope and ForgetCollector never delete them unless a scope also
// reports them: once the last scope reporting a series drops it, the series
// is deleted even if Record wrote it too.
type ScopedRecorder interface {
	Recorder
	// RecordScope replaces the series owned by scope with samples: series
	// written by scope previously but absent from samples are deleted.
	// Samples that fail validation are skipped and reported in the joined
	// error; the remaining samples are still recorded.
	RecordScope(scope Scope, samples []Sample) error
	// ClearScope deletes every series owned by scope.  It is used when a
	// probe fails, because an unknown value is better than a stale one.
	ClearScope(scope Scope)
	// ForgetCollector deletes every series owned by any scope of the given
	// collector.  It is used when a collector is stopped or replaced.
	ForgetCollector(collectorID string)
}

// seriesRef identifies a single exported series.
type seriesRef struct {
	metric string
	labels map[string]string
}

// seriesKey returns a stable identity for metric plus its label set.
func seriesKey(metric string, labels map[string]string) string {
	var b strings.Builder
	b.WriteString(metric)
	for _, key := range sortedKeys(labels) {
		b.WriteByte(0)
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(labels[key])
	}
	return b.String()
}

// newSeriesRef copies labels so later caller mutations cannot corrupt the
// tracked identity.
func newSeriesRef(sample Sample) seriesRef {
	labels := make(map[string]string, len(sample.Labels))
	for key, value := range sample.Labels {
		labels[key] = value
	}
	return seriesRef{metric: sample.Metric, labels: labels}
}

// scopeTracker records which series each scope owns and how many scopes own
// each series, keyed by [seriesKey].  [InMemoryExporter] uses it;
// [PrometheusExporter] tracks ownership on its series directly so recording
// does not allocate.  It is not safe for concurrent use; owners guard it with
// their own mutex.
type scopeTracker struct {
	scopes map[Scope]map[string]seriesRef
	owners map[string]int
}

func newScopeTracker() scopeTracker {
	return scopeTracker{scopes: map[Scope]map[string]seriesRef{}, owners: map[string]int{}}
}

// replace makes current the set of series owned by scope and returns the
// series that are no longer owned by any scope and must be deleted.
func (t *scopeTracker) replace(scope Scope, current map[string]seriesRef) []seriesRef {
	previous := t.scopes[scope]
	for key := range current {
		if _, ok := previous[key]; !ok {
			t.owners[key]++
		}
	}
	var stale []seriesRef
	for key, ref := range previous {
		if _, ok := current[key]; ok {
			continue
		}
		t.owners[key]--
		if t.owners[key] <= 0 {
			delete(t.owners, key)
			stale = append(stale, ref)
		}
	}
	if len(current) == 0 {
		delete(t.scopes, scope)
	} else {
		t.scopes[scope] = current
	}
	return stale
}

// forgetCollector releases every scope of collectorID and returns the series
// that are no longer owned by any scope.
func (t *scopeTracker) forgetCollector(collectorID string) []seriesRef {
	var stale []seriesRef
	for scope := range t.scopes {
		if scope.Collector == collectorID {
			stale = append(stale, t.replace(scope, nil)...)
		}
	}
	return stale
}

// duplicateSeriesError reports two samples with identical labels in one
// scoped batch.  The last value wins, so earlier rows are silently lost; this
// usually means a probe's label columns do not uniquely identify its rows.
func duplicateSeriesError(sample Sample) error {
	return fmt.Errorf("metric %s: duplicate series %v in one batch; only the last value is kept", sample.Metric, sample.Labels)
}

// InMemoryExporter implements [ScopedRecorder] by storing the most-recently
// recorded value for each metric name and for each individual series.  It is
// intended for use in unit tests: it tracks scope ownership like
// [PrometheusExporter] but does not validate samples, so it accepts label set
// or type changes and writes to counters other scopes own.
//
// InMemoryExporter is safe for concurrent use.
type InMemoryExporter struct {
	mu     sync.Mutex
	values map[string]float64
	types  map[string]MetricType
	series map[string]float64
	scopes scopeTracker
}

// NewInMemoryExporter returns an empty in-memory exporter.
func NewInMemoryExporter() *InMemoryExporter {
	return &InMemoryExporter{
		values: map[string]float64{},
		types:  map[string]MetricType{},
		series: map[string]float64{},
		scopes: newScopeTracker(),
	}
}

// Record implements [Recorder].  Each sample overwrites any previously stored
// value for the same metric name and series.
func (e *InMemoryExporter) Record(samples []Sample) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, sample := range samples {
		e.setLocked(sample)
	}
	return nil
}

// RecordScope implements [ScopedRecorder].
func (e *InMemoryExporter) RecordScope(scope Scope, samples []Sample) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	current := make(map[string]seriesRef, len(samples))
	var errs []error
	for _, sample := range samples {
		e.setLocked(sample)
		key := seriesKey(sample.Metric, sample.Labels)
		if _, dup := current[key]; dup {
			errs = append(errs, duplicateSeriesError(sample))
		}
		current[key] = newSeriesRef(sample)
	}
	e.deleteLocked(e.scopes.replace(scope, current))
	return errors.Join(errs...)
}

// ClearScope implements [ScopedRecorder].
func (e *InMemoryExporter) ClearScope(scope Scope) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.deleteLocked(e.scopes.replace(scope, nil))
}

// ForgetCollector implements [ScopedRecorder].
func (e *InMemoryExporter) ForgetCollector(collectorID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.deleteLocked(e.scopes.forgetCollector(collectorID))
}

// Value returns the current value of the series identified by metric and
// labels, and whether that series currently exists.
func (e *InMemoryExporter) Value(metric string, labels map[string]string) (float64, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	value, ok := e.series[seriesKey(metric, labels)]
	return value, ok
}

// Type returns the type of the most recently recorded sample of metric, and
// whether metric was ever recorded.
func (e *InMemoryExporter) Type(metric string) (MetricType, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	metricType, ok := e.types[metric]
	return metricType, ok
}

// setLocked stores sample.  e.mu must be held.
func (e *InMemoryExporter) setLocked(sample Sample) {
	e.values[sample.Metric] = sample.Value
	e.types[sample.Metric] = sample.Type
	e.series[seriesKey(sample.Metric, sample.Labels)] = sample.Value
}

// deleteLocked removes the given series.  e.mu must be held.
func (e *InMemoryExporter) deleteLocked(stale []seriesRef) {
	for _, ref := range stale {
		delete(e.series, seriesKey(ref.metric, ref.labels))
	}
}

// LastValue returns the most recently recorded value for metric, or 0 if no
// value has been recorded yet.  It ignores scoped deletions; use
// [InMemoryExporter.Value] to check whether a series is currently exported.
func (e *InMemoryExporter) LastValue(metric string) float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.values[metric]
}

func sortedKeys(labels map[string]string) []string {
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

var _ ScopedRecorder = (*InMemoryExporter)(nil)
