// Package main is the entry point for the db-collector service.
//
// The binary reads its configuration from environment variables, wires up the
// collector runtime, and blocks until it receives SIGINT or SIGTERM.
//
// Environment variables:
//
//	HEARTBEAT_DB_COLLECTOR_LISTEN_ADDR  HTTP listen address (default ":8082")
//	HEARTBEAT_INTEGRATIONS_PATH         Path to integrations YAML file
//	                                    (default "config/integrations.yaml")
//	HEARTBEAT_ADMIN_TOKEN               Bearer token for the admin endpoints
//	                                    (GET /admin/config, POST /admin/config/reload);
//	                                    unset disables them
//	HEARTBEAT_STATE_DIR                 Writable directory for state that must
//	                                    survive restarts: the outsystems_logs
//	                                    watermarks (default "/var/lib/heartbeat")
//	HEARTBEAT_CREDENTIALS_DIR           Directory of HEARTBEAT_CREDENTIAL_<REF>
//	                                    files, read on every connection; refs
//	                                    without a file come from the
//	                                    environment (default unset)
//	HEARTBEAT_CREDENTIAL_<REF>          Credential for a credential_ref, as
//	                                    username:password
//
// Its own telemetry reads the variables package telemetry lists.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"heartbeat/internal/telemetry"
	"heartbeat/services/db-collector/internal/app"
)

// telemetryFlush bounds sending the last logs when the process stops.
const telemetryFlush = 3 * time.Second

func main() {
	tel := telemetry.Start("db-collector", os.Stderr)
	logger := tel.Logger
	slog.SetDefault(logger)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	cfg := app.Config{
		ListenAddr:                      env("HEARTBEAT_DB_COLLECTOR_LISTEN_ADDR", ":8082"),
		IntegrationsPath:                env("HEARTBEAT_INTEGRATIONS_PATH", "config/integrations.yaml"),
		AdminToken:                      os.Getenv("HEARTBEAT_ADMIN_TOKEN"),
		StateDir:                        env("HEARTBEAT_STATE_DIR", "/var/lib/heartbeat"),
		CredentialsDir:                  os.Getenv("HEARTBEAT_CREDENTIALS_DIR"),
		WatchInterval:                   durationEnv("HEARTBEAT_CONFIG_WATCH_INTERVAL", 0),
		SQLServerTrustServerCertificate: boolEnv("HEARTBEAT_DB_COLLECTOR_SQLSERVER_TRUST_SERVER_CERTIFICATE", false),
		Logger:                          logger,
		Telemetry:                       tel,
	}
	err := app.Run(ctx, cfg)
	stop()
	if err != nil {
		logger.Error("db-collector exited with error", "error", err)
	}
	flush, cancel := context.WithTimeout(context.Background(), telemetryFlush)
	defer cancel()
	tel.Shutdown(flush)
	if err != nil {
		cancel()
		os.Exit(1)
	}
}

// env returns the value of the environment variable named key, or fallback if
// the variable is unset or empty.
func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func durationEnv(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func boolEnv(key string, fallback bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	switch value {
	case "1", "true", "TRUE", "True", "yes", "YES", "Yes", "on", "ON", "On":
		return true
	case "0", "false", "FALSE", "False", "no", "NO", "No", "off", "OFF", "Off":
		return false
	default:
		return fallback
	}
}
