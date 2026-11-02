package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"heartbeat/apps/api/internal/redis"
	"heartbeat/apps/api/internal/store"
)

// setRequired sets the variables without a default, so a test changes only
// the one it is about.
func setRequired(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		envListenAddr, envPostgresPort, envPostgresDatabase, envPostgresSSLMode, envPostgresSSLRootCert,
		envPostgresCredentialRef, envRedisTLS, envRedisCredentialRef, envCredentialsDir, envAuditEndpoint,
	} {
		t.Setenv(key, "")
	}
	t.Setenv(envPostgresHost, "postgresql.heartbeat.svc")
	t.Setenv(envRedisAddr, "valkey.heartbeat.svc:6379")
}

func TestLoadConfigDefaults(t *testing.T) {
	setRequired(t)
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":8081" {
		t.Fatalf("ListenAddr = %q, want the default :8081", cfg.ListenAddr)
	}
	wantPostgres := store.Config{
		Host: "postgresql.heartbeat.svc", Port: 5432, Database: "heartbeat",
		SSLMode: "verify-full", CredentialRef: "secret/api-postgres",
	}
	if cfg.Postgres != wantPostgres {
		t.Fatalf("Postgres = %+v, want %+v", cfg.Postgres, wantPostgres)
	}
	wantRedis := redis.Config{Addr: "valkey.heartbeat.svc:6379", TLS: true, CredentialRef: "secret/api-redis"}
	if cfg.Redis != wantRedis {
		t.Fatalf("Redis = %+v, want %+v", cfg.Redis, wantRedis)
	}
	if cfg.CredentialsDir != "" {
		t.Fatalf("CredentialsDir = %q, want none", cfg.CredentialsDir)
	}
	if cfg.AuditEndpoint != "" {
		t.Fatalf("AuditEndpoint = %q, want none: audit records wait in the outbox", cfg.AuditEndpoint)
	}
}

func TestLoadConfigReadsEverySetting(t *testing.T) {
	setRequired(t)
	for key, value := range map[string]string{
		envListenAddr:            "127.0.0.1:9000",
		envPostgresHost:          "heartbeat.abc123.eu-west-1.rds.amazonaws.com",
		envPostgresPort:          "5433",
		envPostgresDatabase:      "heartbeat_prod",
		envPostgresSSLMode:       "verify-ca",
		envPostgresSSLRootCert:   "/etc/heartbeat/rds-ca.pem",
		envPostgresCredentialRef: "kv/api-postgres-owner",
		envRedisAddr:             "[::1]:6380",
		envRedisTLS:              "false",
		envRedisCredentialRef:    "env/redis.local",
		envCredentialsDir:        "/var/run/secrets/heartbeat/credentials",
		envAuditEndpoint:         "https://otel-collector.observability.svc:4318/",
	} {
		t.Setenv(key, value)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != "127.0.0.1:9000" || cfg.CredentialsDir != "/var/run/secrets/heartbeat/credentials" {
		t.Fatalf("ListenAddr %q, CredentialsDir %q", cfg.ListenAddr, cfg.CredentialsDir)
	}
	wantPostgres := store.Config{
		Host: "heartbeat.abc123.eu-west-1.rds.amazonaws.com", Port: 5433, Database: "heartbeat_prod",
		SSLMode: "verify-ca", SSLRootCert: "/etc/heartbeat/rds-ca.pem", CredentialRef: "kv/api-postgres-owner",
	}
	if cfg.Postgres != wantPostgres {
		t.Fatalf("Postgres = %+v, want %+v", cfg.Postgres, wantPostgres)
	}
	if want := (redis.Config{Addr: "[::1]:6380", TLS: false, CredentialRef: "env/redis.local"}); cfg.Redis != want {
		t.Fatalf("Redis = %+v, want %+v", cfg.Redis, want)
	}
	if want := "https://otel-collector.observability.svc:4318"; cfg.AuditEndpoint != want {
		t.Fatalf("AuditEndpoint = %q, want %q", cfg.AuditEndpoint, want)
	}
}

// The OTLP client adds /v1/logs itself, so a full logs URL works too.
func TestLoadConfigAcceptsTheAuditLogsURL(t *testing.T) {
	setRequired(t)
	t.Setenv(envAuditEndpoint, "http://otel-collector:4318/v1/logs")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuditEndpoint != "http://otel-collector:4318" {
		t.Fatalf("AuditEndpoint = %q, want http://otel-collector:4318", cfg.AuditEndpoint)
	}
}

func TestLoadConfigReadsTheListenAddress(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:9000", ":0", "[::1]:8081", "localhost:http"} {
		t.Run(addr, func(t *testing.T) {
			setRequired(t)
			t.Setenv(envListenAddr, addr)
			cfg, err := loadConfig()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.ListenAddr != addr {
				t.Fatalf("ListenAddr = %q, want %q", cfg.ListenAddr, addr)
			}
		})
	}
}

// No silent fallback to a default: a value that cannot work stops startup,
// and the error names the variable to fix.
func TestLoadConfigRejectsValuesThatCannotWork(t *testing.T) {
	for _, tc := range []struct {
		key, value string
	}{
		{envListenAddr, "8081"},
		{envListenAddr, "localhost"},
		{envListenAddr, "127.0.0.1:"},
		{envListenAddr, ":8081:9000"},
		{envListenAddr, "[::1"},
		{envPostgresHost, ""},
		{envPostgresHost, "db:5432"},
		{envPostgresHost, "postgres://db"},
		{envPostgresHost, "user@db"},
		{envPostgresHost, "[::1]"},
		{envPostgresPort, "0"},
		{envPostgresPort, "65536"},
		{envPostgresPort, "postgres"},
		{envPostgresSSLMode, "prefer"},
		{envPostgresSSLMode, "allow"},
		{envPostgresSSLMode, "verify_full"},
		{envPostgresCredentialRef, "api-postgres"},
		{envPostgresCredentialRef, "secret/"},
		{envRedisAddr, ""},
		{envRedisAddr, "valkey"},
		{envRedisAddr, ":6379"},
		{envRedisAddr, "valkey:0"},
		{envRedisAddr, "redis://valkey:6379"},
		{envRedisTLS, "yes please"},
		{envRedisCredentialRef, "vault:api-redis"},
		{envAuditEndpoint, "otel-collector:4318"},
		{envAuditEndpoint, "grpc://otel-collector:4317"},
		{envAuditEndpoint, "http://"},
		{envAuditEndpoint, "http://user:secret@otel-collector:4318"},
		{envAuditEndpoint, "http://otel-collector:4318?x=1"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			setRequired(t)
			t.Setenv(tc.key, tc.value)
			_, err := loadConfig()
			if err == nil {
				t.Fatalf("loadConfig accepted %s=%q", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("error %q does not name %s", err, tc.key)
			}
		})
	}
}

// The init container runs `api migrate up` with the PostgreSQL settings
// only.
func TestLoadMigrateConfigNeedsNoRedisSettings(t *testing.T) {
	setRequired(t)
	t.Setenv(envRedisAddr, "")
	t.Setenv(envRedisTLS, "not a bool")
	cfg, resolver, err := loadMigrateConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "postgresql.heartbeat.svc" || resolver == nil {
		t.Fatalf("cfg = %+v, resolver = %v", cfg, resolver)
	}
	t.Setenv(envPostgresHost, "")
	if _, _, err := loadMigrateConfig(); err == nil || !strings.Contains(err.Error(), envPostgresHost) {
		t.Fatalf("loadMigrateConfig without %s: err = %v", envPostgresHost, err)
	}
}

func TestExecuteRejectsUnknownCommands(t *testing.T) {
	setRequired(t)
	for _, args := range [][]string{
		{"serve"}, {"migrate"}, {"migrate", "down"}, {"migrate", "up", "2"}, {"--help"},
		{"users"}, {"users", "create"}, {"users", "create-admin"}, {"users", "create-admin", "--email"},
		{"users", "create-admin", "--email", "admin@example.com", "extra"},
		{"users", "create-admin", "--name", "Admin"}, {"users", "create-admin", "--password", "x"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			err := execute(context.Background(), args, slog.New(slog.DiscardHandler), strings.NewReader(""), &bytes.Buffer{})
			if !errors.Is(err, errUsage) {
				t.Fatalf("execute(%q) = %v, want the usage error", args, err)
			}
		})
	}
}

func TestExecuteStopsOnInvalidConfiguration(t *testing.T) {
	setRequired(t)
	t.Setenv(envPostgresSSLMode, "prefer")
	for _, args := range [][]string{nil, {"migrate", "up"}, {"migrate", "version"}, {"users", "create-admin", "--email", "admin@example.com"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			stdin := strings.NewReader("correct horse battery staple\n")
			err := execute(context.Background(), args, slog.New(slog.DiscardHandler), stdin, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), "invalid configuration") || !strings.Contains(err.Error(), envPostgresSSLMode) {
				t.Fatalf("execute(%q) = %v, want an invalid configuration error naming %s", args, err, envPostgresSSLMode)
			}
		})
	}
}

// create-admin checks the email and the password before it connects, so a
// mistake fails at once, without a database.
func TestCreateAdminChecksItsInputBeforeConnecting(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = devNull.Close() })
	for _, tc := range []struct {
		name  string
		email string
		stdin io.Reader
		want  string
	}{
		{"not an address", "admin", strings.NewReader("correct horse battery staple\n"), "want an address"},
		{"a display name", "Admin <admin@example.com>", strings.NewReader("correct horse battery staple\n"), "want an address"},
		{"no password", "admin@example.com", strings.NewReader(""), "shorter than 15"},
		{"short password", "admin@example.com", strings.NewReader("tr0ub4dor&3\n"), "shorter than 15"},
		{"long password", "admin@example.com", strings.NewReader(strings.Repeat("x", 1025)), "longer than 1024"},
		{"two lines", "admin@example.com", strings.NewReader("correct horse battery staple\nmore\n"), "more than one line"},
		{"not UTF-8", "admin@example.com", strings.NewReader("correct horse battery \xff\xfe\n"), "not valid UTF-8"},
		{"a terminal or device", "admin@example.com", devNull, "pipe the password in"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Unreachable on purpose: the checks must fail first.
			setRequired(t)
			t.Setenv(envPostgresHost, "127.0.0.1")
			t.Setenv(envPostgresPort, "1")
			args := []string{"users", "create-admin", "--email", tc.email}
			err := execute(context.Background(), args, slog.New(slog.DiscardHandler), tc.stdin, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("execute = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

// The password may end with a line end, LF or CRLF, which is not part of it.
func TestReadPasswordDropsOneLineEnd(t *testing.T) {
	for input, want := range map[string]string{
		"correct horse battery staple":     "correct horse battery staple",
		"correct horse battery staple\n":   "correct horse battery staple",
		"correct horse battery staple\r\n": "correct horse battery staple",
		" spaces count too, at both ends ": " spaces count too, at both ends ",
		strings.Repeat("é", 512):           strings.Repeat("é", 512),
	} {
		got, err := readPassword(strings.NewReader(input))
		if err != nil || got != want {
			t.Errorf("readPassword(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
}
