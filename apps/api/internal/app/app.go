// Package app wires together the API's subsystems and runs the service until
// the context is cancelled.
//
// Run is the entry point. It connects to PostgreSQL (package store) and Redis
// (package redis), serves the operations of the API's contract (package
// httpapi) and the Prometheus metrics, and drains in-flight requests on
// shutdown before closing the connections.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"heartbeat/apps/api/internal/httpapi"
	"heartbeat/apps/api/internal/redis"
	"heartbeat/apps/api/internal/store"
	"heartbeat/internal/credentials"
	"heartbeat/internal/telemetry"
)

// Config holds the top-level runtime parameters for the API service.
type Config struct {
	// ListenAddr is the TCP address on which the HTTP server will listen
	// (e.g. ":8081").
	ListenAddr string
	// Postgres says where the API's PostgreSQL database is.
	Postgres store.Config
	// Redis says where Redis (Valkey) is.
	Redis redis.Config
	// CredentialsDir holds one file per credential reference, normally the
	// mounted credentials Secret, read on every new connection; references
	// without a file come from the environment ([credentials.Files]).
	CredentialsDir string
	// Logger receives structured service logs. When nil, Run logs JSON to
	// stderr.
	Logger *slog.Logger
	// Telemetry, when set, adds its metrics to the service's registry.
	Telemetry *telemetry.Telemetry
}

// timeouts bounds every blocking step of serving and shutdown.
type timeouts struct {
	// readiness bounds one GET /readyz, every check included.
	readiness time.Duration
	// httpShutdown bounds draining in-flight HTTP requests.
	httpShutdown time.Duration
}

// defaultTimeouts returns the production timeouts.
func defaultTimeouts() timeouts {
	return timeouts{
		readiness:    2 * time.Second,
		httpShutdown: 10 * time.Second,
	}
}

// Run initialises the service and serves HTTP until ctx is cancelled or the
// HTTP server fails.
//
// Run does not wait for PostgreSQL or Redis: it starts while either is
// down, and GET /readyz answers 503 until both work. It fails at once only
// on settings that cannot work.
//
// Shutdown is synchronous: after ctx is cancelled, Run stops accepting
// connections, waits up to 10 seconds for in-flight requests to finish (see
// defaultTimeouts), closes the connections still open, then the PostgreSQL
// and Redis connections, and only then returns. Run returns nil on a clean
// shutdown.
func Run(ctx context.Context, cfg Config) error {
	deps, err := newRunDeps(cfg)
	if err != nil {
		return err
	}
	return run(ctx, deps)
}

// newRunDeps builds the production collaborators of run.
func newRunDeps(cfg Config) (runDeps, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	registry, requestMetrics, err := newRegistry()
	if err != nil {
		return runDeps{}, err
	}
	if err := cfg.Telemetry.Register(registry); err != nil {
		return runDeps{}, err
	}
	checks, closeDeps, err := connect(cfg, logger, registry)
	if err != nil {
		return runDeps{}, err
	}
	return runDeps{
		cfg:            cfg,
		logger:         logger,
		registry:       registry,
		requestMetrics: requestMetrics,
		checks:         checks,
		closeDeps:      closeDeps,
		timeouts:       defaultTimeouts(),
	}, nil
}

// Readiness check names, as logged when GET /readyz fails.
const (
	checkPostgreSQL = "postgresql"
	checkRedis      = "redis"
)

// connect creates the PostgreSQL pool and the Redis client, without
// connecting, registers their pool metrics, and returns their readiness checks
// and a function that closes both.
//
// The PostgreSQL check passes when a query reaches the database and its
// schema is at least the newest migration this build embeds, so a replica
// started before `api migrate up` has run stays out of its Service. The
// Redis check is a PING.
func connect(cfg Config, logger *slog.Logger, registerer prometheus.Registerer) ([]httpapi.Check, func(), error) {
	resolver := credentials.Files{Dir: cfg.CredentialsDir, Fallback: credentials.Env{}}
	want, err := store.LatestVersion()
	if err != nil {
		return nil, nil, err
	}
	pool, err := store.NewPool(cfg.Postgres, resolver)
	if err != nil {
		return nil, nil, fmt.Errorf("postgresql: %w", err)
	}
	client, err := redis.New(cfg.Redis, resolver)
	if err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("redis: %w", err)
	}
	if err := registerer.Register(poolCollector{postgres: pool.Stat, redis: client.PoolStats}); err != nil {
		pool.Close()
		_ = client.Close()
		return nil, nil, err
	}
	logger.Info("dependencies configured",
		"postgresql_host", cfg.Postgres.Host, "postgresql_port", cfg.Postgres.Port,
		"postgresql_database", cfg.Postgres.Database, "postgresql_sslmode", cfg.Postgres.SSLMode,
		"postgresql_schema_version", want,
		"redis_addr", cfg.Redis.Addr, "redis_tls", cfg.Redis.TLS)
	schema := store.SchemaChecker{Queries: store.New(pool), Want: want}
	checks := []httpapi.Check{
		{Name: checkPostgreSQL, Run: schema.Check},
		{Name: checkRedis, Run: func(ctx context.Context) error { return redis.Ping(ctx, client) }},
	}
	closeDeps := func() {
		pool.Close()
		if err := client.Close(); err != nil {
			logger.Warn("shutdown: close redis client", "error", err)
		}
	}
	return checks, closeDeps, nil
}

// runDeps are the collaborators of run, injectable for tests.
type runDeps struct {
	cfg      Config
	logger   *slog.Logger
	registry *prometheus.Registry
	// requestMetrics, when set, records every request.
	requestMetrics *telemetry.HTTPMetrics
	// checks are the readiness checks GET /readyz runs: PostgreSQL and
	// Redis in production.
	checks []httpapi.Check
	// closeDeps, when set, closes the PostgreSQL and Redis connections once
	// the HTTP server has drained.
	closeDeps func()
	timeouts  timeouts
	// onListen, when set, receives the bound HTTP address.
	onListen func(net.Addr)
}

// run is Run after dependency construction.
func run(ctx context.Context, deps runDeps) error {
	logger := deps.logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if deps.closeDeps != nil {
		defer deps.closeDeps()
	}
	listener, err := net.Listen("tcp", deps.cfg.ListenAddr)
	if err != nil {
		return err
	}
	if deps.onListen != nil {
		deps.onListen(listener.Addr())
	}

	api := httpapi.NewServer(logger, deps.checks, deps.timeouts.readiness)
	server := &http.Server{Handler: routes(deps.registry, deps.requestMetrics, api), ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	logger.Info("http server listening", "addr", listener.Addr().String())

	var runErr error
	select {
	case <-ctx.Done():
		logger.Info("shutdown requested")
	case err := <-serveErr:
		logger.Error("http server failed", "error", err)
		runErr = err
	}
	shutdown(server, logger, deps.timeouts.httpShutdown)
	return runErr
}

// shutdown stops accepting connections and waits for in-flight requests to
// finish, at most timeout; then it closes the connections still open.
func shutdown(server *http.Server, logger *slog.Logger, timeout time.Duration) {
	start := time.Now()
	logger.Info("shutdown: draining http server", "timeout", timeout.String())
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		logger.Warn("shutdown: http drain timed out; closing connections", "error", err)
		_ = server.Close()
	}
	logger.Info("shutdown complete", "duration", time.Since(start).String())
}

// routes builds the HTTP handler tree for the service.
//
// Endpoints:
//   - GET /metrics – Prometheus metrics scrape endpoint.
//   - GET /healthz – Liveness; always 200 while serving.
//   - GET /readyz  – Readiness; 200 or 503 with only the status in the body.
//
// /healthz, /readyz and every later operation come from apps/api/openapi.yaml
// through httpapi.Server; /metrics is not part of the contract. Every request
// is recorded in the request metrics, by the route that served it.
func routes(registry *prometheus.Registry, metrics *telemetry.HTTPMetrics, api *httpapi.Server) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	api.Register(mux)
	return telemetry.Traced(metrics.Instrument(mux))
}
