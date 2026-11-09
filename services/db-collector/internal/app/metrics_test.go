package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	atriumconfig "atrium/internal/config"
	"atrium/services/db-collector/internal/collectors"
	collectorexport "atrium/services/db-collector/internal/export"
	collectormetadata "atrium/services/db-collector/internal/metadata"
)

// noopExecutor is a ProbeExecutor whose probes succeed with no samples.
type noopExecutor struct{}

func (noopExecutor) RunProbe(context.Context, collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
	return nil, nil, nil
}

// The collector's own health must be visible on /metrics: Go runtime and
// process metrics, and the per-probe metrics the Runner records.
func TestNewRegistryExposesRuntimeProcessAndProbeMetrics(t *testing.T) {
	registry, probeMetrics, err := newRegistry()
	if err != nil {
		t.Fatalf("newRegistry: %v", err)
	}
	runner := collectors.NewRunner(noopExecutor{}, collectorexport.NewPrometheusExporter(registry), nil).WithProbeMetrics(probeMetrics)
	collector := atriumconfig.CollectorRuntimeConfig{
		ID:             "sql-a",
		Kind:           "sqlserver",
		Enabled:        true,
		ScrapeInterval: time.Minute,
		Targets: []atriumconfig.TargetRuntimeConfig{{
			Name:            "db",
			EnvironmentSlug: "prod",
			Probes:          []atriumconfig.ProbeRuntimeConfig{{Name: "waits"}},
		}},
	}
	if _, err := runner.RunOnce(context.Background(), collector); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	got := map[string]bool{}
	for _, family := range families {
		got[family.GetName()] = true
	}
	want := []string{"go_goroutines", "go_memstats_heap_alloc_bytes", collectors.MetricProbeDuration, collectors.MetricCycleDuration}
	// The process collector reads /proc on Linux and the kernel on macOS;
	// elsewhere it exports nothing.
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		want = append(want, "process_cpu_seconds_total", "process_resident_memory_bytes", "process_start_time_seconds")
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("registry does not export %s", name)
		}
	}
}

// newConfigMetricsFixture starts a service whose config manager applies the
// db-collector's validators, with the config metrics registered and the
// startup load counted as Run does.
func newConfigMetricsFixture(t *testing.T, pollers *fakePollers, collectors ...testCollector) (*service, http.Handler, *prometheus.Registry, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "integrations.yaml")
	writeTestConfig(t, path, collectors...)
	manager, err := newConfigManager(path)
	if err != nil {
		t.Fatalf("newConfigManager: %v", err)
	}
	registry := prometheus.NewRegistry()
	metrics, err := newConfigMetrics(registry, manager.Snapshot)
	if err != nil {
		t.Fatalf("newConfigMetrics: %v", err)
	}
	metrics.observe(nil)
	svc := newTestService(t, manager, pollers.run)
	svc.configMetrics = metrics
	return svc, routes(registry, svc, true), registry, path
}

// configState is what the config metrics say.
type configState struct {
	successful float64
	// successAt is the success timestamp.
	successAt float64
	// versions are the version labels of the info metric.
	versions []string
	// reloads are the counter values by result.
	reloads map[string]float64
}

// gatherConfigState reads the config metrics from registry.
func gatherConfigState(t *testing.T, registry *prometheus.Registry) configState {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	state := configState{successful: -1, successAt: -1, reloads: map[string]float64{}}
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, pair := range metric.GetLabel() {
				labels[pair.GetName()] = pair.GetValue()
			}
			switch family.GetName() {
			case MetricConfigLastReloadSuccessful:
				state.successful = metric.GetGauge().GetValue()
			case MetricConfigLastReloadSuccessTimestamp:
				state.successAt = metric.GetGauge().GetValue()
			case MetricConfigInfo:
				if metric.GetGauge().GetValue() != 1 {
					t.Errorf("%s{version=%q} is %v, want 1", MetricConfigInfo, labels["version"], metric.GetGauge().GetValue())
				}
				state.versions = append(state.versions, labels["version"])
			case MetricConfigReloads:
				state.reloads[labels["result"]] = metric.GetCounter().GetValue()
			}
		}
	}
	return state
}

// wantConfigState fails unless the metrics show the snapshot's version and
// load time, successful, and the given reload counts (every other result 0).
func wantConfigState(t *testing.T, registry *prometheus.Registry, snapshot atriumconfig.Snapshot, successful float64, reloads map[string]float64) {
	t.Helper()
	got := gatherConfigState(t, registry)
	if got.successful != successful {
		t.Errorf("%s = %v, want %v", MetricConfigLastReloadSuccessful, got.successful, successful)
	}
	if want := float64(snapshot.LoadedAt.UnixNano()) / 1e9; got.successAt != want {
		t.Errorf("%s = %v, want %v (the active config's load time)", MetricConfigLastReloadSuccessTimestamp, got.successAt, want)
	}
	if !slices.Equal(got.versions, []string{snapshot.Config.Version}) {
		t.Errorf("%s versions = %v, want exactly [%s]", MetricConfigInfo, got.versions, snapshot.Config.Version)
	}
	for _, result := range reloadResults {
		if got.reloads[result] != reloads[result] {
			t.Errorf("%s{result=%q} = %v, want %v", MetricConfigReloads, result, got.reloads[result], reloads[result])
		}
	}
}

// Every load and reload is visible in metrics: the startup load, a rejected
// candidate (a misspelled probe, usability finding F4), a reload that found
// another one in progress, and the valid file that clears the failure.
func TestConfigMetricsFollowEveryReload(t *testing.T) {
	svc, _, registry, path := newConfigMetricsFixture(t, newFakePollers(), testCollector{id: "sql-a", env: "prod"})
	manager := svc.configManager
	started := manager.Snapshot()
	wantConfigState(t, registry, started, 1, map[string]float64{reloadOK: 1})

	writeTestConfig(t, path, testCollector{id: "sql-a", env: "prod"})
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(content), "name: waits", "name: wiats", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = svc.reload(context.Background(), "test")
	if !errors.Is(err, atriumconfig.ErrInvalidCandidate) || !strings.Contains(err.Error(), `unknown SQL Server probe "wiats"; probes: blocking,`) {
		t.Fatalf("reload with a misspelled probe = %v, want an invalid candidate naming the probes", err)
	}
	// The last good config keeps running, and the metrics say so.
	if manager.Snapshot().Config.Version != started.Config.Version {
		t.Fatal("a rejected candidate replaced the active config")
	}
	wantConfigState(t, registry, started, 0, map[string]float64{reloadOK: 1, reloadInvalidCandidate: 1})

	// A reload that gives up waiting for another one reads nothing: the last
	// result stands while the other one still runs. The lock holder bypasses
	// the service, so only the busy reload is counted.
	writeTestConfig(t, path, testCollector{id: "sql-a", env: "staging"})
	svc.timeouts.reload = 20 * time.Millisecond
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_, _ = manager.ReloadApplying(func(atriumconfig.RuntimeConfig, atriumconfig.RuntimeConfig) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	_, err = svc.reload(context.Background(), "test")
	if !errors.Is(err, atriumconfig.ErrReloadBusy) {
		t.Fatalf("reload while another holds the lock = %v, want busy", err)
	}
	wantConfigState(t, registry, started, 0, map[string]float64{reloadOK: 1, reloadInvalidCandidate: 1, reloadBusy: 1})
	close(release)
	<-done
	svc.timeouts = testTimeouts()

	if _, err := svc.reload(context.Background(), "test"); err != nil {
		t.Fatalf("valid reload: %v", err)
	}
	fixed := manager.Snapshot()
	if fixed.Config.Version == started.Config.Version || !fixed.LoadedAt.After(started.LoadedAt) {
		t.Fatalf("valid reload did not activate the new file: %+v", fixed)
	}
	// One info series: the new version replaced the old one.
	wantConfigState(t, registry, fixed, 1, map[string]float64{reloadOK: 2, reloadInvalidCandidate: 1, reloadBusy: 1})
}

// A valid file that cannot be applied is a failed reload too, counted under
// the result POST /admin/config/reload reports.
func TestConfigMetricsCountAFailedApply(t *testing.T) {
	pollers := newFakePollers()
	var runs atomic.Int32
	pollers.set("sql-a", func(ctx context.Context, report func(collectors.CycleResult)) error {
		err := healthyPoller("sql-a", collectors.TargetOK)(ctx, report)
		if runs.Add(1) == 1 {
			time.Sleep(300 * time.Millisecond) // slower than the reload deadline
		}
		return err
	})
	svc, handler, registry, path := newConfigMetricsFixture(t, pollers, testCollector{id: "sql-a", env: "prod"})
	svc.timeouts.reload = 100 * time.Millisecond
	svc.timeouts.rollback = 2 * time.Second
	active := svc.configManager.Snapshot()

	writeTestConfig(t, path, testCollector{id: "sql-a", env: "staging"})
	code, body := adminReload(t, handler)
	if code != http.StatusInternalServerError || body.Result != reloadApplyFailed {
		t.Fatalf("expected an apply failure: code=%d body=%+v", code, body)
	}
	wantConfigState(t, registry, active, 0, map[string]float64{reloadOK: 1, reloadApplyFailed: 1})
}

// Run serves the config metrics on /metrics, with the startup load counted,
// and counts the reloads its triggers start.
func TestRunExportsConfigMetrics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "integrations.yaml")
	writeTestConfig(t, path)
	deps, err := newRunDeps(Config{
		ListenAddr: "127.0.0.1:0", IntegrationsPath: path, AdminToken: "test-token", StateDir: t.TempDir(),
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("newRunDeps: %v", err)
	}
	addrs := make(chan net.Addr, 1)
	deps.onListen = func(addr net.Addr) { addrs <- addr }
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- run(ctx, deps) }()
	t.Cleanup(func() {
		cancel()
		if err := <-result; err != nil {
			t.Errorf("run: %v", err)
		}
	})
	base := fmt.Sprintf("http://%s", <-addrs)
	// No idle connections for shutdown to wait for.
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	scrape := func() string {
		resp, err := client.Get(base + "/metrics")
		if err != nil {
			return ""
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}
	version := deps.configManager.Snapshot().Config.Version
	var metrics string
	for _, line := range []string{
		MetricConfigLastReloadSuccessful + " 1",
		MetricConfigReloads + `{result="ok"} 1`,
		MetricConfigReloads + `{result="invalid_candidate"} 0`,
		MetricConfigInfo + `{version="` + version + `"} 1`,
		MetricConfigLastReloadSuccessTimestamp + " ",
	} {
		eventually(t, 2*time.Second, func() bool {
			metrics = scrape()
			return strings.Contains(metrics, "\n"+line)
		}, "/metrics lacks %q:\n%s", line, lazy(func() any { return metrics }))
	}

	// The server answers before the initial reconcile ends, and a reload
	// before it is refused; /readyz is 200 once it is done.
	eventually(t, 2*time.Second, func() bool {
		resp, err := client.Get(base + "/readyz")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode == http.StatusOK
	}, "collector never became ready")
	if err := os.WriteFile(path, []byte("platform: unsupported\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, base+"/admin/config/reload", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("reload of an invalid file returned %d, want 400", resp.StatusCode)
	}
	metrics = scrape()
	for _, line := range []string{
		MetricConfigLastReloadSuccessful + " 0",
		MetricConfigReloads + `{result="invalid_candidate"} 1`,
		MetricConfigInfo + `{version="` + version + `"} 1`,
	} {
		if !strings.Contains(metrics, "\n"+line) {
			t.Errorf("/metrics after a rejected reload lacks %q:\n%s", line, metrics)
		}
	}
}

func TestReloadResult(t *testing.T) {
	rolledBack := &atriumconfig.ReconcileError{Err: errors.New("update collector sql-a: deadline")}
	tests := []struct {
		err  error
		want string
	}{
		{nil, reloadOK},
		{fmt.Errorf("%w: %v", atriumconfig.ErrInvalidCandidate, errors.New("bad host")), reloadInvalidCandidate},
		{fmt.Errorf("%w: %w", atriumconfig.ErrReloadBusy, context.DeadlineExceeded), reloadBusy},
		{fmt.Errorf("%w: %w", atriumconfig.ErrReloadApply, rolledBack), reloadApplyFailed},
	}
	for _, tt := range tests {
		if got := reloadResult(tt.err); got != tt.want {
			t.Errorf("reloadResult(%v) = %q, want %q", tt.err, got, tt.want)
		}
	}
}
