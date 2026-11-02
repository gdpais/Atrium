// Package telemetry is what every Heartbeat service does about its own
// observability (ADR 0018): one identity for its signals, a build-info metric,
// its logs sent over OTLP as well as written to stderr, its traces, and an
// optional pprof listener.
//
// Metrics stay on each service's Prometheus registry; [Telemetry.Register]
// adds this package's series to it. Nothing here may stop a service from
// working: without a Collector it logs to stderr as before, and a slow or
// unreachable Collector costs dropped records, counted, never a blocked caller.
//
// Environment variables, the OpenTelemetry names where one exists:
//
//	OTEL_EXPORTER_OTLP_LOGS_ENDPOINT  where logs go, such as
//	                                  http://otel-collector:4319/v1/logs; unset
//	                                  sends none. The general
//	                                  OTEL_EXPORTER_OTLP_ENDPOINT is not read for
//	                                  logs on purpose: it names the port whose
//	                                  logs pipeline is the OutSystems delivery
//	                                  path, which a service's own logs must not
//	                                  share
//	OTEL_EXPORTER_OTLP_TRACES_ENDPOINT  where spans go; see [EnvTracesEndpoint]
//	OTEL_RESOURCE_ATTRIBUTES          deployment.environment.name is read from it
//	OTEL_SDK_DISABLED                 true turns the OTLP export off
//	HEARTBEAT_PPROF_ADDR              loopback address of the pprof listener; unset
//	                                  runs none
package telemetry

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"

	"heartbeat/internal/otlp"
)

// Environment variables this package reads.
const (
	EnvLogsEndpoint       = "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"
	EnvResourceAttributes = "OTEL_RESOURCE_ATTRIBUTES"
	EnvSDKDisabled        = "OTEL_SDK_DISABLED"
	EnvPprofAddr          = "HEARTBEAT_PPROF_ADDR"
)

// Namespace is the service.namespace of every Heartbeat service.
const Namespace = "heartbeat"

// MetricServiceInfo is always 1 and says what is running. Labels: service,
// version, revision, environment.
//
// environment is here rather than on the scrape job because the collector's
// series already use that label for the monitored target's environment; a
// scrape label of the same name would rename theirs.
const MetricServiceInfo = "heartbeat_service_info"

// environmentAttribute is the resource attribute that names the environment
// the service itself runs in.
const environmentAttribute = "deployment.environment.name"

// version and revision are set at build time, with
// -ldflags "-X heartbeat/internal/telemetry.version=... -X ...revision=...".
var (
	version  = "dev"
	revision = ""
)

// Identity says which service instance a signal comes from, in OpenTelemetry's
// terms.
type Identity struct {
	Service     string
	Version     string
	Revision    string
	InstanceID  string
	Environment string
}

// Telemetry is a running service's telemetry. Create it with [Start]; a nil
// Telemetry is valid, registers nothing and shuts down at once, so tests and
// callers that want none can leave it out.
type Telemetry struct {
	// Logger writes JSON to stderr and, when the export is on, sends the same
	// records over OTLP.
	Logger *slog.Logger
	// Identity is the identity the service's signals carry.
	Identity Identity

	logs    *logQueue
	records *prometheus.CounterVec
	otlp    *otlp.Metrics
	pprof   *http.Server

	traceErrors prometheus.Counter
	stopTraces  func(context.Context)
}

// Start builds the service's logger from the environment and starts what it
// needs. It cannot fail: settings that do not work are logged and left out, so
// that a telemetry mistake never stops the service.
func Start(service string, stderr io.Writer) *Telemetry {
	base := slog.NewJSONHandler(stderr, nil)
	diagnostics := slog.New(base)
	t := &Telemetry{
		Identity: identityFromEnv(service),
		otlp:     otlp.NewMetrics(),
		records: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: MetricLogRecords,
			Help: "Log records the service tried to send over OTLP, by outcome.",
		}, []string{"outcome"}),
		traceErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: MetricTraceExportErrors,
			Help: "Errors the tracing SDK reported, mostly batches of spans it could not send.",
		}),
	}
	for _, outcome := range logOutcomes {
		t.records.WithLabelValues(outcome)
	}
	if endpoint := logsEndpoint(); endpoint != "" {
		t.logs = newLogQueue(endpoint, t.Identity, t.records, t.otlp.Client(selfLogsClient), diagnostics)
		go t.logs.run()
	}
	t.stopTraces = startTraces(t.Identity, t.traceErrors, diagnostics)
	t.Logger = slog.New(&handler{base: base, queue: t.logs})
	t.pprof = startPprof(os.Getenv(EnvPprofAddr), diagnostics)
	return t
}

// Register adds this package's metrics to registerer.
func (t *Telemetry) Register(registerer prometheus.Registerer) error {
	if t == nil {
		return nil
	}
	info := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: MetricServiceInfo,
		Help: "Always 1: the service, version, revision and environment that are running.",
	}, []string{"service", "version", "revision", "environment"})
	info.WithLabelValues(t.Identity.Service, t.Identity.Version, t.Identity.Revision, t.Identity.Environment).Set(1)
	errs := []error{registerer.Register(info), registerer.Register(t.records), registerer.Register(t.traceErrors)}
	for _, collector := range t.otlp.Collectors() {
		errs = append(errs, registerer.Register(collector))
	}
	return errors.Join(errs...)
}

// OTLPClient returns the metrics of an OTLP client of this service, called
// name in the client label.
func (t *Telemetry) OTLPClient(name string) *otlp.ClientMetrics {
	if t == nil {
		return nil
	}
	return t.otlp.Client(name)
}

// Shutdown sends the logs and spans still queued and stops the pprof listener,
// waiting until ctx ends at most. Records that do not make it are counted as
// dropped. The logs go first, then the spans, which may be lost: spans the
// Collector does not take are retried until ctx ends. Both go before the
// listener: a CPU profile in flight holds it for its whole duration, and would
// spend all of ctx.
func (t *Telemetry) Shutdown(ctx context.Context) {
	if t == nil {
		return
	}
	if t.logs != nil {
		t.logs.shutdown(ctx)
	}
	if t.stopTraces != nil {
		t.stopTraces(ctx)
	}
	if t.pprof != nil {
		_ = t.pprof.Shutdown(ctx)
	}
}

// identityFromEnv reads the identity of service from the environment and the
// build.
func identityFromEnv(service string) Identity {
	instance, _ := os.Hostname()
	return Identity{
		Service:     service,
		Version:     version,
		Revision:    buildRevision(),
		InstanceID:  instance,
		Environment: resourceAttribute(os.Getenv(EnvResourceAttributes), environmentAttribute),
	}
}

// buildRevision returns the revision set at build time, or the one the Go
// toolchain stamped when built from a checkout.
func buildRevision() string {
	if revision != "" {
		return revision
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			return setting.Value
		}
	}
	return ""
}

// resourceAttribute returns one attribute of an OTEL_RESOURCE_ATTRIBUTES
// value: comma-separated key=value pairs with percent-encoded values.
func resourceAttribute(attributes, key string) string {
	for _, pair := range strings.Split(attributes, ",") {
		k, v, ok := strings.Cut(pair, "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		if decoded, err := url.PathUnescape(strings.TrimSpace(v)); err == nil {
			return decoded
		}
		return strings.TrimSpace(v)
	}
	return ""
}

// logsEndpoint returns the OTLP/HTTP base URL logs go to, or "" when the export
// is off.
func logsEndpoint() string {
	if disabled, err := strconv.ParseBool(os.Getenv(EnvSDKDisabled)); err == nil && disabled {
		return ""
	}
	return strings.TrimSuffix(strings.TrimRight(os.Getenv(EnvLogsEndpoint), "/"), "/v1/logs")
}
