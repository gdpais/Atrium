package outsystems

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	collectorconfig "heartbeat/internal/config"
	"heartbeat/internal/telemetry"
	collectormetadata "heartbeat/services/db-collector/internal/metadata"
)

// Extraction self-metrics, maintained by [Metrics].  Labels: collector,
// environment, target, log_type.  Probe duration, probe errors and target
// health come from the Runner, with the log type as the probe label.
const (
	// MetricRows counts rows extracted and delivered to the OTel Collector.
	MetricRows = "heartbeat_outsystems_extract_rows_total"
	// MetricLag is how far the watermark trails now: rows newer than it are
	// not extracted yet.  At rest it is about lag plus the poll interval.
	MetricLag = "heartbeat_outsystems_extract_lag_seconds"
	// MetricLagSetting is the lag the log type is configured with: its own,
	// or the collector's.  MetricLag never goes below it, so an objective on
	// the lag is set relative to it.
	MetricLagSetting = "heartbeat_outsystems_extract_lag_setting_seconds"
	// MetricExportFailures counts batches the OTel Collector did not accept
	// after retries; the watermark stays and the batch is read again.
	MetricExportFailures = "heartbeat_outsystems_export_failures_total"
	// MetricStuckBatches counts cycles that failed because every row of a
	// full batch had the same Instant.  Extraction of the log type cannot
	// move until batch_limit is raised.
	MetricStuckBatches = "heartbeat_outsystems_extract_stuck_batches_total"
	// MetricLogDelay is now minus the newest Instant extracted, set when rows
	// arrive: how far behind the platform's own log service is.
	MetricLogDelay = "heartbeat_outsystems_log_delay_seconds"
	// MetricExportBlocked is 1 while the log type waits for the OTel
	// Collector to accept a batch, past the cycle that read it: its
	// extraction is paused, but its target is up (export.go).  0 otherwise.
	MetricExportBlocked = "heartbeat_outsystems_export_blocked"
	// MetricLateRows counts rows a recount found in the logging database
	// behind the watermark that the extraction never read: committed later
	// than the lag (recount.go).  They are not sent.
	MetricLateRows = "heartbeat_outsystems_late_rows_total"
	// MetricDuplicateRows counts rows a recount found delivered more often
	// than the logging database holds them: read twice, or deleted after
	// they were sent.  A batch sent again after the Collector took it is
	// not counted.
	MetricDuplicateRows = "heartbeat_outsystems_duplicate_rows_total"
	// MetricRecounts counts the recounts run, failed ones included.
	MetricRecounts = "heartbeat_outsystems_recounts_total"
	// MetricRecountFailures counts recounts that failed; meanwhile late and
	// duplicate rows go uncounted.
	MetricRecountFailures = "heartbeat_outsystems_recount_failures_total"
	// MetricStageDuration is the time of one step of a batch (see
	// [batchStages]).  Labels: collector, log_type, stage.  Without target,
	// since each series is a histogram and a step's cost hardly differs
	// between the targets of a collector.
	MetricStageDuration = "heartbeat_outsystems_stage_duration_seconds"
	// MetricHeldBytes is the row data held in memory now, read, being
	// exported or pending, against [Executor.MaxHeldBytes].  No labels.
	MetricHeldBytes = "heartbeat_outsystems_held_bytes"
)

// Values of the stage label of [MetricStageDuration].
const (
	// stageRead is the query on the logging database.
	stageRead = "read"
	// stageBuild is turning rows into OTLP log records.
	stageBuild = "build"
	// stageExport is until the OTel Collector accepted the batch, retries
	// and waiting for Loki included.
	stageExport = "export"
	// stageDerive is updating the derived metrics from the delivered rows.
	stageDerive = "derive"
	// stageWatermark is saving the watermark to disk.
	stageWatermark = "watermark"
)

// stageDurationBuckets reach far: an export waits for as long as Loki is down.
var stageDurationBuckets = []float64{0.01, 0.05, 0.1, 0.5, 1, 5, 30, 300}

// metricLabels are the label names of every extraction metric.
var metricLabels = []string{"collector", "environment", "target", "log_type"}

// Metrics records the extraction self-metrics and the derived metrics
// (derived.go), within each collector's series budget.  The self-metric
// series of a collector are created, counters at 0, when its Poller starts;
// every series of a collector is deleted when it stops, which frees its
// budget.  A label value Prometheus rejects skips the update instead of
// panicking.  A nil *Metrics records nothing.  Metrics is safe for
// concurrent use.
type Metrics struct {
	rows            *prometheus.CounterVec
	lag             *prometheus.GaugeVec
	lagSetting      *prometheus.GaugeVec
	exportFailures  *prometheus.CounterVec
	stuck           *prometheus.CounterVec
	logDelay        *prometheus.GaugeVec
	lateRows        *prometheus.CounterVec
	duplicateRows   *prometheus.CounterVec
	recounts        *prometheus.CounterVec
	recountFailures *prometheus.CounterVec
	exportBlocked   *prometheus.GaugeVec
	stages          *prometheus.HistogramVec
	// heldBytes is the executor's, once it exists; see [MetricHeldBytes].
	heldBytes atomic.Pointer[func() int64]
	derived   *derived
}

// NewMetrics creates the extraction metrics and the derived metrics and
// registers them in reg.
func NewMetrics(reg prometheus.Registerer) (*Metrics, error) {
	m := &Metrics{
		rows: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: MetricRows,
			Help: "OutSystems log rows extracted and delivered to the OTel Collector.",
		}, metricLabels),
		lag: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: MetricLag,
			Help: "Seconds the OutSystems log extraction watermark trails now; about lag plus the poll interval when caught up.",
		}, metricLabels),
		lagSetting: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: MetricLagSetting,
			Help: "Seconds behind now the OutSystems log type's reads stop, as configured; the extraction lag never goes below it.",
		}, metricLabels),
		exportFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: MetricExportFailures,
			Help: "OutSystems log batches the OTel Collector did not accept after retries; they are read again.",
		}, metricLabels),
		stuck: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: MetricStuckBatches,
			Help: "Extraction cycles that failed because every row of a full batch had the same Instant; raise batch_limit.",
		}, metricLabels),
		logDelay: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: MetricLogDelay,
			Help: "Seconds between now and the newest OutSystems log Instant extracted, set when rows arrive.",
		}, metricLabels),
		exportBlocked: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: MetricExportBlocked,
			Help: "Whether the OutSystems log type waits for the OTel Collector to accept a batch, its extraction paused (1), or not (0).",
		}, metricLabels),
		lateRows: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: MetricLateRows,
			Help: "OutSystems log rows a recount found behind the watermark that were never read: committed later than the lag. They are not sent.",
		}, metricLabels),
		duplicateRows: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: MetricDuplicateRows,
			Help: "OutSystems log rows a recount found delivered more often than the logging database holds them.",
		}, metricLabels),
		recounts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: MetricRecounts,
			Help: "OutSystems log recounts run, about one a minute per log type, failed ones included.",
		}, metricLabels),
		recountFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: MetricRecountFailures,
			Help: "OutSystems log recounts that failed; late and duplicate rows are not counted meanwhile.",
		}, metricLabels),
		stages: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    MetricStageDuration,
			Help:    "Time of one step of an OutSystems log batch in seconds: read, build, export, derive or watermark.",
			Buckets: stageDurationBuckets,
		}, []string{"collector", "log_type", "stage"}),
		derived: newDerived(),
	}
	held := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: MetricHeldBytes,
		Help: "OutSystems log batches held in memory now, row data and built records: read, being exported or pending.",
	}, func() float64 {
		if read := m.heldBytes.Load(); read != nil {
			return float64((*read)())
		}
		return 0
	})
	for _, collector := range append([]prometheus.Collector{m.rows, m.lag, m.lagSetting, m.exportFailures, m.stuck, m.logDelay, m.exportBlocked, m.lateRows, m.duplicateRows, m.recounts, m.recountFailures, m.stages, held}, m.derived.collectors()...) {
		if err := reg.Register(collector); err != nil {
			return nil, fmt.Errorf("register outsystems metrics: %w", err)
		}
	}
	return m, nil
}

// labelValues returns the label values of item, in metricLabels order.
func labelValues(item collectormetadata.ScheduledProbe) []string {
	return []string{item.CollectorID, item.Target.EnvironmentSlug, item.Target.Name, item.Definition.Name}
}

// initCollector creates the counters of every log type collector extracts at
// 0, so increase() sees the first row and the first failure, its
// export-blocked gauge at 0, and its lag setting.  It also sets the collector's series budget
// for the derived metrics and creates its budget series.
func (m *Metrics) initCollector(collector collectorconfig.CollectorRuntimeConfig) {
	if m == nil || !collector.Enabled {
		return
	}
	m.derived.init(collector)
	for _, target := range collector.Targets {
		if len(collector.TargetNames) > 0 && !slices.Contains(collector.TargetNames, target.Name) {
			continue
		}
		for _, probe := range target.Probes {
			values := []string{collector.ID, target.EnvironmentSlug, target.Name, probe.Name}
			for _, vec := range []*prometheus.CounterVec{m.rows, m.exportFailures, m.stuck, m.lateRows, m.duplicateRows, m.recounts, m.recountFailures} {
				_, _ = vec.GetMetricWithLabelValues(values...)
			}
			_, _ = m.exportBlocked.GetMetricWithLabelValues(values...)
			lag := collector.OutSystemsLogs.Lag
			if probe.Lag > 0 {
				lag = probe.Lag
			}
			if gauge, err := m.lagSetting.GetMetricWithLabelValues(values...); err == nil {
				gauge.Set(lag.Seconds())
			}
		}
	}
}

// forgetCollector deletes every series of collectorID.
func (m *Metrics) forgetCollector(collectorID string) {
	if m == nil {
		return
	}
	labels := prometheus.Labels{"collector": collectorID}
	m.rows.DeletePartialMatch(labels)
	m.lag.DeletePartialMatch(labels)
	m.lagSetting.DeletePartialMatch(labels)
	m.exportFailures.DeletePartialMatch(labels)
	m.stuck.DeletePartialMatch(labels)
	m.logDelay.DeletePartialMatch(labels)
	m.exportBlocked.DeletePartialMatch(labels)
	m.lateRows.DeletePartialMatch(labels)
	m.duplicateRows.DeletePartialMatch(labels)
	m.recounts.DeletePartialMatch(labels)
	m.recountFailures.DeletePartialMatch(labels)
	m.stages.DeletePartialMatch(labels)
	m.derived.forget(collectorID)
}

// tracer starts the spans of a batch's steps.
var tracer = telemetry.Tracer("heartbeat/outsystems")

// stage starts one step of a batch, as a span and for [MetricStageDuration].
// The function it returns ends it, with err marking a failed one.  A nil
// *Metrics still traces.
func (m *Metrics) stage(ctx context.Context, item collectormetadata.ScheduledProbe, stage string) (context.Context, func(err error)) {
	began := time.Now()
	ctx, span := tracer.Start(ctx, "outsystems."+stage, trace.WithAttributes(attribute.String("heartbeat.log_type", item.Definition.Name)))
	return ctx, func(err error) {
		if err != nil {
			// Not the error's text, which can name the host and the login.
			span.SetStatus(codes.Error, stage+" failed")
		}
		span.End()
		m.observeStage(item, stage, began)
	}
}

// observeStage records the time of one step since began.
func (m *Metrics) observeStage(item collectormetadata.ScheduledProbe, stage string, began time.Time) {
	if m == nil {
		return
	}
	if observer, err := m.stages.GetMetricWithLabelValues(item.CollectorID, item.Definition.Name, stage); err == nil {
		observer.Observe(time.Since(began).Seconds())
	}
}

func (m *Metrics) addRows(item collectormetadata.ScheduledProbe, n int) {
	if m == nil {
		return
	}
	if counter, err := m.rows.GetMetricWithLabelValues(labelValues(item)...); err == nil {
		counter.Add(float64(n))
	}
}

// observeRows updates the derived metrics from delivered rows.
func (m *Metrics) observeRows(item collectormetadata.ScheduledProbe, batch Batch, rows [][]any, instants []time.Time) {
	if m == nil {
		return
	}
	m.derived.observe(item, batch, rows, instants)
}

func (m *Metrics) exportFailed(item collectormetadata.ScheduledProbe) {
	if m == nil {
		return
	}
	if counter, err := m.exportFailures.GetMetricWithLabelValues(labelValues(item)...); err == nil {
		counter.Inc()
	}
}

func (m *Metrics) batchStuck(item collectormetadata.ScheduledProbe) {
	if m == nil {
		return
	}
	if counter, err := m.stuck.GetMetricWithLabelValues(labelValues(item)...); err == nil {
		counter.Inc()
	}
}

func (m *Metrics) setLag(item collectormetadata.ScheduledProbe, lag time.Duration) {
	if m == nil {
		return
	}
	if gauge, err := m.lag.GetMetricWithLabelValues(labelValues(item)...); err == nil {
		gauge.Set(lag.Seconds())
	}
}

func (m *Metrics) setExportBlocked(item collectormetadata.ScheduledProbe, blocked bool) {
	if m == nil {
		return
	}
	if gauge, err := m.exportBlocked.GetMetricWithLabelValues(labelValues(item)...); err == nil {
		value := 0.0
		if blocked {
			value = 1
		}
		gauge.Set(value)
	}
}

func (m *Metrics) setLogDelay(item collectormetadata.ScheduledProbe, delay time.Duration) {
	if m == nil {
		return
	}
	if gauge, err := m.logDelay.GetMetricWithLabelValues(labelValues(item)...); err == nil {
		gauge.Set(delay.Seconds())
	}
}

// recounted adds what a recount newly found.
func (m *Metrics) recounted(item collectormetadata.ScheduledProbe, late, duplicate int64) {
	if m == nil {
		return
	}
	for _, c := range []struct {
		vec *prometheus.CounterVec
		n   int64
	}{{m.lateRows, late}, {m.duplicateRows, duplicate}} {
		if counter, err := c.vec.GetMetricWithLabelValues(labelValues(item)...); err == nil && c.n > 0 {
			counter.Add(float64(c.n))
		}
	}
}

func (m *Metrics) recountRan(item collectormetadata.ScheduledProbe) {
	if m == nil {
		return
	}
	if counter, err := m.recounts.GetMetricWithLabelValues(labelValues(item)...); err == nil {
		counter.Inc()
	}
}

func (m *Metrics) recountFailed(item collectormetadata.ScheduledProbe) {
	if m == nil {
		return
	}
	if counter, err := m.recountFailures.GetMetricWithLabelValues(labelValues(item)...); err == nil {
		counter.Inc()
	}
}
