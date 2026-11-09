package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	atriumconfig "atrium/internal/config"
	"atrium/services/db-collector/internal/collectors"
)

func TestRoutesExposeHealthzAndCompatibilityHealthcheck(t *testing.T) {
	configManager, _ := newTestConfigManager(t)
	svc := newTestService(t, configManager, newFakePollers().run)
	handler := routes(prometheus.NewRegistry(), svc, true)

	for _, path := range []string{"/healthz", "/healthcheck"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected %s to return 200, got %d", path, rec.Code)
		}
	}
}

func newTestConfigManager(t *testing.T) (*atriumconfig.Manager, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "integrations.yaml")
	writeTestConfig(t, path)
	manager, err := atriumconfig.NewManager(path)
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}
	return manager, path
}

// testCollector describes one sqlserver collector for writeTestConfig.
type testCollector struct {
	id, env, interval string
}

// writeTestConfig writes a valid integrations file declaring collectors.
func writeTestConfig(t *testing.T, path string, collectors ...testCollector) {
	t.Helper()
	var b strings.Builder
	b.WriteString("dashboards:\n  base_url: http://grafana:3000\n  dashboard_templates: {}\nlogs:\n  base_url: http://loki:3100\nalerting:\n  base_url: http://alertmanager:9093\n")
	if len(collectors) == 0 {
		b.WriteString("collectors: []\n")
	} else {
		b.WriteString("collectors:\n")
	}
	for _, c := range collectors {
		interval := c.interval
		if interval == "" {
			interval = "1s"
		}
		fmt.Fprintf(&b, `  - id: %s
    kind: sqlserver
    enabled: true
    credential_ref: kv/%s
    config:
      environment: %s
      scrape_interval: %s
      probes:
        - name: waits
      targets:
        - name: core-db-%s
          host: sql.example.internal
          port: 1433
          database_name: Atrium
`, c.id, c.id, c.env, interval, c.id)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// testTimeouts are short timeouts so failure paths finish quickly.
func testTimeouts() timeouts {
	return timeouts{
		reload:       2 * time.Second,
		rollback:     2 * time.Second,
		httpShutdown: time.Second,
		pollerStop:   2 * time.Second,
		background:   time.Second,
		watchSettle:  10 * time.Millisecond,
		staleGrace:   10 * time.Second,
	}
}

// newTestService builds an initialised service whose pollers are driven by
// run. The lifecycle is shut down at test cleanup.
func newTestService(t *testing.T, configManager *atriumconfig.Manager, run pollerRunFunc) *service {
	t.Helper()
	lifecycle := newPollerLifecycleWithRun(context.Background(), nil, run)
	lifecycle.backoff = restartBackoff{initial: 10 * time.Millisecond, max: 40 * time.Millisecond}
	svc := newService(configManager, lifecycle, nil, "test-token", testTimeouts())
	if err := svc.initialReconcile(context.Background()); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = lifecycle.shutdown(ctx)
	})
	return svc
}

// pollerBehavior decides what one poller run does.
type pollerBehavior func(ctx context.Context, report func(collectors.CycleResult)) error

// fakePollers is a controllable pollerRunFunc. By default each run reports a
// healthy cycle every 5ms until cancelled.
type fakePollers struct {
	mu       sync.Mutex
	behavior map[string]pollerBehavior
	starts   map[string][]time.Time
	ctxs     map[string][]context.Context
	configs  map[string][]atriumconfig.CollectorRuntimeConfig
}

func newFakePollers() *fakePollers {
	return &fakePollers{
		behavior: map[string]pollerBehavior{},
		starts:   map[string][]time.Time{},
		ctxs:     map[string][]context.Context{},
		configs:  map[string][]atriumconfig.CollectorRuntimeConfig{},
	}
}

func (f *fakePollers) set(id string, behavior pollerBehavior) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.behavior[id] = behavior
}

func (f *fakePollers) run(ctx context.Context, collector atriumconfig.CollectorRuntimeConfig, report func(collectors.CycleResult)) error {
	f.mu.Lock()
	f.starts[collector.ID] = append(f.starts[collector.ID], time.Now())
	f.ctxs[collector.ID] = append(f.ctxs[collector.ID], ctx)
	f.configs[collector.ID] = append(f.configs[collector.ID], collector)
	behavior := f.behavior[collector.ID]
	f.mu.Unlock()
	if behavior == nil {
		behavior = healthyPoller(collector.ID, collectors.TargetOK)
	}
	return behavior(ctx, report)
}

func (f *fakePollers) startCount(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.starts[id])
}

func (f *fakePollers) lastCtx(id string) context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	ctxs := f.ctxs[id]
	return ctxs[len(ctxs)-1]
}

// healthyPoller reports a cycle with one target in state every 5ms.
func healthyPoller(id string, state collectors.TargetState) pollerBehavior {
	return func(ctx context.Context, report func(collectors.CycleResult)) error {
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			now := time.Now()
			target := collectors.TargetResult{Target: "core-db", State: state, LastSuccess: now}
			if state != collectors.TargetOK {
				target = collectors.TargetResult{
					Target:              "core-db",
					State:               state,
					Err:                 fmt.Errorf("mssql: login failed for user 'atrium_svc' on sql.example.internal:1433"),
					ConsecutiveFailures: 3,
					NextAttempt:         now.Add(time.Minute),
				}
			}
			report(collectors.CycleResult{CollectorID: id, Started: now, Finished: now, Targets: []collectors.TargetResult{target}})
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
		}
	}
}

// eventually polls cond until it holds or the deadline passes. cond is checked
// once more after the deadline, so a test goroutine starved past the deadline
// on a busy CI runner does not fail without looking. Wrap args in lazy so the
// failure message shows the final state rather than the state before polling
// began.
func eventually(t *testing.T, timeout time.Duration, cond func() bool, format string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if cond() {
		return
	}
	t.Fatalf(format, args...)
}

// lazy defers evaluating a format argument until it is printed.
type lazy func() any

func (f lazy) Format(s fmt.State, verb rune) { fmt.Fprintf(s, fmt.FormatString(s, verb), f()) }

// serve issues one request against handler.
func serve(handler http.Handler, method, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// decode unmarshals a JSON response body into out.
func decode(t *testing.T, rec *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
}

func TestReloadRouteRejectsUnauthorizedAndInvalidCandidates(t *testing.T) {
	manager, path := newTestConfigManager(t)
	handler := routes(prometheus.NewRegistry(), newTestService(t, manager, newFakePollers().run), true)
	active := manager.Snapshot().Config.Version
	for _, tc := range []struct {
		method, token string
		want          int
	}{
		{http.MethodGet, "Bearer test-token", http.StatusMethodNotAllowed},
		{http.MethodPost, "", http.StatusUnauthorized},
		{http.MethodPost, "Bearer wrong", http.StatusUnauthorized},
		{http.MethodPost, "Bearer test-token", http.StatusOK},
	} {
		rec := serve(handler, tc.method, "/admin/config/reload", tc.token)
		if rec.Code != tc.want {
			t.Fatalf("reload returned %d, want %d", rec.Code, tc.want)
		}
	}
	if err := os.WriteFile(path, []byte("logs: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := serve(handler, http.MethodPost, "/admin/config/reload", "Bearer test-token")
	if rec.Code != http.StatusBadRequest || manager.Snapshot().Config.Version != active {
		t.Fatalf("invalid candidate: status=%d active=%s", rec.Code, manager.Snapshot().Config.Version)
	}
	if manager.Snapshot().LastReloadErr == "" {
		t.Fatal("missing failed reload diagnostic")
	}
	// A rejected candidate keeps the old config running and the pod ready.
	if rec := serve(handler, http.MethodGet, "/readyz", ""); rec.Code != http.StatusOK {
		t.Fatalf("invalid candidate made the pod unready: %d %s", rec.Code, rec.Body.String())
	}
}

func TestReloadRouteRejectsReloadBeforeInitialReconcile(t *testing.T) {
	manager, _ := newTestConfigManager(t)
	lifecycle := newPollerLifecycleWithRun(context.Background(), nil, newFakePollers().run)
	svc := newService(manager, lifecycle, nil, "test-token", testTimeouts())
	handler := routes(prometheus.NewRegistry(), svc, true)
	if rec := serve(handler, http.MethodPost, "/admin/config/reload", "Bearer test-token"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("reload before initial reconcile returned %d", rec.Code)
	}
	rec := serve(handler, http.MethodGet, "/readyz", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz before initial reconcile returned %d", rec.Code)
	}
}
