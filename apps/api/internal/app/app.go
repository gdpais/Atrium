// Package app wires together the API's subsystems and runs the service until
// the context is cancelled.
//
// Run is the entry point. It connects to PostgreSQL (package store) and Redis
// (package redis), serves the operations of the API's contract (package
// httpapi) and the Prometheus metrics, delivers audit records from the
// outbox (package audit), and drains in-flight requests on shutdown before
// closing the connections.
package app

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"heartbeat/apps/api/internal/audit"
	"heartbeat/apps/api/internal/auth"
	"heartbeat/apps/api/internal/httpapi"
	"heartbeat/apps/api/internal/password"
	"heartbeat/apps/api/internal/redis"
	"heartbeat/apps/api/internal/store"
	"heartbeat/internal/credentials"
	"heartbeat/internal/otlp"
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
	// AuditEndpoint is the OTLP/HTTP base URL audit records go to, the OTel
	// Collector's logs receiver with the persistent queue, such as
	// http://otel-collector:4318. Empty leaves them in the outbox.
	AuditEndpoint string
	// Site is the origin unsafe methods must come from, such as
	// https://heartbeat.example.com. Empty refuses every unsafe method, so
	// nobody can log in.
	Site string
	// SessionIdleTimeout and SessionMaxAge are the session lifetimes; zero
	// takes auth.DefaultIdleTimeout and auth.DefaultMaxAge.
	SessionIdleTimeout, SessionMaxAge time.Duration
	// TrustedProxies are the peers, such as an ingress controller, whose
	// X-Forwarded-For header names the client: for the failed-login limits
	// per address and for audit records.
	TrustedProxies []netip.Prefix
	// ShutdownDelay is how long the API keeps serving once shutdown begins,
	// with GET /readyz answering 503, before it stops accepting
	// connections: time for its Service to stop sending it requests.
	ShutdownDelay time.Duration
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
	// shutdownDelay is how long the API serves, unready, once shutdown has
	// begun.
	shutdownDelay time.Duration
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
// Shutdown is synchronous: after ctx is cancelled, GET /readyz answers 503
// and Run keeps serving for cfg.ShutdownDelay, so that clients sent to it
// meanwhile are served; then it stops accepting connections, waits up to 10
// seconds for in-flight requests to finish (see defaultTimeouts), closes the
// connections still open, then the PostgreSQL and Redis connections, and
// only then returns. Run returns nil on a clean shutdown.
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
	deps, err := connect(cfg, logger, registry)
	if err != nil {
		return runDeps{}, err
	}
	t := defaultTimeouts()
	t.shutdownDelay = cfg.ShutdownDelay
	return runDeps{
		cfg:            cfg,
		logger:         logger,
		registry:       registry,
		requestMetrics: requestMetrics,
		checks:         deps.checks,
		auth:           deps.auth,
		drain:          deps.drain,
		closeDeps:      deps.close,
		timeouts:       t,
	}, nil
}

// Readiness check names, as logged when GET /readyz fails.
const (
	checkPostgreSQL = "postgresql"
	checkRedis      = "redis"
)

// connected is what connect sets up.
type connected struct {
	// checks are the readiness checks of PostgreSQL and Redis.
	checks []httpapi.Check
	// auth logs users in and authenticates their sessions.
	auth *auth.Service
	// drain delivers audit records until its context ends; nil without an
	// audit endpoint.
	drain func(context.Context)
	// close closes both connections.
	close func()
}

// connect creates the PostgreSQL pool and the Redis client, without
// connecting, registers their pool metrics, the audit outbox's and the
// logins', and returns their readiness checks, the login service, the audit
// drainer, and a function that closes both.
//
// The PostgreSQL check passes when a query reaches the database and its
// schema is at least the newest migration this build embeds, so a replica
// started before `api migrate up` has run stays out of its Service. The
// Redis check is a PING.
func connect(cfg Config, logger *slog.Logger, registerer prometheus.Registerer) (connected, error) {
	resolver := credentials.Files{Dir: cfg.CredentialsDir, Fallback: credentials.Env{}}
	want, err := store.LatestVersion()
	if err != nil {
		return connected{}, err
	}
	pool, err := store.NewPool(cfg.Postgres, resolver)
	if err != nil {
		return connected{}, fmt.Errorf("postgresql: %w", err)
	}
	client, err := redis.New(cfg.Redis, resolver)
	if err != nil {
		pool.Close()
		return connected{}, fmt.Errorf("redis: %w", err)
	}
	delivered := audit.NewDeliveredCounter()
	attempts := auth.NewAttemptsCounter()
	for _, collector := range []prometheus.Collector{
		poolCollector{postgres: pool.Stat, redis: client.PoolStats},
		audit.OutboxCollector{Queries: store.New(pool)},
		delivered,
		attempts,
	} {
		if err := registerer.Register(collector); err != nil {
			pool.Close()
			_ = client.Close()
			return connected{}, err
		}
	}
	logger.Info("dependencies configured",
		"postgresql_host", cfg.Postgres.Host, "postgresql_port", cfg.Postgres.Port,
		"postgresql_database", cfg.Postgres.Database, "postgresql_sslmode", cfg.Postgres.SSLMode,
		"postgresql_schema_version", want,
		"redis_addr", cfg.Redis.Addr, "redis_tls", cfg.Redis.TLS)
	schema := store.SchemaChecker{Queries: store.New(pool), Want: want}
	sessions := &auth.Sessions{
		Redis:       client,
		IdleTimeout: cmp.Or(cfg.SessionIdleTimeout, auth.DefaultIdleTimeout),
		MaxAge:      cmp.Or(cfg.SessionMaxAge, auth.DefaultMaxAge),
	}
	result := connected{
		checks: []httpapi.Check{
			{Name: checkPostgreSQL, Run: schema.Check},
			{Name: checkRedis, Run: func(ctx context.Context) error { return redis.Ping(ctx, client) }},
		},
		auth: auth.NewService(pool, password.NewHasher(password.DefaultParams, password.DefaultConcurrency),
			sessions, &auth.Limits{Redis: client}, attempts, logger),
		close: func() {
			pool.Close()
			if err := client.Close(); err != nil {
				logger.Warn("shutdown: close redis client", "error", err)
			}
		},
	}
	if cfg.AuditEndpoint == "" {
		logger.Warn("audit delivery off: no endpoint is set, so audit records stay in the outbox")
		return result, nil
	}
	exporter := otlp.NewClient()
	exporter.Logger = logger
	exporter.Metrics = cfg.Telemetry.OTLPClient(audit.OTLPClient)
	var identity telemetry.Identity
	if cfg.Telemetry != nil {
		identity = cfg.Telemetry.Identity
	}
	drainer := audit.NewDrainer(audit.Outbox{DB: pool}, exporter, cfg.AuditEndpoint, identity, logger)
	drainer.Delivered = delivered
	result.drain = drainer.Run
	logger.Info("audit delivery on", "endpoint", cfg.AuditEndpoint)
	return result, nil
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
	// auth, when set, serves the session operations.
	auth *auth.Service
	// drain, when set, runs beside the HTTP server until shutdown: the
	// audit drainer in production.
	drain func(context.Context)
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
	// The drainer stops when shutdown begins and is waited for before the
	// connections close: records written by the requests still draining
	// wait in the outbox for the next replica.
	if deps.drain != nil {
		drainCtx, stopDrain := context.WithCancel(ctx)
		drained := make(chan struct{})
		go func() {
			defer close(drained)
			deps.drain(drainCtx)
		}()
		defer func() {
			stopDrain()
			<-drained
		}()
	}

	opts := httpapi.Options{
		Logger:           logger,
		Checks:           deps.checks,
		ReadinessTimeout: deps.timeouts.readiness,
		Site:             deps.cfg.Site,
		TrustedProxies:   deps.cfg.TrustedProxies,
	}
	if deps.auth != nil {
		opts.Auth = deps.auth
	}
	api, err := httpapi.NewServer(opts)
	if err != nil {
		_ = listener.Close()
		return err
	}
	if deps.cfg.Site == "" {
		logger.Warn("no site origin is set, so every unsafe request is refused and nobody can log in")
	}
	server := &http.Server{
		Handler:           routes(deps.registry, deps.requestMetrics, api),
		ReadHeaderTimeout: 10 * time.Second,
		// Bodies are small JSON; a slow one ties up a connection.
		ReadTimeout: 30 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	logger.Info("http server listening", "addr", listener.Addr().String())

	var runErr error
	select {
	case <-ctx.Done():
		logger.Info("shutdown requested")
		runErr = delayShutdown(api, serveErr, logger, deps.timeouts.shutdownDelay)
	case err := <-serveErr:
		logger.Error("http server failed", "error", err)
		runErr = err
	}
	shutdown(server, logger, deps.timeouts.httpShutdown)
	return runErr
}

// delayShutdown makes the API unready and keeps it serving for delay: a
// rolling update sends no new requests to a pod once its readiness fails,
// but the Service's endpoints take a few seconds to drop it, and a request
// sent meanwhile would be refused if the API stopped listening at once. It
// returns early, with the error, if the HTTP server fails.
func delayShutdown(api *httpapi.Server, serveErr <-chan error, logger *slog.Logger, delay time.Duration) error {
	api.Drain()
	if delay <= 0 {
		return nil
	}
	logger.Info("shutdown: serving unready before draining", "delay", delay.String())
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case err := <-serveErr:
		logger.Error("http server failed", "error", err)
		return err
	}
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
