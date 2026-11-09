package httpapi

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newServer returns NewServer(opts), and fails the test on its error.
func newServer(t *testing.T, opts Options) *Server {
	t.Helper()
	server, err := NewServer(opts)
	if err != nil {
		t.Fatal(err)
	}
	return server
}

// serve sends one request to a mux with the server's operations registered.
func serve(t *testing.T, server *Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	server.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func passing(name string) Check {
	return Check{Name: name, Run: func(context.Context) error { return nil }}
}

func failing(name, err string) Check {
	return Check{Name: name, Run: func(context.Context) error { return errors.New(err) }}
}

func TestHealthzIsAlwaysOK(t *testing.T) {
	// Liveness ignores the dependencies: a failing one must not restart the
	// pod.
	server := newServer(t, Options{Checks: []Check{failing("postgres", "connection refused")}, ReadinessTimeout: time.Second})
	rec := serve(t, server, http.MethodGet, "/healthz")
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("GET /healthz = %d %q, want 200 \"ok\"", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain" {
		t.Fatalf("Content-Type = %q, want text/plain", got)
	}
}

func TestReadyz(t *testing.T) {
	for _, tc := range []struct {
		name   string
		checks []Check
		code   int
		body   string
	}{
		{"no checks", nil, http.StatusOK, `{"status":"ready"}`},
		{"every check passes", []Check{passing("postgres"), passing("redis")}, http.StatusOK, `{"status":"ready"}`},
		{"one check fails", []Check{passing("postgres"), failing("redis", "dial tcp 10.0.0.7:6379: connection refused")}, http.StatusServiceUnavailable, `{"status":"not_ready"}`},
		{"every check fails", []Check{failing("postgres", "password authentication failed for user atrium"), failing("redis", "NOAUTH")}, http.StatusServiceUnavailable, `{"status":"not_ready"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(t, newServer(t, Options{Checks: tc.checks, ReadinessTimeout: time.Second}), http.MethodGet, "/readyz")
			if rec.Code != tc.code {
				t.Fatalf("GET /readyz = %d, want %d", rec.Code, tc.code)
			}
			// The endpoint is unauthenticated: the body is the status
			// alone, never a check's name or error text.
			if body := strings.TrimSpace(rec.Body.String()); body != tc.body {
				t.Fatalf("GET /readyz body = %s, want %s", body, tc.body)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", got)
			}
		})
	}
}

func TestReadyzRunsChecksConcurrently(t *testing.T) {
	// Each check waits for the other to start: run one after the other, the
	// first would wait until the timeout and the pod would be unready.
	started := []chan struct{}{make(chan struct{}), make(chan struct{})}
	waitFor := func(self, other int) Check {
		return Check{Name: "check", Run: func(ctx context.Context) error {
			close(started[self])
			select {
			case <-started[other]:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}
	}
	server := newServer(t, Options{Checks: []Check{waitFor(0, 1), waitFor(1, 0)}, ReadinessTimeout: 5 * time.Second})
	if rec := serve(t, server, http.MethodGet, "/readyz"); rec.Code != http.StatusOK {
		t.Fatalf("GET /readyz = %d, want 200", rec.Code)
	}
}

func TestReadyzAnswersWithinTheTimeout(t *testing.T) {
	// A check that ignores its context must not hold every kubelet probe:
	// the timeout answers 503 without waiting for it.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	hung := Check{Name: "redis", Run: func(context.Context) error { <-release; return nil }}
	server := newServer(t, Options{Checks: []Check{passing("postgres"), hung}, ReadinessTimeout: 100 * time.Millisecond})
	start := time.Now()
	rec := serve(t, server, http.MethodGet, "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz = %d, want 503", rec.Code)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("GET /readyz took %s with a 100ms timeout", elapsed)
	}
}

func TestReadyzChecksGetTheDeadline(t *testing.T) {
	var deadline bool
	check := Check{Name: "postgres", Run: func(ctx context.Context) error {
		_, deadline = ctx.Deadline()
		return nil
	}}
	if rec := serve(t, newServer(t, Options{Checks: []Check{check}, ReadinessTimeout: time.Second}), http.MethodGet, "/readyz"); rec.Code != http.StatusOK {
		t.Fatalf("GET /readyz = %d, want 200", rec.Code)
	}
	if !deadline {
		t.Fatal("the check ran without the readiness deadline")
	}
}

func TestReadinessLogsOnlyChangesWithTheReasons(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	healthy := true
	check := Check{Name: "postgres", Run: func(context.Context) error {
		if healthy {
			return nil
		}
		return errors.New("connection refused")
	}}
	server := newServer(t, Options{Logger: logger, Checks: []Check{check}, ReadinessTimeout: time.Second})
	for _, step := range []struct {
		healthy bool
		code    int
		log     string // "" when nothing is logged
	}{
		{true, http.StatusOK, "level=INFO msg=\"readiness changed\" status=ready"},
		{true, http.StatusOK, ""},
		{false, http.StatusServiceUnavailable, "level=WARN msg=\"readiness changed\" status=not_ready reasons=\"[postgres: connection refused]\""},
		{false, http.StatusServiceUnavailable, ""},
		{true, http.StatusOK, "level=INFO msg=\"readiness changed\" status=ready"},
	} {
		healthy = step.healthy
		logs.Reset()
		if rec := serve(t, server, http.MethodGet, "/readyz"); rec.Code != step.code {
			t.Fatalf("healthy=%t: GET /readyz = %d, want %d", step.healthy, rec.Code, step.code)
		}
		got := logs.String()
		if step.log == "" && got != "" {
			t.Fatalf("healthy=%t: logged %q without a change", step.healthy, got)
		}
		if step.log != "" && !strings.Contains(got, step.log) {
			t.Fatalf("healthy=%t: logged %q, want %q", step.healthy, got, step.log)
		}
	}
}

func TestOperationsKeepTheirMethods(t *testing.T) {
	server := newServer(t, Options{Checks: nil, ReadinessTimeout: time.Second})
	for _, path := range []string{"/healthz", "/readyz"} {
		if rec := serve(t, server, http.MethodHead, path); rec.Code != http.StatusOK {
			t.Fatalf("HEAD %s = %d, want 200 (GET routes answer HEAD)", path, rec.Code)
		}
		if rec := serve(t, server, http.MethodPost, path); rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s = %d, want 405", path, rec.Code)
		}
	}
}
