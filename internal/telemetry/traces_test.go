package telemetry

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// recordSpans makes the global tracer provider keep spans in memory.
func recordSpans(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	previous, propagator := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter)))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		otel.SetTextMapPropagator(propagator)
	})
	return exporter
}

func tracedMux() http.Handler {
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	mux.HandleFunc("GET /things/{id}", ok)
	for path := range untracedPaths {
		mux.HandleFunc(path, ok)
	}
	return Traced(mux)
}

func TestTracedNamesTheSpanAfterTheRoute(t *testing.T) {
	spans := recordSpans(t)
	handler := tracedMux()
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/things/42", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/made-up", nil))
	got := spans.GetSpans()
	if len(got) != 2 {
		t.Fatalf("got %d spans, want 2", len(got))
	}
	if got[0].Name != "GET /things/{id}" {
		t.Errorf("span = %q; the ID must not be in the name", got[0].Name)
	}
	if got[1].Name != "GET unmatched" {
		t.Errorf("span = %q; an unknown path must not name a span", got[1].Name)
	}
}

func TestScrapesAndProbesMakeNoSpans(t *testing.T) {
	spans := recordSpans(t)
	handler := tracedMux()
	for path := range untracedPaths {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	if got := spans.GetSpans(); len(got) != 0 {
		t.Errorf("got spans %v", got.Snapshots())
	}
}

// A caller could otherwise have every request recorded by sending a sampled
// traceparent.
func TestClientTraceContextIsLinkedNotContinued(t *testing.T) {
	spans := recordSpans(t)
	req := httptest.NewRequest(http.MethodGet, "/things/42", nil)
	req.Header.Set("traceparent", "00-5b8efff798038103d269b633813fc60c-eee19b7ec3c1b174-01")
	tracedMux().ServeHTTP(httptest.NewRecorder(), req)
	got := spans.GetSpans()
	if len(got) != 1 {
		t.Fatalf("got %d spans, want 1", len(got))
	}
	if got[0].Parent.IsValid() {
		t.Error("the client's span became the parent")
	}
	if len(got[0].Links) != 1 || got[0].Links[0].SpanContext.TraceID().String() != "5b8efff798038103d269b633813fc60c" {
		t.Errorf("links = %v, want the client's trace", got[0].Links)
	}
}

func TestStartTracesSendsOnShutdown(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		requests.Add(1)
	}))
	defer server.Close()
	t.Setenv(EnvSDKDisabled, "")
	t.Setenv(EnvTracesEndpoint, server.URL+"/v1/traces")
	previous := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	stop := startTraces(Identity{Service: "svc"}, prometheus.NewCounter(prometheus.CounterOpts{Name: "e"}), slog.New(slog.DiscardHandler))
	if stop == nil {
		t.Fatal("no tracing with an endpoint set")
	}
	_, span := otel.GetTracerProvider().Tracer("t").Start(context.Background(), "work")
	span.End()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stop(ctx)
	if requests.Load() == 0 {
		t.Error("the span was not sent")
	}
}

func TestNoEndpointOrDisabledStartsNoTracing(t *testing.T) {
	counter := prometheus.NewCounter(prometheus.CounterOpts{Name: "e"})
	logger := slog.New(slog.DiscardHandler)
	t.Setenv(EnvSDKDisabled, "")
	t.Setenv(EnvTracesEndpoint, "")
	if startTraces(Identity{}, counter, logger) != nil {
		t.Error("tracing started without an endpoint")
	}
	t.Setenv(EnvTracesEndpoint, "http://localhost:1/v1/traces")
	t.Setenv(EnvSDKDisabled, "true")
	if startTraces(Identity{}, counter, logger) != nil {
		t.Error("tracing started while disabled")
	}
}

func TestTraceErrorsAreCountedAndWarnedOnce(t *testing.T) {
	counter := prometheus.NewCounter(prometheus.CounterOpts{Name: "e"})
	var out bytes.Buffer
	handler := &traceErrors{count: counter, diagnostics: slog.New(slog.NewJSONHandler(&out, nil))}
	handler.Handle(io.ErrUnexpectedEOF)
	handler.Handle(io.ErrUnexpectedEOF)
	if got := testutil.ToFloat64(counter); got != 2 {
		t.Errorf("errors = %v, want 2", got)
	}
	if got := bytes.Count(out.Bytes(), []byte("tracing error")); got != 1 {
		t.Errorf("%d warnings, want 1", got)
	}
}

func BenchmarkTracedRequestUnsampled(b *testing.B) {
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.NeverSample())))
	handler := tracedMux()
	req := httptest.NewRequest(http.MethodGet, "/things/42", nil)
	b.ReportAllocs()
	for b.Loop() {
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}
}
