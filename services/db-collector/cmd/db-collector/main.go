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
//	HEARTBEAT_DB_COLLECTOR_ADMIN_TLS_CERT_FILE
//	HEARTBEAT_DB_COLLECTOR_ADMIN_TLS_KEY_FILE
//	                                    PEM certificate and key, both or
//	                                    neither: the admin endpoints move to
//	                                    their own HTTPS listener and leave the
//	                                    listen address (default unset)
//	HEARTBEAT_DB_COLLECTOR_ADMIN_LISTEN_ADDR
//	                                    HTTPS address of the admin endpoints
//	                                    (default ":8443")
//	HEARTBEAT_STATE_DIR                 Writable directory for state that must
//	                                    survive restarts: the outsystems_logs
//	                                    watermarks (default "/var/lib/heartbeat")
//	HEARTBEAT_CREDENTIALS_DIR           Directory of HEARTBEAT_CREDENTIAL_<REF>
//	                                    files, read on every connection; refs
//	                                    without a file come from the
//	                                    environment (default unset)
//	HEARTBEAT_CREDENTIAL_<REF>          Credential for a credential_ref, as
//	                                    username:password
//	HEARTBEAT_OTLP_HEADERS_DIR          Directory of header files the OTLP
//	                                    exporter adds to every request to
//	                                    opentelemetry.endpoint, one per header,
//	                                    named by the header; read at each
//	                                    request (default unset)
//	HEARTBEAT_OTLP_CA_FILE              PEM certificates that verify an https
//	                                    opentelemetry.endpoint instead of the
//	                                    system roots (default unset)
//	HEARTBEAT_OTLP_CLIENT_CERT_FILE     PEM client certificate and key for
//	HEARTBEAT_OTLP_CLIENT_KEY_FILE      mutual TLS, both or neither; read at
//	                                    each TLS handshake (default unset)
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

	"heartbeat/internal/otlp"
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
		OTLPHeadersDir:                  os.Getenv("HEARTBEAT_OTLP_HEADERS_DIR"),
		OTLPTLS: otlp.TLSFiles{
			CAFile:   os.Getenv("HEARTBEAT_OTLP_CA_FILE"),
			CertFile: os.Getenv("HEARTBEAT_OTLP_CLIENT_CERT_FILE"),
			KeyFile:  os.Getenv("HEARTBEAT_OTLP_CLIENT_KEY_FILE"),
		},
		AdminTLS: app.AdminTLS{
			ListenAddr: os.Getenv("HEARTBEAT_DB_COLLECTOR_ADMIN_LISTEN_ADDR"),
			CertFile:   os.Getenv("HEARTBEAT_DB_COLLECTOR_ADMIN_TLS_CERT_FILE"),
			KeyFile:    os.Getenv("HEARTBEAT_DB_COLLECTOR_ADMIN_TLS_KEY_FILE"),
		},
		Logger:    logger,
		Telemetry: tel,
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
