// Package app wires together all db-collector subsystems and runs the service
// until the context is cancelled.
//
// Run is the entry point. It loads configuration, serves metrics plus health
// endpoints, reconciles enabled collectors, and supervises per-collector
// pollers until shutdown.
package app

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	heartbeatconfig "heartbeat/internal/config"
	"heartbeat/internal/otlp"
	"heartbeat/internal/telemetry"
	"heartbeat/services/db-collector/internal/collectors"
	"heartbeat/services/db-collector/internal/collectors/outsystems"
	connector "heartbeat/services/db-collector/internal/connectors/sqlserver"
	collectorexport "heartbeat/services/db-collector/internal/export"
	catalogoutsystems "heartbeat/services/db-collector/internal/probes/outsystems"
	catalogsqlserver "heartbeat/services/db-collector/internal/probes/sqlserver"
)

// collectorKinds are the collector kinds this service runs.
var collectorKinds = []string{heartbeatconfig.KindSQLServer, heartbeatconfig.KindOutSystemsLogs}

// outSystemsStateDir is the subdirectory of Config.StateDir that holds the
// outsystems_logs watermarks.
const outSystemsStateDir = "outsystems_logs"

// exportDrainTimeout is how long shutdown waits for outsystems_logs exports
// that are still running after the pollers stopped.
const exportDrainTimeout = 5 * time.Second

// Config holds the top-level runtime parameters for the db-collector service.
type Config struct {
	// ListenAddr is the TCP address on which the HTTP server will listen
	// (e.g. ":8082").
	ListenAddr string
	// IntegrationsPath is the path to the YAML file that declares collectors,
	// targets, and probes (e.g. "config/integrations.yaml").
	IntegrationsPath string
	// AdminToken enables the admin endpoints (GET /admin/config and POST
	// /admin/config/reload) when set. Requests must send
	// Authorization: Bearer <token>; without a token both answer 401.
	AdminToken string
	// WatchInterval enables dev/local config polling when positive. The watcher
	// checks both the config file and its parent directory.
	WatchInterval time.Duration
	// StateDir holds the state that must survive a restart: the
	// outsystems_logs watermarks, under StateDir/outsystems_logs.  It is
	// created when the first watermark is saved, so the sqlserver kind, which
	// keeps no state, never needs it.
	StateDir string
	// CredentialsDir holds one file per credential reference, normally the
	// credentials Secret mounted as a directory.  Files are read on every
	// connection, so a rotated credential needs no restart; references
	// without a file, or every reference when it is empty, come from the
	// environment ([connector.FileCredentialResolver]).
	CredentialsDir string
	// SQLServerTrustServerCertificate is a dev/test escape hatch for self-signed
	// SQL Server containers. Production should keep this disabled; when set,
	// startup logs a warning that GET /admin/config also lists.
	SQLServerTrustServerCertificate bool
	// AdminTLS, when it names a certificate, moves the admin endpoints off
	// ListenAddr to their own HTTPS listener ([AdminTLS]), so the admin
	// token never travels over plain HTTP.
	AdminTLS AdminTLS
	// Logger receives structured service logs. When nil, Run logs JSON to
	// stderr.
	Logger *slog.Logger
	// Telemetry, when set, adds its metrics to the service's registry.
	Telemetry *telemetry.Telemetry
}

// timeouts bounds every blocking step of reloads and shutdown.
type timeouts struct {
	// reload bounds one reload end to end: waiting for the reload lock plus
	// the forward reconcile (including waits for replaced pollers to exit).
	reload time.Duration
	// rollback bounds undoing a partially applied reload. It starts fresh
	// when the forward reconcile fails, so it works after reload expired.
	rollback time.Duration
	// httpShutdown bounds draining in-flight HTTP requests.
	httpShutdown time.Duration
	// pollerStop bounds waiting for every poller to finish its in-flight cycle.
	pollerStop time.Duration
	// background bounds waiting for the SIGHUP and watcher goroutines.
	background time.Duration
	// watchSettle is how long the file watcher waits after a change before
	// reloading, so editors and mount swaps finish writing.
	watchSettle time.Duration
	// staleGrace is added to 2x the scrape interval before a collector that
	// has not completed a cycle makes the pod unready.
	staleGrace time.Duration
}

// defaultTimeouts returns the production timeouts.
func defaultTimeouts() timeouts {
	return timeouts{
		reload:       30 * time.Second,
		rollback:     heartbeatconfig.DefaultRollbackTimeout,
		httpShutdown: 10 * time.Second,
		pollerStop:   20 * time.Second,
		background:   5 * time.Second,
		watchSettle:  500 * time.Millisecond,
		staleGrace:   10 * time.Second,
	}
}

// Run initialises the service, reconciles the configured collectors, and
// serves HTTP until ctx is cancelled or the HTTP server fails.
//
// Each enabled sqlserver and outsystems_logs collector runs as an
// independently supervised Poller; a crashing poller is restarted with
// backoff and never stops the process. All pollers share one Runner, which
// runs each probe with the executor of its collector's kind, and export into
// one Prometheus registry.
//
// Shutdown is synchronous: after ctx is cancelled, Run drains HTTP, stops all
// pollers, closes pooled database connections, and only then returns. Every
// step is bounded (see defaultTimeouts). Run returns nil on a clean shutdown.
func Run(ctx context.Context, cfg Config) error {
	deps, err := newRunDeps(cfg)
	if err != nil {
		return err
	}
	return run(ctx, deps)
}

// newRunDeps loads the config, failing closed on an invalid file, and builds
// the production collaborators of run.
func newRunDeps(cfg Config) (runDeps, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	configManager, err := newConfigManager(cfg.IntegrationsPath)
	if err != nil {
		return runDeps{}, err
	}
	registry, probeMetrics, err := newRegistry()
	if err != nil {
		return runDeps{}, err
	}
	if err := cfg.Telemetry.Register(registry); err != nil {
		return runDeps{}, err
	}
	httpMetrics, err := telemetry.NewHTTPMetrics(registry, MetricHTTPRequests, MetricHTTPRequestDuration)
	if err != nil {
		return runDeps{}, err
	}
	configMetrics, err := newConfigMetrics(registry, configManager.Snapshot)
	if err != nil {
		return runDeps{}, err
	}
	configMetrics.observe(nil) // the startup load
	logMetrics, err := outsystems.NewMetrics(registry)
	if err != nil {
		return runDeps{}, err
	}
	exporter := collectorexport.NewPrometheusExporter(registry)
	manager := connector.NewManager(connector.FileCredentialResolver{Dir: cfg.CredentialsDir, Fallback: connector.EnvCredentialResolver{}})
	manager.TrustServerCertificate = cfg.SQLServerTrustServerCertificate
	if err := registry.Register(manager.Collector()); err != nil {
		return runDeps{}, err
	}
	logsClient := otlp.NewClient()
	logsClient.Logger = logger
	logsClient.Metrics = cfg.Telemetry.OTLPClient("outsystems")
	logs := outsystems.NewExecutor(outsystems.SQLServerSource{Manager: manager},
		outsystems.NewStore(filepath.Join(cfg.StateDir, outSystemsStateDir)), logsClient, logMetrics, logger)
	executor := collectors.EngineExecutor{
		heartbeatconfig.KindSQLServer:      collectors.NewSQLExecutor(manager),
		heartbeatconfig.KindOutSystemsLogs: logs,
	}
	runner := collectors.NewRunner(executor, exporter, collectors.LoggingEvidenceSink{}).WithLogger(logger).WithProbeMetrics(probeMetrics)
	return runDeps{
		cfg:           cfg,
		logger:        logger,
		configManager: configManager,
		configMetrics: configMetrics,
		registry:      registry,
		httpMetrics:   httpMetrics,
		newLifecycle: func(parent context.Context) *pollerLifecycle {
			return newPollerLifecycle(parent, runner, logger)
		},
		closeConnections: func() error {
			// outsystems_logs exports still running in the background get a
			// few seconds to be accepted; the rest are cancelled and their
			// rows read again after the restart.
			ctx, cancel := context.WithTimeout(context.Background(), exportDrainTimeout)
			defer cancel()
			logs.Close(ctx)
			return manager.Close()
		},
		timeouts: defaultTimeouts(),
	}, nil
}

// newConfigManager loads the integrations file at path with every check the
// db-collector applies, at startup and on each reload: the loader's own,
// then the names of the SQL Server probes and OutSystems log types, which
// only the embedded catalogs know.
func newConfigManager(path string) (*heartbeatconfig.Manager, error) {
	return heartbeatconfig.NewManager(path,
		heartbeatconfig.WithValidator(catalogsqlserver.DefaultCatalog().Validate),
		heartbeatconfig.WithValidator(catalogoutsystems.DefaultCatalog().Validate))
}

// runDeps are the collaborators of run, injectable for tests.
type runDeps struct {
	cfg           Config
	logger        *slog.Logger
	configManager *heartbeatconfig.Manager
	// configMetrics, when set, counts every reload; Run has already counted
	// the startup load.
	configMetrics *configMetrics
	registry      *prometheus.Registry
	// httpMetrics, when set, records every request.
	httpMetrics *telemetry.HTTPMetrics
	// newLifecycle builds the poller supervisor under parent.
	newLifecycle func(parent context.Context) *pollerLifecycle
	// closeConnections releases pooled database connections after every
	// poller has stopped.
	closeConnections func() error
	timeouts         timeouts
	// onListen, when set, receives the bound HTTP address.
	onListen func(net.Addr)
	// onAdminListen, when set, receives the bound admin HTTPS address.
	onAdminListen func(net.Addr)
}

// run is Run after dependency construction.
func run(ctx context.Context, deps runDeps) error {
	logger := deps.logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	adminTLS := deps.cfg.AdminTLS.Enabled()
	var adminTLSConfig *tls.Config
	if adminTLS {
		var err error
		if adminTLSConfig, err = deps.cfg.AdminTLS.serverConfig(); err != nil {
			return err
		}
	}
	listener, err := net.Listen("tcp", deps.cfg.ListenAddr)
	if err != nil {
		return err
	}
	if deps.onListen != nil {
		deps.onListen(listener.Addr())
	}
	var adminListener net.Listener
	if adminTLS {
		if adminListener, err = net.Listen("tcp", deps.cfg.AdminTLS.listenAddr()); err != nil {
			_ = listener.Close()
			return err
		}
		if deps.onAdminListen != nil {
			deps.onAdminListen(adminListener.Addr())
		}
	}

	// Pollers must outlive ctx until they are stopped explicitly, so shutdown
	// can drain HTTP first and then stop pollers in a bounded, logged step.
	lifecycle := deps.newLifecycle(context.WithoutCancel(ctx))
	svc := newService(deps.configManager, lifecycle, logger, deps.cfg.AdminToken, deps.timeouts)
	svc.warnings = startupWarnings(deps.cfg, logger)
	svc.configMetrics = deps.configMetrics
	server := &http.Server{Addr: deps.cfg.ListenAddr, Handler: telemetry.Traced(deps.httpMetrics.Instrument(routes(deps.registry, svc, !adminTLS))), ReadHeaderTimeout: 10 * time.Second}
	servers := []*http.Server{server}
	serveErr := make(chan error, 2)
	go func() { serveErr <- server.Serve(listener) }()
	if adminTLS {
		adminServer := &http.Server{
			Addr:              adminListener.Addr().String(),
			Handler:           telemetry.Traced(deps.httpMetrics.Instrument(adminRoutes(svc))),
			TLSConfig:         adminTLSConfig,
			ReadHeaderTimeout: 10 * time.Second,
		}
		servers = append(servers, adminServer)
		go func() { serveErr <- adminServer.ServeTLS(adminListener, "", "") }()
		logger.Info("admin endpoints served over HTTPS only", "addr", adminListener.Addr().String())
	}
	logger.Info("http server listening", "addr", listener.Addr().String())

	var background sync.WaitGroup
	var runErr error
	if err := svc.initialReconcile(ctx); err != nil {
		runErr = err
	} else {
		background.Go(func() { svc.reloadOnSIGHUP(ctx) })
		if deps.cfg.WatchInterval > 0 {
			background.Go(func() { svc.reloadOnConfigChange(ctx, deps.cfg.IntegrationsPath, deps.cfg.WatchInterval) })
		}
		select {
		case <-ctx.Done():
			logger.Info("shutdown requested")
		case err := <-serveErr:
			logger.Error("http server failed", "error", err)
			runErr = err
		}
	}
	svc.shutdown(servers, &background, deps.closeConnections)
	return runErr
}

// service holds the state shared by HTTP handlers, reload triggers, and
// shutdown.
type service struct {
	configManager *heartbeatconfig.Manager
	lifecycle     *pollerLifecycle
	logger        *slog.Logger
	auth          adminAuth
	timeouts      timeouts
	now           func() time.Time
	// warnings are the insecure settings found at startup, listed in GET
	// /admin/config.
	warnings []startupWarning
	// configMetrics counts reloads by result; nil counts nothing.
	configMetrics *configMetrics

	// initialized is set once the initial reconcile has launched every
	// collector.
	initialized atomic.Bool
	// warm latches once every collector has completed its first cycle after
	// startup; before that, collectors without a cycle keep the pod unready.
	warm atomic.Bool

	readyMu   sync.Mutex
	lastReady string
}

// newService builds a service with the given collaborators.
func newService(configManager *heartbeatconfig.Manager, lifecycle *pollerLifecycle, logger *slog.Logger, adminToken string, t timeouts) *service {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &service{
		configManager: configManager,
		lifecycle:     lifecycle,
		logger:        logger,
		auth:          newAdminAuth(adminToken),
		timeouts:      t,
		now:           time.Now,
		warnings:      []startupWarning{},
	}
}

// initialReconcile starts every enabled collector from the active snapshot.
func (s *service) initialReconcile(ctx context.Context) error {
	snapshot := s.configManager.Snapshot()
	diff := heartbeatconfig.DiffCollectors(heartbeatconfig.RuntimeConfig{}, snapshot.Config, collectorKinds...)
	reconcileCtx, cancel := context.WithTimeout(ctx, s.timeouts.reload)
	defer cancel()
	if err := heartbeatconfig.ReconcileWithRollback(reconcileCtx, s.lifecycle, diff, s.timeouts.rollback); err != nil {
		s.logger.Error("initial collector reconcile failed", "error", err)
		return err
	}
	s.initialized.Store(true)
	s.logger.Info("initial collector reconcile complete", "config_version", snapshot.Config.Version, "collectors", len(diff.Added))
	return nil
}

// shutdown drains HTTP, stops every poller, waits for reload goroutines, and
// closes pooled connections, each step bounded by its timeout.
func (s *service) shutdown(servers []*http.Server, background *sync.WaitGroup, closeConnections func() error) {
	start := s.now()
	s.logger.Info("shutdown: draining http server", "timeout", s.timeouts.httpShutdown.String())
	httpCtx, cancelHTTP := context.WithTimeout(context.Background(), s.timeouts.httpShutdown)
	var drain sync.WaitGroup
	for _, server := range servers {
		drain.Go(func() {
			if err := server.Shutdown(httpCtx); err != nil {
				s.logger.Warn("shutdown: http drain timed out; closing connections", "addr", server.Addr, "error", err)
				_ = server.Close()
			}
		})
	}
	drain.Wait()
	cancelHTTP()

	s.logger.Info("shutdown: stopping collector pollers", "timeout", s.timeouts.pollerStop.String())
	stopCtx, cancelStop := context.WithTimeout(context.Background(), s.timeouts.pollerStop)
	if err := s.lifecycle.shutdown(stopCtx); err != nil {
		s.logger.Error("shutdown: pollers did not stop in time", "error", err)
	}
	cancelStop()

	if !waitGroupTimeout(background, s.timeouts.background) {
		s.logger.Warn("shutdown: reload goroutines still running", "timeout", s.timeouts.background.String())
	}

	if closeConnections != nil {
		if err := closeConnections(); err != nil {
			s.logger.Error("shutdown: closing database connections failed", "error", err)
		}
	}
	s.logger.Info("shutdown complete", "duration", s.now().Sub(start).String())
}

// waitGroupTimeout waits for wg up to timeout and reports whether it finished.
func waitGroupTimeout(wg *sync.WaitGroup, timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// routes builds the HTTP handler tree for the service.
//
// Endpoints:
//   - GET /metrics  – Prometheus metrics scrape endpoint.
//   - GET /healthz, /healthcheck – Liveness; always 200 while serving.
//   - GET /readyz   – Readiness; 200 or 503 (see evaluateReadiness) with only
//     the status in the body. Reasons and details are diagnostics.
//   - GET /admin/config – Authenticated diagnostics: redacted active config,
//     reload status, startup warnings and the full readiness report.
//   - POST /admin/config/reload – Authenticated explicit reload trigger.
//
// The unauthenticated endpoints expose no config, error text or names; every
// admin endpoint goes through requireAdmin.
func routes(registry *prometheus.Registry, svc *service, admin bool) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	health := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
	mux.HandleFunc("/healthz", health)
	mux.HandleFunc("/healthcheck", health)
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		report := svc.readiness(svc.configManager.Snapshot(), false)
		writeJSON(w, report.httpStatus(), readinessStatus{Status: report.Status})
	})
	if admin {
		addAdminRoutes(mux, svc)
	}
	return mux
}

// adminRoutes serves only the admin endpoints, for the HTTPS admin listener.
func adminRoutes(svc *service) http.Handler {
	mux := http.NewServeMux()
	addAdminRoutes(mux, svc)
	return mux
}

func addAdminRoutes(mux *http.ServeMux, svc *service) {
	mux.HandleFunc("/admin/config", svc.requireAdmin(http.MethodGet, svc.handleConfig))
	mux.HandleFunc("/admin/config/reload", svc.requireAdmin(http.MethodPost, svc.handleReload))
}

// reloadResponse is the body of POST /admin/config/reload. The endpoint is
// authenticated, so the reload error text is included.
type reloadResponse struct {
	Result string `json:"result"`
	Error  string `json:"error,omitempty"`
	// RolledBack is set when applying failed: true means the runtime was
	// restored to the active config, false means it diverged.
	RolledBack *bool           `json:"rolled_back,omitempty"`
	Summary    reloadSummary   `json:"summary"`
	Readiness  readinessReport `json:"readiness"`
}

// handleReload serves POST /admin/config/reload behind requireAdmin: 503
// before the initial reconcile or when another reload holds the lock past the
// deadline, 400 for an invalid candidate, 500 when applying failed, 200 on
// success.
func (s *service) handleReload(w http.ResponseWriter, r *http.Request) {
	if !s.initialized.Load() {
		writeJSON(w, http.StatusServiceUnavailable, reloadResponse{Result: "not_ready", Error: "initial reconcile not complete", Readiness: s.readiness(s.configManager.Snapshot(), true)})
		return
	}
	// Detach from the client connection: a caller that disconnects or times
	// out must not cancel (and roll back) a reload halfway. s.reload still
	// bounds it with timeouts.reload.
	summary, err := s.reload(context.WithoutCancel(r.Context()), "admin")
	response := reloadResponse{Result: reloadResult(err), Summary: summary}
	status := http.StatusOK
	if err != nil {
		response.Error = err.Error()
		switch response.Result {
		case reloadInvalidCandidate:
			status = http.StatusBadRequest
		case reloadBusy:
			status = http.StatusServiceUnavailable
		default:
			status = http.StatusInternalServerError
			rolledBack := rollbackSucceeded(err)
			response.RolledBack = &rolledBack
		}
	}
	response.Readiness = s.readiness(s.configManager.Snapshot(), true)
	writeJSON(w, status, response)
}

// rollbackSucceeded reports whether err carries a successful rollback.
func rollbackSucceeded(err error) bool {
	var reconcileErr *heartbeatconfig.ReconcileError
	return errors.As(err, &reconcileErr) && reconcileErr.RolledBack()
}

// reloadOnSIGHUP reloads collector config whenever the process receives SIGHUP.
func (s *service) reloadOnSIGHUP(ctx context.Context) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGHUP)
	defer signal.Stop(signals)
	for {
		select {
		case <-ctx.Done():
			return
		case <-signals:
			_, _ = s.reload(ctx, "sighup")
		}
	}
}

// reloadOnConfigChange polls the config file and its parent directory for
// changes and reloads collectors when the fingerprint changes.
func (s *service) reloadOnConfigChange(ctx context.Context, path string, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	parent := filepath.Dir(path)
	last := configFingerprint(path, parent)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			next := configFingerprint(path, parent)
			if next == last {
				continue
			}
			last = next
			if !sleepContext(ctx, s.timeouts.watchSettle) {
				return
			}
			_, _ = s.reload(ctx, "watch")
		}
	}
}

// sleepContext waits for d or until ctx ends, reporting whether d elapsed.
func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// reloadSummary counts the collector transitions a reload planned.
type reloadSummary struct {
	Added     int `json:"added"`
	Updated   int `json:"updated"`
	Removed   int `json:"removed"`
	Unchanged int `json:"unchanged"`
	// Restarted counts unchanged collectors restarted because they were not
	// running (crashed or left over from a timed-out operation).
	Restarted int `json:"restarted"`
}

// reload validates the candidate config and reconciles the runtime to it.
//
// The whole reload is bounded by timeouts.reload derived from parent (the
// detached request context for admin reloads, the app context for
// SIGHUP/watch), and
// rollback by timeouts.rollback. Pollers started by the reload run under the
// lifecycle's own base context, so they survive the end of parent.
func (s *service) reload(parent context.Context, trigger string) (reloadSummary, error) {
	ctx, cancel := context.WithTimeout(parent, s.timeouts.reload)
	defer cancel()
	start := s.now()
	s.logger.Info("config reload started", "trigger", trigger, "timeout", s.timeouts.reload.String())

	var summary reloadSummary
	snapshot, err := s.configManager.ReloadApplyingContext(ctx, func(_, next heartbeatconfig.RuntimeConfig) error {
		diff, restarted := s.planReload(next)
		summary = reloadSummary{Added: len(diff.Added), Updated: len(diff.Updated) - len(restarted), Removed: len(diff.Removed), Unchanged: len(diff.Unchanged), Restarted: len(restarted)}
		if len(restarted) > 0 {
			s.logger.Info("config reload restarting collectors that are not running", "trigger", trigger, "collectors", restarted)
		}
		return heartbeatconfig.ReconcileWithRollback(ctx, s.lifecycle, diff, s.timeouts.rollback)
	})
	s.configMetrics.observe(err)
	attrs := []any{
		"trigger", trigger, "duration", s.now().Sub(start).String(), "config_version", snapshot.Config.Version,
		"added", summary.Added, "updated", summary.Updated, "removed", summary.Removed,
		"unchanged", summary.Unchanged, "restarted", summary.Restarted,
	}
	switch {
	case err == nil:
		s.logger.Info("config reload applied", attrs...)
	case errors.Is(err, heartbeatconfig.ErrReloadApply):
		rolledBack := rollbackSucceeded(err)
		attrs = append(attrs, "error", err, "rolled_back", rolledBack)
		if rolledBack {
			s.logger.Error("config reload failed; runtime rolled back to active config", attrs...)
		} else {
			s.logger.Error("config reload failed and rollback failed; runtime diverged from active config", attrs...)
		}
	default:
		s.logger.Warn("config reload rejected; active config kept", append(attrs, "error", err)...)
	}
	return summary, err
}

// planReload diffs the collectors the runtime is actually running against
// next, so a reload after a failed rollback converges on next rather than on
// the stale snapshot. Unchanged collectors that are not running are moved to
// Updated so they are restarted; their IDs are returned.
func (s *service) planReload(next heartbeatconfig.RuntimeConfig) (heartbeatconfig.CollectorDiff, []string) {
	running := heartbeatconfig.RuntimeConfig{Collectors: s.lifecycle.configs()}
	diff := heartbeatconfig.DiffCollectors(running, next, collectorKinds...)
	nextByID := map[string]heartbeatconfig.CollectorRuntimeConfig{}
	for _, collector := range next.EnabledCollectors(collectorKinds...) {
		nextByID[collector.ID] = collector
	}
	var unchanged, restarted []string
	for _, id := range diff.Unchanged {
		if s.lifecycle.needsRestart(id) {
			diff.Updated = append(diff.Updated, nextByID[id])
			restarted = append(restarted, id)
			continue
		}
		unchanged = append(unchanged, id)
	}
	diff.Unchanged = unchanged
	sort.Slice(diff.Updated, func(i, j int) bool { return diff.Updated[i].ID < diff.Updated[j].ID })
	return diff, restarted
}

// configFingerprint combines the file and parent-directory fingerprints so
// local edits and mount swaps both trigger reloads.
func configFingerprint(path, parent string) string {
	fileInfo, fileErr := os.Stat(path)
	parentInfo, parentErr := os.Stat(parent)
	return statFingerprint(fileInfo, fileErr) + "|" + statFingerprint(parentInfo, parentErr)
}

// statFingerprint converts os.Stat output into a stable string fingerprint.
func statFingerprint(info os.FileInfo, err error) string {
	if err != nil {
		return err.Error()
	}
	return info.ModTime().UTC().Format(time.RFC3339Nano) + ":" + info.Mode().String()
}

// Readiness values for readinessReport.Status.
const (
	statusReady    = "ready"
	statusNotReady = "not_ready"
)

// readinessStatus is the body of GET /readyz. The endpoint is
// unauthenticated and kubelet probes only need the status code, so it carries
// nothing else: no reasons, names, versions or error text.
type readinessStatus struct {
	Status string `json:"status"`
}

// readinessReport is the full readiness evaluation. It is served only on the
// authenticated admin endpoints (GET /admin/config and the reload response),
// so it includes raw target error text.
type readinessReport struct {
	Status        string    `json:"status"`
	Reasons       []string  `json:"reasons"`
	ConfigVersion string    `json:"config_version"`
	LastReloadAt  time.Time `json:"last_reload_at"`
	// LastReloadErr is the config manager's reload diagnostic (candidate
	// validation or lifecycle errors, never probe errors). A failed reload
	// alone does not make the pod unready: the previous config keeps running.
	LastReloadErr   string            `json:"last_reload_err"`
	RuntimeDiverged bool              `json:"runtime_diverged"`
	Collectors      []collectorReport `json:"collectors"`
}

// httpStatus maps the report to 200 or 503.
func (r readinessReport) httpStatus() int {
	if r.Status == statusReady {
		return http.StatusOK
	}
	return http.StatusServiceUnavailable
}

// collectorReport is the per-collector section of readinessReport.
type collectorReport struct {
	ID                 string     `json:"id"`
	Phase              string     `json:"phase"`
	Ready              bool       `json:"ready"`
	StartedAt          time.Time  `json:"started_at"`
	LastCycleFinished  *time.Time `json:"last_cycle_finished"`
	CycleAgeSeconds    *float64   `json:"cycle_age_seconds"`
	StaleAfterSeconds  float64    `json:"stale_after_seconds"`
	Restarts           int        `json:"restarts"`
	ConsecutiveCrashes int        `json:"consecutive_crashes"`
	NextRestart        *time.Time `json:"next_restart,omitempty"`
	TargetsTotal       int        `json:"targets_total"`
	TargetsOK          int        `json:"targets_ok"`
	TargetsFailed      int        `json:"targets_failed"`
	TargetsBackoff     int        `json:"targets_backoff"`
	// TargetsBlocked counts targets whose probes ran but wait for their
	// output to be accepted downstream (collectors.TargetBlocked): an
	// outsystems_logs log type waiting for the OTel Collector. They are up,
	// not failed.
	TargetsBlocked int `json:"targets_blocked"`
	// TargetsDegraded counts targets with probes that failed or back off on
	// their own while their other probes succeeded
	// (collectors.TargetDegraded). They are up; their probes say which fail.
	TargetsDegraded int            `json:"targets_degraded"`
	Targets         []targetReport `json:"targets"`
}

// targetReport is the per-target section of readinessReport.
type targetReport struct {
	Name                string     `json:"name"`
	State               string     `json:"state"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	LastSuccess         *time.Time `json:"last_success"`
	NextAttempt         *time.Time `json:"next_attempt,omitempty"`
	// Error is the raw probe or driver error of the target's last failed
	// cycle, also set while it backs off. For a blocked target it says what
	// waits downstream and for how long. It can name the host, port and
	// login but never the password: hosts are validated at config load so
	// the connection URL always parses. Admin endpoints only.
	Error string `json:"error,omitempty"`
	// Probes lists the failing probes of a degraded target.
	Probes []probeReport `json:"probes,omitempty"`
}

// probeReport is a failing probe of a degraded target.
type probeReport struct {
	Name                string     `json:"name"`
	State               string     `json:"state"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	NextAttempt         *time.Time `json:"next_attempt,omitempty"`
	// Error is the probe's raw error, as Error of its target. Admin
	// endpoints only.
	Error string `json:"error,omitempty"`
}

// readiness evaluates readiness now against snapshot, latches warm-up, and
// logs transitions. withErrors adds raw target error text, for the
// authenticated endpoints only.
func (s *service) readiness(snapshot heartbeatconfig.Snapshot, withErrors bool) readinessReport {
	report, allCycled := evaluateReadiness(readinessInput{
		initialized: s.initialized.Load(),
		warm:        s.warm.Load(),
		snapshot:    snapshot,
		collectors:  s.lifecycle.states(),
		grace:       s.timeouts.staleGrace,
		now:         s.now(),
		withErrors:  withErrors,
	})
	if s.initialized.Load() && allCycled {
		s.warm.Store(true)
	}
	s.logReadinessTransition(report)
	return report
}

// logReadinessTransition logs when the readiness status changes.
func (s *service) logReadinessTransition(report readinessReport) {
	s.readyMu.Lock()
	changed := s.lastReady != report.Status
	s.lastReady = report.Status
	s.readyMu.Unlock()
	if !changed {
		return
	}
	if report.Status == statusReady {
		s.logger.Info("readiness changed", "status", report.Status)
		return
	}
	s.logger.Warn("readiness changed", "status", report.Status, "reasons", report.Reasons)
}

// readinessInput is everything evaluateReadiness needs, captured at one time.
type readinessInput struct {
	initialized bool
	warm        bool
	snapshot    heartbeatconfig.Snapshot
	collectors  []collectorState
	grace       time.Duration
	now         time.Time
	// withErrors fills targetReport.Error; /readyz leaves it off.
	withErrors bool
}

// evaluateReadiness applies the readiness rules. Readiness means "this
// collector process is doing its job", not "every monitored database is up".
// The pod is not ready (HTTP 503) when:
//
//  1. the initial reconcile has not completed, or, until warm-up, some
//     collector has not yet completed its first cycle since startup;
//  2. the last reload failed and rolling the runtime back also failed, so the
//     running collectors may not match the active config (cleared by the next
//     successful reload);
//  3. any collector is crashed: backing_off after a crash or failed;
//  4. any running collector has not completed a cycle within
//     2x its scrape interval + grace since it started or since its last cycle.
//
// Target-level failures (a monitored database down or backing off) never make
// the pod unready; they are reported per target. A rejected candidate config
// (last_reload_err) is reported but does not make the pod unready, since the
// previous config keeps running. Collectors being drained, stopped, or
// replaced by an in-progress reload are not judged.
//
// The second result reports whether every judged collector has completed at
// least one cycle, which ends warm-up.
func evaluateReadiness(in readinessInput) (readinessReport, bool) {
	report := readinessReport{
		Reasons:         []string{},
		ConfigVersion:   in.snapshot.Config.Version,
		LastReloadAt:    in.snapshot.LastReloadAt,
		LastReloadErr:   in.snapshot.LastReloadErr,
		RuntimeDiverged: in.snapshot.RuntimeDiverged,
		Collectors:      make([]collectorReport, 0, len(in.collectors)),
	}
	if !in.initialized {
		report.Reasons = append(report.Reasons, "initial collector reconcile not complete")
	}
	if in.snapshot.RuntimeDiverged {
		report.Reasons = append(report.Reasons, "last reload failed and rollback failed: running collectors may not match the active config until the next successful reload")
	}
	allCycled := true
	for _, state := range in.collectors {
		collector, reason := collectorReadiness(state, in)
		if reason != "" {
			report.Reasons = append(report.Reasons, reason)
		}
		if judged(state.Phase) && !state.HasCycle {
			allCycled = false
		}
		report.Collectors = append(report.Collectors, collector)
	}
	report.Status = statusReady
	if len(report.Reasons) > 0 {
		report.Status = statusNotReady
	}
	return report, allCycled
}

// judged reports whether a collector in phase counts towards readiness.
func judged(phase string) bool {
	return phase != phaseDraining && phase != phaseStopping && phase != phaseRestarting
}

// collectorReadiness builds one collector's report and the reason it makes
// the pod unready, if any.
func collectorReadiness(state collectorState, in readinessInput) (collectorReport, string) {
	staleAfter := 2*state.Collector.ScrapeInterval + in.grace
	report := collectorReport{
		ID:                 state.Collector.ID,
		Phase:              state.Phase,
		StartedAt:          state.Started,
		StaleAfterSeconds:  staleAfter.Seconds(),
		Restarts:           state.Restarts,
		ConsecutiveCrashes: state.Crashes,
		NextRestart:        optionalTime(state.NextRestart),
		Targets:            []targetReport{},
	}
	if state.HasCycle {
		finished := state.LastCycle.Finished
		age := in.now.Sub(finished).Seconds()
		report.LastCycleFinished = &finished
		report.CycleAgeSeconds = &age
		addTargets(&report, state.LastCycle.Targets, in.withErrors)
	}

	var reason string
	switch {
	case state.Phase == phaseBackingOff || state.Phase == phaseFailed:
		reason = fmt.Sprintf("collector %s poller is %s (consecutive crashes: %d)", state.Collector.ID, state.Phase, state.Crashes)
	case !judged(state.Phase):
		// Transitional phases owned by a bounded, in-progress reload.
	case !in.warm && !state.HasCycle:
		reason = fmt.Sprintf("collector %s has not completed its first cycle", state.Collector.ID)
	case state.Phase == phaseRunning:
		since := state.Started
		if state.HasCycle && state.LastCycle.Finished.After(since) {
			since = state.LastCycle.Finished
		}
		if idle := in.now.Sub(since); idle > staleAfter {
			reason = fmt.Sprintf("collector %s is stale: no completed cycle for %s (limit %s)", state.Collector.ID, idle.Round(time.Second), staleAfter)
		}
	}
	report.Ready = reason == ""
	return report, reason
}

// addTargets fills the target counts and per-target details of report, with
// raw error text when withErrors is set.
func addTargets(report *collectorReport, targets []collectors.TargetResult, withErrors bool) {
	for _, target := range targets {
		report.TargetsTotal++
		switch target.State {
		case collectors.TargetOK:
			report.TargetsOK++
		case collectors.TargetFailed:
			report.TargetsFailed++
		case collectors.TargetBackoff:
			report.TargetsBackoff++
		case collectors.TargetBlocked:
			report.TargetsBlocked++
		case collectors.TargetDegraded:
			report.TargetsDegraded++
		}
		var errText string
		if withErrors && target.Err != nil {
			errText = target.Err.Error()
		}
		var probes []probeReport
		for _, probe := range target.Probes {
			var probeErr string
			if withErrors && probe.Err != nil {
				probeErr = probe.Err.Error()
			}
			probes = append(probes, probeReport{
				Name:                probe.Probe,
				State:               string(probe.State),
				ConsecutiveFailures: probe.ConsecutiveFailures,
				NextAttempt:         optionalTime(probe.NextAttempt),
				Error:               probeErr,
			})
		}
		report.Targets = append(report.Targets, targetReport{
			Name:                target.Target,
			State:               string(target.State),
			ConsecutiveFailures: target.ConsecutiveFailures,
			LastSuccess:         optionalTime(target.LastSuccess),
			NextAttempt:         optionalTime(target.NextAttempt),
			Error:               errText,
			Probes:              probes,
		})
	}
}

// optionalTime returns nil for the zero time so JSON renders null.
func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// writeJSON writes value as a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
