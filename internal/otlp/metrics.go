package otlp

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Export metrics, one set per process, labelled by the client that sends.
const (
	// MetricExportAttempts counts HTTP attempts by outcome (see
	// [exportOutcomes]). Labels: client, outcome.
	MetricExportAttempts = "heartbeat_otlp_export_attempts_total"
	// MetricExportAttemptDuration is the time of one HTTP attempt, whatever
	// its outcome. Labels: client.
	MetricExportAttemptDuration = "heartbeat_otlp_export_attempt_duration_seconds"
	// MetricExportBytes counts the compressed bytes of accepted requests.
	// Labels: client.
	MetricExportBytes = "heartbeat_otlp_export_bytes_total"
	// MetricExportRejectedRecords counts the records the receiver refused
	// inside requests it accepted (OTLP partial success). Retrying such a
	// request is not allowed, so these records are lost. Labels: client.
	MetricExportRejectedRecords = "heartbeat_otlp_export_rejected_records_total"
)

// Values of the outcome label of [MetricExportAttempts].
const (
	// outcomeAccepted is a request the receiver took.
	outcomeAccepted = "accepted"
	// outcomeRetryable is a network error, timeout, 429 or 5xx.
	outcomeRetryable = "retryable"
	// outcomeRejected is a status retrying cannot fix.
	outcomeRejected = "rejected"
)

// exportOutcomes lists every outcome label value, in a stable order.
var exportOutcomes = []string{outcomeAccepted, outcomeRetryable, outcomeRejected}

// exportDurationBuckets reach past [DefaultRequestTimeout]: a Collector that
// waits for Loki answers late by design, and 10 s is an attempt that gave up.
var exportDurationBuckets = []float64{0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30}

// Metrics holds a process's export metrics, unregistered until
// [Metrics.Collectors] is handed to a registry. Each client gets a
// [ClientMetrics] from it.
type Metrics struct {
	attempts *prometheus.CounterVec
	duration *prometheus.HistogramVec
	bytes    *prometheus.CounterVec
	rejected *prometheus.CounterVec
}

// NewMetrics returns the export metrics.
func NewMetrics() *Metrics {
	return &Metrics{
		attempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: MetricExportAttempts,
			Help: "OTLP/HTTP export attempts, by client and outcome (accepted, retryable, rejected).",
		}, []string{"client", "outcome"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    MetricExportAttemptDuration,
			Help:    "Time of one OTLP/HTTP export attempt in seconds, whatever its outcome.",
			Buckets: exportDurationBuckets,
		}, []string{"client"}),
		bytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: MetricExportBytes,
			Help: "Compressed bytes of the OTLP/HTTP requests the receiver accepted.",
		}, []string{"client"}),
		rejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: MetricExportRejectedRecords,
			Help: "Log records the receiver refused inside accepted OTLP/HTTP requests (partial success); they are lost.",
		}, []string{"client"}),
	}
}

// Collectors returns what a registry must register.
func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{m.attempts, m.duration, m.bytes, m.rejected}
}

// Client returns the metrics of the client called name, with its counters at
// 0, so rate() and increase() see its first attempt. A nil *Metrics returns
// nil, which records nothing.
func (m *Metrics) Client(name string) *ClientMetrics {
	if m == nil {
		return nil
	}
	c := &ClientMetrics{
		accepted:  m.attempts.WithLabelValues(name, outcomeAccepted),
		retryable: m.attempts.WithLabelValues(name, outcomeRetryable),
		rejected:  m.attempts.WithLabelValues(name, outcomeRejected),
		duration:  m.duration.WithLabelValues(name),
		bytes:     m.bytes.WithLabelValues(name),
		records:   m.rejected.WithLabelValues(name),
	}
	return c
}

// ClientMetrics records one client's attempts. A nil *ClientMetrics records
// nothing.
type ClientMetrics struct {
	accepted, retryable, rejected prometheus.Counter
	duration                      prometheus.Observer
	bytes                         prometheus.Counter
	// records counts the records refused by partial success.
	records prometheus.Counter
}

// rejectRecords records n records refused inside an accepted request.
func (c *ClientMetrics) rejectRecords(n int64) {
	if c == nil {
		return
	}
	c.records.Add(float64(n))
}

// observe records one attempt that sent size bytes and ended with err.  An
// attempt the caller cancelled, as at shutdown, says nothing about the
// receiver and is not recorded.
func (c *ClientMetrics) observe(err error, elapsed time.Duration, size int) {
	if c == nil || errors.Is(err, context.Canceled) {
		return
	}
	c.duration.Observe(elapsed.Seconds())
	var permanent *permanentError
	switch {
	case err == nil:
		c.accepted.Inc()
		c.bytes.Add(float64(size))
	case errors.As(err, &permanent):
		c.rejected.Inc()
	default:
		c.retryable.Inc()
	}
}
