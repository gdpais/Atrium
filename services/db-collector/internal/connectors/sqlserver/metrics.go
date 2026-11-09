package sqlserver

import (
	"database/sql"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Connection pool metrics, read from the handles at each scrape, so the
// series of a target go with its last handle.  Labels: environment, target.
const (
	// MetricConnections is a target's connections by state: in_use or idle.
	// It also has the label state.
	MetricConnections = "atrium_collector_db_connections"
	// MetricConnectionWaits counts the waits for a free connection.
	MetricConnectionWaits = "atrium_collector_db_connection_waits_total"
	// MetricConnectionWaitSeconds is the time spent in those waits.
	MetricConnectionWaitSeconds = "atrium_collector_db_connection_wait_seconds_total"
)

var (
	connectionsDesc = prometheus.NewDesc(MetricConnections,
		"Connections to a monitored database by state (in_use, idle).", []string{"environment", "target", "state"}, nil)
	connectionWaitsDesc = prometheus.NewDesc(MetricConnectionWaits,
		"Waits for a free connection to a monitored database.", []string{"environment", "target"}, nil)
	connectionWaitSecondsDesc = prometheus.NewDesc(MetricConnectionWaitSeconds,
		"Seconds spent waiting for a free connection to a monitored database.", []string{"environment", "target"}, nil)
)

// poolOwner names the target a handle is reported under.  Targets on one
// server and database with one credential share a handle.
type poolOwner struct{ environment, target string }

// reportedOwner is the owner a handle's series carry at now: of the targets
// that used it within ttl, the first in name order, so that targets sharing it
// do not take turns; when none did, the last one.  A renamed target therefore
// takes its series over within ttl.  It forgets owners older than ttl.  p.mu
// must be held.
func (e *poolEntry) reportedOwner(now time.Time, ttl time.Duration) poolOwner {
	var chosen, latest poolOwner
	var found bool
	var latestSeen time.Time
	for owner, seen := range e.owners {
		if seen.After(latestSeen) {
			latest, latestSeen = owner, seen
		}
		if now.Sub(seen) > ttl {
			continue
		}
		if !found || owner.environment < chosen.environment || (owner.environment == chosen.environment && owner.target < chosen.target) {
			chosen, found = owner, true
		}
	}
	for owner, seen := range e.owners {
		if now.Sub(seen) > ttl && owner != latest {
			delete(e.owners, owner)
		}
	}
	if !found {
		return latest
	}
	return chosen
}

// retireLocked keeps the wait counters of entry's handle, which is about to
// close, under its owner.  p.mu must be held.
func (p *pool) retireLocked(entry *poolEntry, now time.Time) {
	owner := entry.reportedOwner(now, p.idleTTL)
	stats := entry.db.Stats()
	total := p.retired[owner]
	total.WaitCount += stats.WaitCount
	total.WaitDuration += stats.WaitDuration
	p.retired[owner] = total
}

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

// stats returns the statistics of every initialised handle by owner, with the
// wait counters of the handles each owner closed.  An owner with no handle left
// is forgotten, and its series with it.
func (p *pool) stats() map[poolOwner][]sql.DBStats {
	p.mu.Lock()
	now := p.now()
	handles := make(map[*sql.DB]poolOwner, len(p.entries))
	live := map[poolOwner]bool{}
	for _, entry := range p.entries {
		select {
		case <-entry.ready:
			if entry.db != nil {
				owner := entry.reportedOwner(now, p.idleTTL)
				handles[entry.db] = owner
				live[owner] = true
			}
		default:
		}
	}
	retired := make(map[poolOwner]sql.DBStats, len(p.retired))
	for owner, stats := range p.retired {
		if !live[owner] {
			delete(p.retired, owner)
			continue
		}
		retired[owner] = stats
	}
	p.mu.Unlock()
	out := make(map[poolOwner][]sql.DBStats, len(handles))
	for db, owner := range handles {
		out[owner] = append(out[owner], db.Stats())
	}
	for owner, stats := range retired {
		out[owner] = append(out[owner], sql.DBStats{WaitCount: stats.WaitCount, WaitDuration: stats.WaitDuration})
	}
	return out
}
