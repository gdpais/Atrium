// Package main is the entry point for the API service, Heartbeat's control
// plane (ADR 0013).
//
// Usage:
//
//	api                                          serve the HTTP API until SIGINT or SIGTERM
//	api migrate up                               apply the migrations in db/migrations, then exit
//	api migrate version                          print the database's schema version, then exit
//	api users create-admin --email A [--name N]  create an admin with the password on standard input
//
// `api migrate up` is what the API's init container runs before the API
// starts. The migrate and users commands need only the PostgreSQL settings.
//
// Environment variables:
//
//	HEARTBEAT_API_LISTEN_ADDR              HTTP listen address, host:port (default ":8081")
//	HEARTBEAT_API_POSTGRES_HOST            PostgreSQL host (required)
//	HEARTBEAT_API_POSTGRES_PORT            PostgreSQL port (default 5432)
//	HEARTBEAT_API_POSTGRES_DATABASE        database name (default "heartbeat")
//	HEARTBEAT_API_POSTGRES_SSLMODE         disable, require, verify-ca or verify-full (default verify-full)
//	HEARTBEAT_API_POSTGRES_SSLROOTCERT     PEM CA bundle for verify-ca and verify-full (default: the system's)
//	HEARTBEAT_API_POSTGRES_CREDENTIAL_REF  credential reference of the login (default "secret/api-postgres")
//	HEARTBEAT_API_REDIS_ADDR               Redis (Valkey) host:port (required)
//	HEARTBEAT_API_REDIS_TLS                true or false (default true)
//	HEARTBEAT_API_REDIS_CREDENTIAL_REF     credential reference of the login (default "secret/api-redis")
//	HEARTBEAT_API_AUDIT_ENDPOINT           OTLP/HTTP base URL audit records are sent to, such as
//	                                       http://otel-collector:4318 (default: none; they wait in the outbox)
//	HEARTBEAT_API_SITE_ORIGIN              origin unsafe requests must come from, such as
//	                                       https://heartbeat.example.com (default: none; they are all refused)
//	HEARTBEAT_API_SESSION_IDLE_TIMEOUT     a session ends after this long without a request (default 8h)
//	HEARTBEAT_API_SESSION_MAX_AGE          a session ends this long after login however active (default 168h)
//	HEARTBEAT_API_TRUSTED_PROXIES          comma-separated addresses or CIDRs of proxies whose
//	                                       X-Forwarded-For names the client (default: none)
//	HEARTBEAT_API_SHUTDOWN_DELAY           how long to serve, unready, before draining at shutdown (default 5s)
//	HEARTBEAT_CREDENTIALS_DIR              directory of HEARTBEAT_CREDENTIAL_<REF> files (default: none)
//	HEARTBEAT_CREDENTIAL_<REF>             "username:password" for a reference without a file
//
// Its own telemetry reads the variables package telemetry lists; the migrate
// and users commands send none.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"heartbeat/apps/api/internal/app"
	"heartbeat/apps/api/internal/auth"
	"heartbeat/apps/api/internal/redis"
	"heartbeat/apps/api/internal/store"
	"heartbeat/internal/credentials"
	"heartbeat/internal/telemetry"
)

// telemetryFlush bounds sending the last logs when the process stops.
const telemetryFlush = 3 * time.Second

// Environment variables.
const (
	envListenAddr            = "HEARTBEAT_API_LISTEN_ADDR"
	envPostgresHost          = "HEARTBEAT_API_POSTGRES_HOST"
	envPostgresPort          = "HEARTBEAT_API_POSTGRES_PORT"
	envPostgresDatabase      = "HEARTBEAT_API_POSTGRES_DATABASE"
	envPostgresSSLMode       = "HEARTBEAT_API_POSTGRES_SSLMODE"
	envPostgresSSLRootCert   = "HEARTBEAT_API_POSTGRES_SSLROOTCERT"
	envPostgresCredentialRef = "HEARTBEAT_API_POSTGRES_CREDENTIAL_REF"
	envRedisAddr             = "HEARTBEAT_API_REDIS_ADDR"
	envRedisTLS              = "HEARTBEAT_API_REDIS_TLS"
	envRedisCredentialRef    = "HEARTBEAT_API_REDIS_CREDENTIAL_REF"
	envAuditEndpoint         = "HEARTBEAT_API_AUDIT_ENDPOINT"
	envSiteOrigin            = "HEARTBEAT_API_SITE_ORIGIN"
	envSessionIdleTimeout    = "HEARTBEAT_API_SESSION_IDLE_TIMEOUT"
	envSessionMaxAge         = "HEARTBEAT_API_SESSION_MAX_AGE"
	envTrustedProxies        = "HEARTBEAT_API_TRUSTED_PROXIES"
	envShutdownDelay         = "HEARTBEAT_API_SHUTDOWN_DELAY"
	envCredentialsDir        = "HEARTBEAT_CREDENTIALS_DIR"
)

// maxShutdownDelay bounds HEARTBEAT_API_SHUTDOWN_DELAY: the pod's
// termination grace period must cover it and the drain after it.
const maxShutdownDelay = time.Minute

// errUsage marks a command line that names no command.
var errUsage = errors.New(`usage: api | api migrate up | api migrate version | api users create-admin --email ADDRESS [--name NAME] < password`)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := execute(ctx, os.Args[1:], logger, os.Stdin, os.Stdout)
	stop()
	switch {
	case errors.Is(err, errUsage):
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	case err != nil:
		logger.Error("api exited with error", "error", err)
		os.Exit(1)
	}
}

// execute runs the command args name.
func execute(ctx context.Context, args []string, logger *slog.Logger, stdin io.Reader, stdout io.Writer) error {
	switch {
	case len(args) == 0:
		cfg, err := loadConfig()
		if err != nil {
			return fmt.Errorf("invalid configuration: %w", err)
		}
		tel := telemetry.Start("api", os.Stderr)
		defer func() {
			flush, cancel := context.WithTimeout(context.Background(), telemetryFlush)
			defer cancel()
			tel.Shutdown(flush)
		}()
		slog.SetDefault(tel.Logger)
		cfg.Logger = tel.Logger
		cfg.Telemetry = tel
		redis.SetLogger(tel.Logger)
		return app.Run(ctx, cfg)
	case slices.Equal(args, []string{"migrate", "up"}):
		cfg, resolver, err := loadMigrateConfig()
		if err != nil {
			return fmt.Errorf("invalid configuration: %w", err)
		}
		result, err := store.MigrateUp(ctx, cfg, resolver, logger)
		if err != nil {
			return err
		}
		if result.Newer {
			logger.Info("migrate: schema is newer than this build's migrations; nothing to apply", "version", result.To)
			return nil
		}
		logger.Info("migrate: schema up to date", "from", result.From, "to", result.To)
		return nil
	case slices.Equal(args, []string{"migrate", "version"}):
		cfg, resolver, err := loadMigrateConfig()
		if err != nil {
			return fmt.Errorf("invalid configuration: %w", err)
		}
		version, dirty, err := store.MigrationVersion(ctx, cfg, resolver)
		if err != nil {
			return err
		}
		if dirty {
			_, err = fmt.Fprintf(stdout, "%d (dirty)\n", version)
			return err
		}
		_, err = fmt.Fprintf(stdout, "%d\n", version)
		return err
	case len(args) >= 2 && args[0] == "users" && args[1] == "create-admin":
		return createAdmin(ctx, args[2:], logger, stdin)
	}
	return errUsage
}

// loadConfig reads the service configuration from the environment. Unset
// variables take their defaults; a value that cannot work is an error rather
// than a silent fallback, so a typo stops startup instead of running with a
// setting nobody chose.
func loadConfig() (app.Config, error) {
	cfg := app.Config{
		ListenAddr:     env(envListenAddr, ":8081"),
		CredentialsDir: os.Getenv(envCredentialsDir),
	}
	if _, port, err := net.SplitHostPort(cfg.ListenAddr); err != nil || port == "" {
		return app.Config{}, fmt.Errorf("%s %q: want host:port, such as :8081 or 127.0.0.1:8081", envListenAddr, cfg.ListenAddr)
	}
	var err error
	if cfg.Postgres, err = loadPostgresConfig(); err != nil {
		return app.Config{}, err
	}
	if cfg.Redis, err = loadRedisConfig(); err != nil {
		return app.Config{}, err
	}
	if cfg.AuditEndpoint, err = auditEndpoint(os.Getenv(envAuditEndpoint)); err != nil {
		return app.Config{}, fmt.Errorf("%s: %w", envAuditEndpoint, err)
	}
	if cfg.Site, err = siteOrigin(os.Getenv(envSiteOrigin)); err != nil {
		return app.Config{}, fmt.Errorf("%s: %w", envSiteOrigin, err)
	}
	if cfg.SessionIdleTimeout, err = duration(envSessionIdleTimeout, auth.DefaultIdleTimeout, time.Minute, 0); err != nil {
		return app.Config{}, err
	}
	if cfg.SessionMaxAge, err = duration(envSessionMaxAge, auth.DefaultMaxAge, time.Minute, 0); err != nil {
		return app.Config{}, err
	}
	if cfg.SessionIdleTimeout > cfg.SessionMaxAge {
		return app.Config{}, fmt.Errorf("%s (%s) is longer than %s (%s): want it at most as long",
			envSessionIdleTimeout, cfg.SessionIdleTimeout, envSessionMaxAge, cfg.SessionMaxAge)
	}
	if cfg.TrustedProxies, err = trustedProxies(os.Getenv(envTrustedProxies)); err != nil {
		return app.Config{}, fmt.Errorf("%s: %w", envTrustedProxies, err)
	}
	if cfg.ShutdownDelay, err = duration(envShutdownDelay, 5*time.Second, 0, maxShutdownDelay); err != nil {
		return app.Config{}, err
	}
	return cfg, nil
}

// siteOrigin checks the site's origin and writes it as a browser writes the
// Origin header: scheme and host in lower case, without a default port or a
// trailing slash. Empty stays empty.
func siteOrigin(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", fmt.Errorf("%q: want the site's origin, such as https://heartbeat.example.com, without a path", value)
	}
	scheme, host := strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	if port := u.Port(); (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		host = strings.TrimSuffix(host, ":"+port)
	}
	return scheme + "://" + host, nil
}

// duration reads a Go duration such as 8h or 30m from key, fallback when
// unset, and fails outside [least, most]; most 0 means no upper bound.
func duration(key string, fallback, least, most time.Duration) (time.Duration, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d < least || (most > 0 && d > most) {
		bounds := fmt.Sprintf("at least %s", least)
		if most > 0 {
			bounds = fmt.Sprintf("from %s to %s", least, most)
		}
		return 0, fmt.Errorf("%s %q: want a duration such as %s, %s", key, value, fallback, bounds)
	}
	return d, nil
}

// trustedProxies reads a comma-separated list of addresses and CIDRs; an
// address stands for itself alone.
func trustedProxies(value string) ([]netip.Prefix, error) {
	var prefixes []netip.Prefix
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if prefix, err := netip.ParsePrefix(item); err == nil {
			prefixes = append(prefixes, prefix.Masked())
			continue
		}
		addr, err := netip.ParseAddr(item)
		if err != nil {
			return nil, fmt.Errorf("%q: want IP addresses or CIDRs, such as 10.0.0.0/8, separated by commas", item)
		}
		prefixes = append(prefixes, netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen()))
	}
	return prefixes, nil
}

// auditEndpoint checks an OTLP/HTTP base URL, and drops a /v1/logs the
// value may end with, since the client adds it. Empty stays empty.
func auditEndpoint(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%q: want an http or https URL such as http://otel-collector:4318, without credentials or a query", value)
	}
	return strings.TrimSuffix(strings.TrimRight(value, "/"), "/v1/logs"), nil
}

// loadMigrateConfig reads what `api migrate` needs: the PostgreSQL settings
// and the credential lookup.
func loadMigrateConfig() (store.Config, credentials.Resolver, error) {
	cfg, err := loadPostgresConfig()
	if err != nil {
		return store.Config{}, nil, err
	}
	return cfg, credentials.Files{Dir: os.Getenv(envCredentialsDir), Fallback: credentials.Env{}}, nil
}

func loadPostgresConfig() (store.Config, error) {
	cfg := store.Config{
		Host:          os.Getenv(envPostgresHost),
		Database:      env(envPostgresDatabase, "heartbeat"),
		SSLMode:       env(envPostgresSSLMode, "verify-full"),
		SSLRootCert:   os.Getenv(envPostgresSSLRootCert),
		CredentialRef: env(envPostgresCredentialRef, "secret/api-postgres"),
	}
	if cfg.Host == "" {
		return store.Config{}, fmt.Errorf("%s is required: the PostgreSQL server's host name or address", envPostgresHost)
	}
	if strings.ContainsAny(cfg.Host, "/:@?# \t") && net.ParseIP(cfg.Host) == nil {
		return store.Config{}, fmt.Errorf("%s %q: want a host name or IP address, without a port, scheme or credentials", envPostgresHost, cfg.Host)
	}
	port, err := portNumber(env(envPostgresPort, "5432"))
	if err != nil {
		return store.Config{}, fmt.Errorf("%s: %w", envPostgresPort, err)
	}
	cfg.Port = port
	if !slices.Contains(store.SSLModes, cfg.SSLMode) {
		return store.Config{}, fmt.Errorf("%s %q: want one of %s", envPostgresSSLMode, cfg.SSLMode, strings.Join(store.SSLModes, ", "))
	}
	if err := validRef(cfg.CredentialRef); err != nil {
		return store.Config{}, fmt.Errorf("%s: %w", envPostgresCredentialRef, err)
	}
	if err := cfg.Validate(); err != nil {
		return store.Config{}, fmt.Errorf("PostgreSQL settings (HEARTBEAT_API_POSTGRES_*): %w", err)
	}
	return cfg, nil
}

func loadRedisConfig() (redis.Config, error) {
	cfg := redis.Config{
		Addr:          os.Getenv(envRedisAddr),
		CredentialRef: env(envRedisCredentialRef, "secret/api-redis"),
	}
	if cfg.Addr == "" {
		return redis.Config{}, fmt.Errorf("%s is required: the Redis (Valkey) server's host:port", envRedisAddr)
	}
	tls, err := strconv.ParseBool(env(envRedisTLS, "true"))
	if err != nil {
		return redis.Config{}, fmt.Errorf("%s %q: want true or false", envRedisTLS, os.Getenv(envRedisTLS))
	}
	cfg.TLS = tls
	if err := validRef(cfg.CredentialRef); err != nil {
		return redis.Config{}, fmt.Errorf("%s: %w", envRedisCredentialRef, err)
	}
	if err := cfg.Validate(); err != nil {
		return redis.Config{}, fmt.Errorf("%s: %w", envRedisAddr, err)
	}
	return cfg, nil
}

// portNumber parses a TCP port, 1-65535.
func portNumber(value string) (int, error) {
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("%q: want a port number, 1-65535", value)
	}
	return port, nil
}

// validRef checks a credential reference: one of the prefixes the
// integrations file accepts (env/, kv/ or secret/), then letters, digits,
// ".", "-", "_" and "/" only, so the file name it maps to stays inside the
// credentials directory. The integrations file checks the prefix only.
func validRef(ref string) error {
	rest, ok := "", false
	for _, prefix := range []string{"env/", "kv/", "secret/"} {
		if after, found := strings.CutPrefix(ref, prefix); found {
			rest, ok = after, true
		}
	}
	if !ok || rest == "" {
		return fmt.Errorf("%q: want env/, kv/ or secret/ followed by a name", ref)
	}
	for _, c := range rest {
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '.', c == '-', c == '_', c == '/':
		default:
			return fmt.Errorf("%q: only letters, digits, \".\", \"-\", \"_\" and \"/\" may follow the prefix", ref)
		}
	}
	return nil
}

// env returns the value of the environment variable named key, or fallback if
// the variable is unset or empty.
func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
