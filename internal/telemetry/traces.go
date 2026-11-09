package telemetry

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// EnvTracesEndpoint is where spans go, such as
// http://otel-collector:4318/v1/traces; unset sends none and spans cost next to
// nothing. The exporter and the SDK read the other OTEL_* variables themselves,
// OTEL_TRACES_SAMPLER and OTEL_TRACES_SAMPLER_ARG among them.
const EnvTracesEndpoint = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"

// MetricTraceExportErrors counts the errors the tracing SDK reported, mostly
// batches of spans it could not send. No labels.
const MetricTraceExportErrors = "atrium_telemetry_trace_errors_total"

// Tracer returns the tracer of an instrumentation scope. Before [Start] sets up
// tracing, and without it, its spans do nothing.
func Tracer(name string) trace.Tracer {
	return otel.Tracer(name)
}

// startTraces sets up the global tracer provider when the endpoint is set, and
// returns what flushes it.
func startTraces(identity Identity, errors prometheus.Counter, diagnostics *slog.Logger) func(context.Context) {
	if disabled, err := strconv.ParseBool(os.Getenv(EnvSDKDisabled)); (err == nil && disabled) || os.Getenv(EnvTracesEndpoint) == "" {
		return nil
	}
	exporter, err := otlptracehttp.New(context.Background())
	if err != nil {
		diagnostics.Error("telemetry: traces not started", "error", err)
		return nil
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(traceResource(identity)),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	otel.SetErrorHandler(&traceErrors{count: errors, diagnostics: diagnostics})
	return func(ctx context.Context) {
		if err := provider.Shutdown(ctx); err != nil {
			diagnostics.Warn("telemetry: spans not sent before stopping", "error", err)
		}
	}
}

// traceResource is [logResource] for the SDK's types.
func traceResource(identity Identity) *resource.Resource {
	var attrs []attribute.KeyValue
	for _, attr := range []struct{ key, value string }{
		{"service.namespace", Namespace},
		{"service.name", identity.Service},
		{"service.version", identity.Version},
		{"service.instance.id", identity.InstanceID},
		{environmentAttribute, identity.Environment},
	} {
		if attr.value != "" {
			attrs = append(attrs, attribute.String(attr.key, attr.value))
		}
	}
	return resource.NewSchemaless(attrs...)
}

// traceErrors counts what the SDK reports and warns on stderr at most once a
// minute, as the log export does.
type traceErrors struct {
	count       prometheus.Counter
	diagnostics *slog.Logger
	mu          sync.Mutex
	lastWarn    time.Time
}

// Handle implements [otel.ErrorHandler].
func (e *traceErrors) Handle(err error) {
	e.count.Inc()
	e.mu.Lock()
	defer e.mu.Unlock()
	if time.Since(e.lastWarn) >= warnInterval {
		e.lastWarn = time.Now()
		e.diagnostics.Warn("telemetry: tracing error", "error", err)
	}
}

// untracedPaths are the requests that would only bury the others: scrapes and
// kubelet probes.
var untracedPaths = map[string]bool{"/metrics": true, "/healthz": true, "/healthcheck": true, "/readyz": true}

// Traced starts a span for every request next serves but [untracedPaths]. next
// must end in the [http.ServeMux] that routes the request, since the span is
// named after the pattern it matched. A trace context sent by the client is
// linked rather than continued, so that a caller cannot make the service record
// every request by asking to.
func Traced(next http.Handler) http.Handler {
	named := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		method, route := requestLabels(r)
		trace.SpanFromContext(r.Context()).SetName(method + " " + route)
	})
	return otelhttp.NewHandler(named, "http.server",
		otelhttp.WithPublicEndpointFn(func(*http.Request) bool { return true }),
		otelhttp.WithFilter(func(r *http.Request) bool { return !untracedPaths[r.URL.Path] }),
	)
}
