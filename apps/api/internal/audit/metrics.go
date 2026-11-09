package audit

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"atrium/apps/api/internal/store"
)

// Outbox metrics (ADR 0016 §4).
const (
	// MetricOutboxRows is the number of records waiting in the outbox, read
	// at each scrape. Every replica reports the same table.
	MetricOutboxRows = "atrium_api_audit_outbox_rows"
	// MetricOutboxOldestAge is the age of the oldest waiting record in
	// seconds, 0 when there is none, read at each scrape.
	MetricOutboxOldestAge = "atrium_api_audit_outbox_oldest_age_seconds"
	// MetricRecordsDelivered counts the records this replica delivered: the
	// OTel Collector accepted them and they left the outbox.
	MetricRecordsDelivered = "atrium_api_audit_records_delivered_total"
)

// statsTimeout bounds the query a scrape runs.
const statsTimeout = 2 * time.Second

var (
	outboxRowsDesc = prometheus.NewDesc(MetricOutboxRows,
		"Audit records waiting in the outbox for the OTel Collector.", nil, nil)
	outboxOldestAgeDesc = prometheus.NewDesc(MetricOutboxOldestAge,
		"Age of the oldest audit record waiting in the outbox, in seconds; 0 when it is empty.", nil, nil)
)

// NewDeliveredCounter returns the counter of delivered records.
func NewDeliveredCounter() prometheus.Counter {
	return prometheus.NewCounter(prometheus.CounterOpts{
		Name: MetricRecordsDelivered,
		Help: "Audit records the OTel Collector accepted, deleted from the outbox.",
	})
}

// OutboxCollector reads the outbox's size and oldest row at each scrape.
// When the query fails, as while PostgreSQL is down, the scrape has neither
// series rather than failing: GET /readyz already reports the database, and
// the other metrics still matter.
type OutboxCollector struct {
	Queries *store.Queries
}

// Describe implements [prometheus.Collector].
func (c OutboxCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- outboxRowsDesc
	ch <- outboxOldestAgeDesc
}

// Collect implements [prometheus.Collector].
func (c OutboxCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), statsTimeout)
	defer cancel()
	stats, err := c.Queries.AuditOutboxStats(ctx)
	if err != nil {
		return
	}
	ch <- prometheus.MustNewConstMetric(outboxRowsDesc, prometheus.GaugeValue, float64(stats.Rows))
	ch <- prometheus.MustNewConstMetric(outboxOldestAgeDesc, prometheus.GaugeValue, max(stats.OldestAgeSeconds, 0))
}
