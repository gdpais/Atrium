package app

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"heartbeat/apps/api/internal/httpapi"
	"heartbeat/apps/web"
	"heartbeat/internal/telemetry"
)

// The UI is served at / beside the API (ADR 0020); every pattern of the API
// is more specific than the UI's, so the API still answers its own paths.
func TestRoutesServeTheUIBesideTheAPI(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics, err := telemetry.NewHTTPMetrics(registry, MetricHTTPRequests, MetricHTTPRequestDuration)
	if err != nil {
		t.Fatal(err)
	}
	handler := routes(registry, metrics, mustServer(t, httpapi.Options{ReadinessTimeout: time.Second}))
	const browser = "text/html,*/*;q=0.8"
	for _, tc := range []struct {
		name, method, path string
		code               int
		ui                 bool
	}{
		{"the UI at /", "GET", "/", 200, true},
		{"a client route", "GET", "/environments/abc", 200, true},
		{"liveness", "GET", "/healthz", 200, false},
		{"metrics", "GET", "/metrics", 200, false},
		{"an API operation, without a session", "GET", "/api/v1/session", 401, false},
		{"an unknown API path", "GET", "/api/v1/nothing", 404, true},
		{"logging in from no origin", "POST", "/api/v1/session", 403, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			r.Header.Set("Accept", browser)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.code {
				t.Fatalf("code %d, want %d; body %q", w.Code, tc.code, w.Body)
			}
			if got := w.Header().Get("Content-Security-Policy") == web.ContentSecurityPolicy; got != tc.ui {
				t.Errorf("served by the UI: %v, want %v", got, tc.ui)
			}
			if tc.ui && tc.code == 200 && !strings.Contains(w.Body.String(), "<html") && !strings.Contains(w.Body.String(), `<div id="root">`) {
				t.Errorf("body %q, want a page", w.Body)
			}
		})
	}
}
