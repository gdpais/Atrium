package app

import (
	"errors"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	promcollectors "github.com/prometheus/client_golang/prometheus/collectors"

	atriumconfig "atrium/internal/config"
	"atrium/services/db-collector/internal/collectors"
)

// newRegistry returns the collector's Prometheus registry with Go runtime
// (go_*) and process (process_*) metrics registered, and the per-probe
// duration and error metrics the Runner records.
func newRegistry() (*prometheus.Registry, *collectors.ProbeMetrics, error) {
	registry := prometheus.NewRegistry()
	if err := registry.Register(promcollectors.NewGoCollector()); err != nil {
		return nil, nil, err
	}
	if err := registry.Register(promcollectors.NewProcessCollector(promcollectors.ProcessCollectorOpts{})); err != nil {
		return nil, nil, err
	}
	probeMetrics, err := collectors.NewProbeMetrics(registry)
	if err != nil {
		return nil, nil, err
	}
	return registry, probeMetrics, nil
}

// HTTP request metrics of the collector's own endpoints, for every request
// they serve, /metrics and the health endpoints included.
const (
	// MetricHTTPRequests counts requests by their response's status code.
	// Labels: method, route, code.
	MetricHTTPRequests = "atrium_collector_http_requests_total"
	// MetricHTTPRequestDuration is the time from receiving a request to its
	// handler returning. Labels: method, route.
	MetricHTTPRequestDuration = "atrium_collector_http_request_duration_seconds"
)

// Config load and reload metrics, maintained by [configMetrics].  The startup
// load counts as the first reload: the collector exits when it fails, so a
// running collector always starts with a successful one.
const (
	// MetricConfigLastReloadSuccessful is 1 when the last load or reload
	// succeeded and 0 when it was rejected or failed to apply, until the next
	// successful one.  It is 0 exactly while GET /admin/config shows a
	// last_reload_err.  No labels.
	MetricConfigLastReloadSuccessful = "atrium_collector_config_last_reload_successful"
	// MetricConfigLastReloadSuccessTimestamp is the Unix time of the last
	// successful load or reload: when the active config was loaded.  No
	// labels.
	MetricConfigLastReloadSuccessTimestamp = "atrium_collector_config_last_reload_success_timestamp_seconds"
	// MetricConfigReloads counts loads and reloads by result (see
	// [reloadResults]).  Labels: result.
	MetricConfigReloads = "atrium_collector_config_reloads_total"
	// MetricConfigInfo is always 1 and names the active config version, the
	// SHA-256 of the file, in its version label.  It has one series, which
	// changes only when a reload activates a different file.
	MetricConfigInfo = "atrium_collector_config_info"
)

// Values of the result label of [MetricConfigReloads], and of the result
// field of POST /admin/config/reload.
const (
	// reloadOK is a config that was loaded and applied.
	reloadOK = "ok"
	// reloadInvalidCandidate is a file that could not be read or failed
	// validation.  Nothing was applied.
	reloadInvalidCandidate = "invalid_candidate"
	// reloadApplyFailed is a valid file whose collectors could not be
	// reconciled; the runtime was rolled back, or diverged.
	reloadApplyFailed = "apply_failed"
	// reloadBusy is a reload that gave up waiting for another one to finish.
	// Nothing was read, so the last result stands.
	reloadBusy = "busy"
)

// reloadResults lists every result label value, in a stable order.
var reloadResults = []string{reloadOK, reloadInvalidCandidate, reloadApplyFailed, reloadBusy}

// reloadResult classifies the error of a reload as one of [reloadResults].
func reloadResult(err error) string {
	switch {
	case err == nil:
		return reloadOK
	case errors.Is(err, atriumconfig.ErrInvalidCandidate):
		return reloadInvalidCandidate
	case errors.Is(err, atriumconfig.ErrReloadBusy):
		return reloadBusy
	default:
		return reloadApplyFailed
	}
}

// configMetrics exports the outcome of config loads and reloads.  The gauges
// and the info metric are read from the config manager's snapshot when
// Prometheus scrapes, so they always agree with GET /admin/config; the
// counter is incremented by [configMetrics.observe].  A nil *configMetrics
// records nothing.
type configMetrics struct {
	reloads *prometheus.CounterVec
}

// newConfigMetrics registers the config metrics in reg, reading the active
// snapshot from snapshot.  The reload counter starts at 0 for every result,
// so rate() and increase() see the first failure.
func newConfigMetrics(reg prometheus.Registerer, snapshot func() atriumconfig.Snapshot) (*configMetrics, error) {
	m := &configMetrics{reloads: prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: MetricConfigReloads,
		Help: "Config loads and reloads by result (ok, invalid_candidate, apply_failed, busy); the startup load counts as one ok.",
	}, []string{"result"})}
	for _, result := range reloadResults {
		m.reloads.WithLabelValues(result)
	}
	for _, collector := range []prometheus.Collector{m.reloads, snapshotCollector{snapshot: snapshot}} {
		if err := reg.Register(collector); err != nil {
			return nil, fmt.Errorf("register config metrics: %w", err)
		}
	}
	return m, nil
}

// observe counts one load or reload that returned err.
func (m *configMetrics) observe(err error) {
	if m == nil {
		return
	}
	m.reloads.WithLabelValues(reloadResult(err)).Inc()
}

var (
	lastReloadSuccessfulDesc = prometheus.NewDesc(MetricConfigLastReloadSuccessful,
		"1 if the last config load or reload succeeded, 0 if it was rejected or failed to apply; the last good config keeps running.", nil, nil)
	lastReloadSuccessTimestampDesc = prometheus.NewDesc(MetricConfigLastReloadSuccessTimestamp,
		"Unix time of the last successful config load or reload.", nil, nil)
	configInfoDesc = prometheus.NewDesc(MetricConfigInfo,
		"Always 1; version is the SHA-256 of the active config file.", []string{"version"}, nil)
)

// snapshotCollector exports the gauges derived from the active snapshot.
type snapshotCollector struct {
	snapshot func() atriumconfig.Snapshot
}

// Describe implements [prometheus.Collector].
func (c snapshotCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- lastReloadSuccessfulDesc
	ch <- lastReloadSuccessTimestampDesc
	ch <- configInfoDesc
}

// Collect implements [prometheus.Collector].  A failed reload sets
// LastReloadErr and keeps LoadedAt and the config of the snapshot it did not
// replace; the next successful one clears it.
func (c snapshotCollector) Collect(ch chan<- prometheus.Metric) {
	snapshot := c.snapshot()
	successful := 1.0
	if snapshot.LastReloadErr != "" {
		successful = 0
	}
	ch <- prometheus.MustNewConstMetric(lastReloadSuccessfulDesc, prometheus.GaugeValue, successful)
	ch <- prometheus.MustNewConstMetric(lastReloadSuccessTimestampDesc, prometheus.GaugeValue, float64(snapshot.LoadedAt.UnixNano())/1e9)
	ch <- prometheus.MustNewConstMetric(configInfoDesc, prometheus.GaugeValue, 1, snapshot.Config.Version)
}
