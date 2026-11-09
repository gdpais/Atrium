package app

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	promcollectors "github.com/prometheus/client_golang/prometheus/collectors"
	goredis "github.com/redis/go-redis/v9"

	"atrium/internal/telemetry"
)

// HTTP request metrics, recorded by [telemetry.HTTPMetrics] for every request the
// API serves, /metrics and the health endpoints included.
const (
	// MetricHTTPRequests counts requests by their response's status code.
	// Labels: method, route, code.
	MetricHTTPRequests = "atrium_api_http_requests_total"
	// MetricHTTPRequestDuration is the time from receiving a request to its
	// handler returning, with the Prometheus client's default buckets.
	// Labels: method, route.
	MetricHTTPRequestDuration = "atrium_api_http_request_duration_seconds"
)

// newRegistry returns the API's Prometheus registry with Go runtime (go_*)
// and process (process_*) metrics registered, and the HTTP request metrics.
func newRegistry() (*prometheus.Registry, *telemetry.HTTPMetrics, error) {
	registry := prometheus.NewRegistry()
	if err := registry.Register(promcollectors.NewGoCollector()); err != nil {
		return nil, nil, err
	}
	if err := registry.Register(promcollectors.NewProcessCollector(promcollectors.ProcessCollectorOpts{})); err != nil {
		return nil, nil, err
	}
	metrics, err := telemetry.NewHTTPMetrics(registry, MetricHTTPRequests, MetricHTTPRequestDuration)
	if err != nil {
		return nil, nil, err
	}
	return registry, metrics, nil
}

// Connection pool metrics, read from the pools at each scrape.
const (
	// MetricDBConnections is the PostgreSQL pool's connections by state:
	// in_use, idle or constructing.
	MetricDBConnections = "atrium_api_db_connections"
	// MetricDBConnectionsMax is the most connections the pool opens.
	MetricDBConnectionsMax = "atrium_api_db_connections_max"
	// MetricDBConnectionWaits counts acquires that found no idle connection.
	MetricDBConnectionWaits = "atrium_api_db_connection_waits_total"
	// MetricDBConnectionWaitSeconds is the time those acquires waited.
	MetricDBConnectionWaitSeconds = "atrium_api_db_connection_wait_seconds_total"
	// MetricRedisConnections is the Redis pool's connections by state: in_use
	// or idle.
	MetricRedisConnections = "atrium_api_redis_connections"
	// MetricRedisConnectionTimeouts counts commands that gave up waiting for
	// a connection from the pool.
	MetricRedisConnectionTimeouts = "atrium_api_redis_connection_timeouts_total"
)

var (
	dbConnectionsDesc = prometheus.NewDesc(MetricDBConnections,
		"PostgreSQL pool connections by state (in_use, idle, constructing).", []string{"state"}, nil)
	dbConnectionsMaxDesc = prometheus.NewDesc(MetricDBConnectionsMax,
		"The most connections the PostgreSQL pool opens.", nil, nil)
	dbConnectionWaitsDesc = prometheus.NewDesc(MetricDBConnectionWaits,
		"PostgreSQL pool acquires that found no idle connection.", nil, nil)
	dbConnectionWaitSecondsDesc = prometheus.NewDesc(MetricDBConnectionWaitSeconds,
		"Seconds PostgreSQL pool acquires waited for a connection.", nil, nil)
	redisConnectionsDesc = prometheus.NewDesc(MetricRedisConnections,
		"Redis pool connections by state (in_use, idle).", []string{"state"}, nil)
	redisConnectionTimeoutsDesc = prometheus.NewDesc(MetricRedisConnectionTimeouts,
		"Redis commands that gave up waiting for a connection from the pool.", nil, nil)
)

// poolCollector reads the connection pools at each scrape.
type poolCollector struct {
	postgres func() *pgxpool.Stat
	redis    func() *goredis.PoolStats
}

// Describe implements [prometheus.Collector].
func (c poolCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range []*prometheus.Desc{
		dbConnectionsDesc, dbConnectionsMaxDesc, dbConnectionWaitsDesc, dbConnectionWaitSecondsDesc,
		redisConnectionsDesc, redisConnectionTimeoutsDesc,
	} {
		ch <- desc
	}
}

// Collect implements [prometheus.Collector].
func (c poolCollector) Collect(ch chan<- prometheus.Metric) {
	db := c.postgres()
	ch <- prometheus.MustNewConstMetric(dbConnectionsDesc, prometheus.GaugeValue, float64(db.AcquiredConns()), "in_use")
	ch <- prometheus.MustNewConstMetric(dbConnectionsDesc, prometheus.GaugeValue, float64(db.IdleConns()), "idle")
	ch <- prometheus.MustNewConstMetric(dbConnectionsDesc, prometheus.GaugeValue, float64(db.ConstructingConns()), "constructing")
	ch <- prometheus.MustNewConstMetric(dbConnectionsMaxDesc, prometheus.GaugeValue, float64(db.MaxConns()))
	ch <- prometheus.MustNewConstMetric(dbConnectionWaitsDesc, prometheus.CounterValue, float64(db.EmptyAcquireCount()))
	ch <- prometheus.MustNewConstMetric(dbConnectionWaitSecondsDesc, prometheus.CounterValue, db.EmptyAcquireWaitTime().Seconds())
	cache := c.redis()
	idle := float64(cache.IdleConns)
	ch <- prometheus.MustNewConstMetric(redisConnectionsDesc, prometheus.GaugeValue, float64(cache.TotalConns)-idle, "in_use")
	ch <- prometheus.MustNewConstMetric(redisConnectionsDesc, prometheus.GaugeValue, idle, "idle")
	ch <- prometheus.MustNewConstMetric(redisConnectionTimeoutsDesc, prometheus.CounterValue, float64(cache.Timeouts))
}
