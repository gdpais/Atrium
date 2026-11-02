package sqlserver

import (
	"database/sql"

	"github.com/prometheus/client_golang/prometheus"
)

// Connection pool metrics, read from the handles at each scrape, so the
// series of a target go with its last handle.  Labels: environment, target.
const (
	// MetricConnections is a target's connections by state: in_use or idle.
	// It also has the label state.
	MetricConnections = "heartbeat_collector_db_connections"
	// MetricConnectionWaits counts the waits for a free connection.
	MetricConnectionWaits = "heartbeat_collector_db_connection_waits_total"
	// MetricConnectionWaitSeconds is the time spent in those waits.
	MetricConnectionWaitSeconds = "heartbeat_collector_db_connection_wait_seconds_total"
)

var (
	connectionsDesc = prometheus.NewDesc(MetricConnections,
		"Connections to a monitored database by state (in_use, idle).", []string{"environment", "target", "state"}, nil)
	connectionWaitsDesc = prometheus.NewDesc(MetricConnectionWaits,
		"Waits for a free connection to a monitored database.", []string{"environment", "target"}, nil)
	connectionWaitSecondsDesc = prometheus.NewDesc(MetricConnectionWaitSeconds,
		"Seconds spent waiting for a free connection to a monitored database.", []string{"environment", "target"}, nil)
)

// poolOwner names the target a handle is reported under.  Targets that point
// at one server with one login share a handle, reported under whichever opened
// it first.
type poolOwner struct{ environment, target string }

// poolCollector reads a Manager's handles at each scrape.
type poolCollector struct{ pool *pool }

// Collector returns the Prometheus collector of the Manager's connection pool,
// or nil for a Manager without one.
func (m Manager) Collector() prometheus.Collector {
	if m.pool == nil {
		return nil
	}
	return poolCollector{pool: m.pool}
}

// Describe implements [prometheus.Collector].
func (c poolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- connectionsDesc
	ch <- connectionWaitsDesc
	ch <- connectionWaitSecondsDesc
}

// Collect implements [prometheus.Collector].  A target with several handles,
// as while a rotated credential replaces the old one, reports their sum: a
// registry rejects two series with the same labels.
func (c poolCollector) Collect(ch chan<- prometheus.Metric) {
	totals := map[poolOwner]sql.DBStats{}
	for owner, stats := range c.pool.stats() {
		total := totals[owner]
		for _, s := range stats {
			total.InUse += s.InUse
			total.Idle += s.Idle
			total.WaitCount += s.WaitCount
			total.WaitDuration += s.WaitDuration
		}
		totals[owner] = total
	}
	for owner, total := range totals {
		ch <- prometheus.MustNewConstMetric(connectionsDesc, prometheus.GaugeValue, float64(total.InUse), owner.environment, owner.target, "in_use")
		ch <- prometheus.MustNewConstMetric(connectionsDesc, prometheus.GaugeValue, float64(total.Idle), owner.environment, owner.target, "idle")
		ch <- prometheus.MustNewConstMetric(connectionWaitsDesc, prometheus.CounterValue, float64(total.WaitCount), owner.environment, owner.target)
		ch <- prometheus.MustNewConstMetric(connectionWaitSecondsDesc, prometheus.CounterValue, total.WaitDuration.Seconds(), owner.environment, owner.target)
	}
}

// stats returns the statistics of every initialised handle, by owner.
func (p *pool) stats() map[poolOwner][]sql.DBStats {
	p.mu.Lock()
	handles := make(map[*sql.DB]poolOwner, len(p.entries))
	for _, entry := range p.entries {
		select {
		case <-entry.ready:
			if entry.db != nil {
				handles[entry.db] = entry.owner
			}
		default:
		}
	}
	p.mu.Unlock()
	out := make(map[poolOwner][]sql.DBStats, len(handles))
	for db, owner := range handles {
		out[owner] = append(out[owner], db.Stats())
	}
	return out
}
