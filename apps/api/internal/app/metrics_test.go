package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"heartbeat/apps/api/internal/httpapi"
	"heartbeat/internal/telemetry"
)

func TestRequestMetricsLabelRequestsByRoute(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics, err := telemetry.NewHTTPMetrics(registry, MetricHTTPRequests, MetricHTTPRequestDuration)
	if err != nil {
		t.Fatal(err)
	}
	failing := httpapi.Check{Name: "postgres", Run: func(context.Context) error { return errors.New("down") }}
	handler := routes(registry, metrics, mustServer(t, httpapi.Options{Checks: []httpapi.Check{failing}, ReadinessTimeout: time.Second}))
	for _, req := range []struct{ method, path string }{
		{http.MethodGet, "/healthz"},
		{http.MethodGet, "/healthz"},
		{http.MethodHead, "/healthz"},
		{http.MethodGet, "/readyz"},
		{http.MethodPost, "/readyz"},
		{http.MethodGet, "/metrics"},
		// A scan of made-up paths and methods lands in two routes: the UI's
		// pattern, GET /, matches every path, so a made-up GET is the UI's
		// (404, or ServeMux's 307 to the cleaned path) and any other method
		// is unmatched (405).
		{http.MethodGet, "/wp-login.php"},
		{http.MethodGet, "/api/v1/../../etc/passwd"},
		{"PROPFIND", "/healthz"},
		{"BREW", "/coffee"},
	} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(req.method, req.path, nil))
	}

	want := `
# HELP heartbeat_api_http_requests_total HTTP requests served, by method, route and status code.
# TYPE heartbeat_api_http_requests_total counter
heartbeat_api_http_requests_total{code="200",method="GET",route="/healthz"} 2
heartbeat_api_http_requests_total{code="200",method="HEAD",route="/healthz"} 1
heartbeat_api_http_requests_total{code="503",method="GET",route="/readyz"} 1
heartbeat_api_http_requests_total{code="405",method="POST",route="unmatched"} 1
heartbeat_api_http_requests_total{code="200",method="GET",route="/metrics"} 1
heartbeat_api_http_requests_total{code="404",method="GET",route="/"} 1
heartbeat_api_http_requests_total{code="307",method="GET",route="/"} 1
heartbeat_api_http_requests_total{code="405",method="other",route="unmatched"} 2
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(want), MetricHTTPRequests); err != nil {
		t.Fatal(err)
	}
	if got, err := testutil.GatherAndCount(registry, MetricHTTPRequestDuration); err != nil || got != 7 {
		t.Fatalf("%s has %d series (%v), want 7 (method and route pairs)", MetricHTTPRequestDuration, got, err)
	}
}

// mustServer returns httpapi.NewServer(opts), and fails the test on its
// error.
func mustServer(t *testing.T, opts httpapi.Options) *httpapi.Server {
	t.Helper()
	server, err := httpapi.NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	return server
}
