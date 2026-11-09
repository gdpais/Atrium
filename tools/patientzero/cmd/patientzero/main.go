// PatientZero is a synthetic OutSystems 11 application for Atrium's tests
// and demos (ADR 0010). It serves entry points modelled on OutSystems ones;
// each request runs SQL against the business database and writes the log rows
// the platform would write into the logging database.
//
// Configuration is read from the environment:
//
//	PATIENTZERO_LISTEN                         listen address [:8080]
//	PATIENTZERO_SQL_HOST                       SQL Server host (required)
//	PATIENTZERO_SQL_PORT                       SQL Server port [1433]
//	PATIENTZERO_SQL_CREDENTIAL                 <login>:<password> (required)
//	PATIENTZERO_SQL_TRUST_SERVER_CERTIFICATE   true to trust a self-signed certificate [false]
//	PATIENTZERO_DATABASE                       business database [patientzero]
//	PATIENTZERO_LOG_DATABASE                   logging database [patientzero_log]
//	PATIENTZERO_SCHEMA_DIR                     the oslog fixtures the logging schema is built from [/app/oslog]
//	PATIENTZERO_LOG_READER_ROLE                role granted SELECT on the log views; empty for none [atrium_oslog_reader]
//	PATIENTZERO_FRONT_END                      front-end server name in the log rows [the host name]
//	PATIENTZERO_ADMIN_TOKEN                    bearer token for the scenario admin endpoints; without it they answer 401
//	PATIENTZERO_JOURNAL_DIR                    where run journals are written [/var/lib/patientzero/journal]
//	PATIENTZERO_PROMETHEUS_URL                 Prometheus base URL for scenario signals; empty for none
//	PATIENTZERO_LOKI_URL                       Loki base URL for scenario signals; empty for none
//	PATIENTZERO_ALERTMANAGER_URL               Alertmanager base URL for scenario signals; empty for none
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	_ "github.com/microsoft/go-mssqldb"

	"atrium/tools/patientzero/internal/app"
	"atrium/tools/patientzero/internal/cluster"
	"atrium/tools/patientzero/internal/inject"
	"atrium/tools/patientzero/internal/logschema"
	"atrium/tools/patientzero/internal/oslog"
	"atrium/tools/patientzero/internal/runner"
	"atrium/tools/patientzero/internal/scenario"
	"atrium/tools/patientzero/internal/shop"
	signals "atrium/tools/patientzero/internal/signal"
	"atrium/tools/patientzero/internal/traffic"
	"atrium/tools/patientzero/scenarios"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("patientzero stopped", "err", err)
		os.Exit(1)
	}
}

type config struct {
	listen, host, login, password   string
	port                            int
	trust                           bool
	database, logDatabase           string
	schemaDir, readerRole, frontEnd string
	adminToken, journalDir          string
	sources                         signals.Sources
	// maxOpenConns bounds each SQL Server pool (business and logging) so a
	// flood of requests cannot open unbounded connections and exhaust the
	// database. maxInFlight bounds concurrent database-backed requests; past
	// it the server sheds load with 503 (see app.Server.MaxInFlight), and 0
	// means no limit.
	maxOpenConns, maxInFlight int
}

// Pool and load-shedding defaults; override with the matching env vars.
const (
	defaultMaxOpenConns = 20
	defaultMaxInFlight  = 64
)

func env(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

func loadConfig() (config, error) {
	c := config{
		listen:      env("PATIENTZERO_LISTEN", ":8080"),
		host:        os.Getenv("PATIENTZERO_SQL_HOST"),
		database:    env("PATIENTZERO_DATABASE", "patientzero"),
		logDatabase: env("PATIENTZERO_LOG_DATABASE", "patientzero_log"),
		schemaDir:   env("PATIENTZERO_SCHEMA_DIR", "/app/oslog"),
		readerRole:  env("PATIENTZERO_LOG_READER_ROLE", "atrium_oslog_reader"),
		frontEnd:    os.Getenv("PATIENTZERO_FRONT_END"),
		adminToken:  os.Getenv("PATIENTZERO_ADMIN_TOKEN"),
		journalDir:  env("PATIENTZERO_JOURNAL_DIR", "/var/lib/patientzero/journal"),
		sources: signals.Sources{
			Prometheus:   os.Getenv("PATIENTZERO_PROMETHEUS_URL"),
			Loki:         os.Getenv("PATIENTZERO_LOKI_URL"),
			Alertmanager: os.Getenv("PATIENTZERO_ALERTMANAGER_URL"),
		},
	}
	if c.host == "" {
		return c, errors.New("PATIENTZERO_SQL_HOST is required")
	}
	login, password, ok := strings.Cut(os.Getenv("PATIENTZERO_SQL_CREDENTIAL"), ":")
	if !ok || login == "" || password == "" {
		return c, errors.New("PATIENTZERO_SQL_CREDENTIAL must be <login>:<password>")
	}
	if strings.EqualFold(login, "sa") {
		return c, errors.New("PATIENTZERO_SQL_CREDENTIAL must name PatientZero's own login, not sa")
	}
	c.login, c.password = login, password
	port, err := strconv.Atoi(env("PATIENTZERO_SQL_PORT", "1433"))
	if err != nil || port < 1 || port > 65535 {
		return c, errors.New("PATIENTZERO_SQL_PORT must be a port number")
	}
	c.port = port
	if c.trust, err = strconv.ParseBool(env("PATIENTZERO_SQL_TRUST_SERVER_CERTIFICATE", "false")); err != nil {
		return c, errors.New("PATIENTZERO_SQL_TRUST_SERVER_CERTIFICATE must be true or false")
	}
	if c.maxOpenConns, err = strconv.Atoi(env("PATIENTZERO_SQL_MAX_OPEN_CONNS", strconv.Itoa(defaultMaxOpenConns))); err != nil || c.maxOpenConns < 1 {
		return c, errors.New("PATIENTZERO_SQL_MAX_OPEN_CONNS must be a positive integer")
	}
	if c.maxInFlight, err = strconv.Atoi(env("PATIENTZERO_MAX_IN_FLIGHT", strconv.Itoa(defaultMaxInFlight))); err != nil || c.maxInFlight < 0 {
		return c, errors.New("PATIENTZERO_MAX_IN_FLIGHT must be 0 (no limit) or a positive integer")
	}
	if c.database == c.logDatabase {
		return c, errors.New("PATIENTZERO_DATABASE and PATIENTZERO_LOG_DATABASE must differ")
	}
	if c.frontEnd == "" {
		c.frontEnd, _ = os.Hostname()
	}
	if c.journalDir == "" {
		return c, errors.New("PATIENTZERO_JOURNAL_DIR must not be empty")
	}
	for name, v := range map[string]string{
		"PATIENTZERO_PROMETHEUS_URL":   c.sources.Prometheus,
		"PATIENTZERO_LOKI_URL":         c.sources.Loki,
		"PATIENTZERO_ALERTMANAGER_URL": c.sources.Alertmanager,
	} {
		if v == "" {
			continue
		}
		if u, err := url.Parse(v); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			return c, fmt.Errorf("%s must be an http(s) URL without credentials", name)
		}
	}
	return c, nil
}

func (c config) dsn(database string) string {
	q := url.Values{}
	q.Set("database", database)
	q.Set("encrypt", "true")
	q.Set("TrustServerCertificate", strconv.FormatBool(c.trust))
	q.Set("app name", "PatientZero")
	u := url.URL{
		Scheme:   "sqlserver",
		User:     url.UserPassword(c.login, c.password),
		Host:     net.JoinHostPort(c.host, strconv.Itoa(c.port)),
		RawQuery: q.Encode(),
	}
	return u.String()
}

// ledgerCounts reads the ledger for the delivery checks.
type ledgerCounts struct{ l *oslog.Ledger }

func (c ledgerCounts) Count(_ context.Context, logType string, from, to time.Time, heldOver time.Duration) (rows, held int64, err error) {
	n := c.l.Count(logType, from, to, heldOver)
	return n.Rows, n.Held, nil
}

func run(log *slog.Logger) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	types, err := logschema.Load(cfg.schemaDir)
	if err != nil {
		return err
	}
	business, err := sql.Open("sqlserver", cfg.dsn(cfg.database))
	if err != nil {
		return err
	}
	defer business.Close()
	logging, err := sql.Open("sqlserver", cfg.dsn(cfg.logDatabase))
	if err != nil {
		return err
	}
	defer logging.Close()
	// Bound both pools so a request flood cannot open unbounded connections
	// and exhaust SQL Server. Requests past maxOpenConns wait in the pool for
	// a connection; the in-flight limiter (below) bounds how many can wait,
	// shedding the rest with 503.
	for _, db := range []*sql.DB{business, logging} {
		db.SetMaxOpenConns(cfg.maxOpenConns)
		db.SetMaxIdleConns(cfg.maxOpenConns)
		db.SetConnMaxLifetime(5 * time.Minute)
		db.SetConnMaxIdleTime(time.Minute)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The schemas are applied in the background, retried until SQL Server
	// answers; /readyz fails until they are in place.
	var migrated atomic.Bool
	go func() {
		for {
			err := app.Migrate(ctx, business, logging, types, cfg.readerRole)
			if err == nil {
				migrated.Store(true)
				log.Info("schemas ready", "database", cfg.database, "log_database", cfg.logDatabase, "log_types", len(types))
				return
			}
			log.Warn("applying the schemas failed; retrying", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}()

	// The scenario runner: the scenarios built into the binary, the
	// in-process faults, and the admin endpoints that start them. The
	// ledger counts the committed log rows, the known answer of the
	// delivery checks.
	writer := &oslog.SQLWriter{DB: logging, Schema: oslog.NewSchema(types)}
	ledger := &oslog.Ledger{}
	faults := &inject.Injector{Rotator: writer}
	counter := &traffic.Counter{}
	cfg.sources.Application = app.Application
	cfg.sources.Ledger = ledgerCounts{ledger}
	registry := scenario.NewRegistry()
	inject.Register(registry, faults, app.Modules)
	// Cluster faults go to the host agent of the run that has them.
	delegate := &runner.Delegate{}
	cluster.Register(registry, delegate)
	list, err := scenario.LoadDir(scenarios.FS, ".", registry)
	if err != nil {
		return fmt.Errorf("scenarios: %w", err)
	}
	scenarioRunner := runner.New(ctx, runner.Config{
		Scenarios:  list,
		Registry:   registry,
		Scopes:     []scenario.Scope{scenario.ScopeInProcess, scenario.ScopeCluster},
		Delegate:   delegate,
		Sources:    cfg.sources,
		Traffic:    counter,
		JournalDir: cfg.journalDir,
		Host:       cfg.frontEnd,
		Log:        log,
	})

	srv := &app.Server{
		Faults:      faults,
		Traffic:     counter,
		Admin:       scenarioRunner.Handler(cfg.adminToken),
		Store:       &shop.SQLStore{DB: business},
		Logs:        writer,
		Ledger:      ledger,
		Schema:      oslog.NewSchema(types),
		FrontEnd:    cfg.frontEnd,
		SlowSQL:     app.DefaultSlowSQL,
		MaxInFlight: cfg.maxInFlight,
		Log:         log,
		Ready: func(ctx context.Context) error {
			if !migrated.Load() {
				return errors.New("schemas not applied yet")
			}
			if err := app.Ping(ctx, business); err != nil {
				return err
			}
			return app.Ping(ctx, logging)
		},
	}
	httpServer := &http.Server{
		Addr:              cfg.listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- httpServer.ListenAndServe() }()
	log.Info("patientzero listening", "addr", cfg.listen, "front_end", cfg.frontEnd)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = httpServer.Shutdown(shutdown)
	// ctx has ended, so an active run is stopping: its fault removed and its
	// journal closed with run_end.
	scenarioRunner.Wait()
	return err
}
