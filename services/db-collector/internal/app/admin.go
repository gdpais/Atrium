package app

import (
	"crypto/sha256"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
	"time"

	atriumconfig "atrium/internal/config"
)

// adminAuth authorizes requests to the admin endpoints with the bearer token
// from ATRIUM_ADMIN_TOKEN. It is the only authorization check: every
// admin route goes through service.requireAdmin.
type adminAuth struct {
	// enabled is false when no token is configured; every request is then
	// refused.
	enabled bool
	// digest is the SHA-256 of the configured token, computed once at
	// startup. Comparing fixed-length digests keeps the comparison constant
	// time in the token's length too: subtle.ConstantTimeCompare returns
	// early when its inputs differ in length.
	digest [sha256.Size]byte
}

// newAdminAuth hashes token for later comparisons. Surrounding whitespace is
// ignored, so a Secret written with a trailing newline still works; an empty
// or blank token disables the admin endpoints.
func newAdminAuth(token string) adminAuth {
	token = strings.TrimSpace(token)
	if token == "" {
		return adminAuth{}
	}
	return adminAuth{enabled: true, digest: sha256.Sum256([]byte(token))}
}

// authorized reports whether r carries exactly one Authorization header of the
// form "Bearer <token>" (scheme case-insensitive) with the configured token.
// Only the presented value is hashed per request; its comparison with the
// expected digest takes the same time whatever the value.
func (a adminAuth) authorized(r *http.Request) bool {
	if !a.enabled {
		return false
	}
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return false
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return false
	}
	presented := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(presented[:], a.digest[:]) == 1
}

// requireAdmin wraps an admin handler. Unauthenticated requests get 401 with
// a WWW-Authenticate challenge before anything else, so they learn nothing
// about the endpoint, its methods or the service state; authenticated
// requests with a method other than method get 405. Admin responses are
// never cached.
func (s *service) requireAdmin(method string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !s.auth.authorized(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="atrium-db-collector"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != method {
			w.Header().Set("Allow", method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		next(w, r)
	}
}

// adminConfigResponse is the body of GET /admin/config: the redacted active
// config, reload status, startup warnings and the full readiness report,
// including per-target error text. It is served only with the admin token.
type adminConfigResponse struct {
	Version         string                     `json:"version"`
	LoadedAt        time.Time                  `json:"loaded_at"`
	LastReloadAt    time.Time                  `json:"last_reload_at"`
	LastReloadErr   string                     `json:"last_reload_err"`
	RuntimeDiverged bool                       `json:"runtime_diverged"`
	RollbackErr     string                     `json:"rollback_err,omitempty"`
	Warnings        []startupWarning           `json:"warnings"`
	Readiness       readinessReport            `json:"readiness"`
	Config          atriumconfig.RuntimeConfig `json:"config"`
}

// handleConfig serves GET /admin/config behind requireAdmin.
func (s *service) handleConfig(w http.ResponseWriter, _ *http.Request) {
	snapshot := s.configManager.Snapshot()
	writeJSON(w, http.StatusOK, adminConfigResponse{
		Version:         snapshot.Config.Version,
		LoadedAt:        snapshot.LoadedAt,
		LastReloadAt:    snapshot.LastReloadAt,
		LastReloadErr:   snapshot.LastReloadErr,
		RuntimeDiverged: snapshot.RuntimeDiverged,
		RollbackErr:     snapshot.RollbackErr,
		Warnings:        s.warnings,
		Readiness:       s.readiness(snapshot, true),
		Config:          snapshot.Config.Redacted(),
	})
}

// startupWarning is an insecure setting found at startup. It is logged once
// and listed in GET /admin/config.
type startupWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// startupWarnings logs the insecure or degraded settings in cfg, and in
// endpoint, the opentelemetry.endpoint of the config loaded at start, and returns
// the ones GET /admin/config lists. A missing admin token is only logged:
// nobody could read it from the endpoint it disables.
func startupWarnings(cfg Config, endpoint string, logger *slog.Logger) []startupWarning {
	warnings := []startupWarning{}
	if cfg.SQLServerTrustServerCertificate {
		warnings = append(warnings, startupWarning{
			Code: "sqlserver_trust_server_certificate",
			Message: "SQL Server certificate verification is disabled for every target " +
				"(ATRIUM_DB_COLLECTOR_SQLSERVER_TRUST_SERVER_CERTIFICATE): TLS still encrypts the connection, " +
				"but neither the certificate chain nor the host name is checked, so an attacker in the path " +
				"can impersonate the server and capture the login. Use it only with throwaway local targets.",
		})
	}
	if cfg.OTLPHeadersDir != "" && strings.HasPrefix(strings.ToLower(endpoint), "http://") {
		warnings = append(warnings, startupWarning{
			Code: "otlp_headers_over_http",
			Message: "The OTLP exporter sends the headers in ATRIUM_OTLP_HEADERS_DIR, such as a token, to " +
				"opentelemetry.endpoint over plain HTTP: anyone in the path can read them. Use an https endpoint.",
		})
	}
	for _, warning := range warnings {
		logger.Warn(warning.Message, "warning", warning.Code)
	}
	if strings.TrimSpace(cfg.AdminToken) == "" {
		logger.Warn("ATRIUM_ADMIN_TOKEN is not set: GET /admin/config and POST /admin/config/reload answer 401", "warning", "admin_token_unset")
	}
	return warnings
}
