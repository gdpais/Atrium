package app

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestAdminAuthAuthorized(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured string
		headers    []string
		want       bool
	}{
		{name: "correct token", configured: "test-token", headers: []string{"Bearer test-token"}, want: true},
		{name: "scheme is case-insensitive", configured: "test-token", headers: []string{"bearer test-token"}, want: true},
		{name: "configured token is trimmed", configured: " test-token\n", headers: []string{"Bearer test-token"}, want: true},
		{name: "no header", configured: "test-token"},
		{name: "wrong token", configured: "test-token", headers: []string{"Bearer wrong"}},
		{name: "token prefix", configured: "test-token", headers: []string{"Bearer test-tok"}},
		{name: "token with suffix", configured: "test-token", headers: []string{"Bearer test-token-and-more"}},
		{name: "extra space keeps it part of the token", configured: "test-token", headers: []string{"Bearer  test-token"}},
		{name: "scheme only", configured: "test-token", headers: []string{"Bearer"}},
		{name: "empty bearer token", configured: "test-token", headers: []string{"Bearer "}},
		{name: "other scheme", configured: "test-token", headers: []string{"Basic test-token"}},
		{name: "raw token without scheme", configured: "test-token", headers: []string{"test-token"}},
		{name: "two headers", configured: "test-token", headers: []string{"Bearer test-token", "Bearer test-token"}},
		{name: "no token configured", configured: "", headers: []string{"Bearer "}},
		{name: "blank token configured", configured: "  ", headers: []string{"Bearer   "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/admin/config", nil)
			for _, header := range tc.headers {
				req.Header.Add("Authorization", header)
			}
			if got := newAdminAuth(tc.configured).authorized(req); got != tc.want {
				t.Fatalf("authorized = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestAdminAuthStoresOnlyAFixedLengthDigest(t *testing.T) {
	auth := newAdminAuth("test-token")
	if !auth.enabled || auth.digest != sha256.Sum256([]byte("test-token")) {
		t.Fatalf("expected the SHA-256 of the token, got %+v", auth)
	}
	if disabled := newAdminAuth(""); disabled.enabled || disabled.digest != ([sha256.Size]byte{}) {
		t.Fatalf("an empty token must disable admin auth: %+v", disabled)
	}
}

func TestAdminEndpointsRequireTheToken(t *testing.T) {
	manager, _ := newTestConfigManager(t)
	handler := routes(prometheus.NewRegistry(), newTestService(t, manager, newFakePollers().run), true)
	for _, tc := range []struct {
		method, path, token string
		want                int
	}{
		{http.MethodGet, "/admin/config", "", http.StatusUnauthorized},
		{http.MethodGet, "/admin/config", "Bearer wrong", http.StatusUnauthorized},
		{http.MethodGet, "/admin/config", "Basic dGVzdC10b2tlbg==", http.StatusUnauthorized},
		{http.MethodGet, "/admin/config", "Bearer test-token", http.StatusOK},
		// Method errors are only reported to authenticated callers.
		{http.MethodPost, "/admin/config", "", http.StatusUnauthorized},
		{http.MethodPost, "/admin/config", "Bearer test-token", http.StatusMethodNotAllowed},
		{http.MethodGet, "/admin/config/reload", "", http.StatusUnauthorized},
		{http.MethodGet, "/admin/config/reload", "Bearer test-token", http.StatusMethodNotAllowed},
		{http.MethodPost, "/admin/config/reload", "Bearer wrong", http.StatusUnauthorized},
	} {
		t.Run(fmt.Sprintf("%s %s %q", tc.method, tc.path, tc.token), func(t *testing.T) {
			rec := serve(handler, tc.method, tc.path, tc.token)
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("admin response is cacheable: %v", rec.Header())
			}
			switch tc.want {
			case http.StatusUnauthorized:
				if !strings.HasPrefix(rec.Header().Get("WWW-Authenticate"), "Bearer") || rec.Body.Len() != 0 {
					t.Fatalf("401 must carry a Bearer challenge and no body: %v %q", rec.Header(), rec.Body.String())
				}
			case http.StatusMethodNotAllowed:
				if rec.Header().Get("Allow") == "" {
					t.Fatal("405 without an Allow header")
				}
			}
		})
	}
}

func TestAdminEndpointsDisabledWithoutConfiguredToken(t *testing.T) {
	manager, _ := newTestConfigManager(t)
	lifecycle := newPollerLifecycleWithRun(t.Context(), nil, newFakePollers().run)
	handler := routes(prometheus.NewRegistry(), newService(manager, lifecycle, nil, "", testTimeouts()), true)
	for _, tc := range []struct{ method, path, token string }{
		{http.MethodGet, "/admin/config", ""},
		{http.MethodGet, "/admin/config", "Bearer "},
		{http.MethodGet, "/admin/config", "Bearer test-token"},
		{http.MethodPost, "/admin/config/reload", "Bearer "},
	} {
		if rec := serve(handler, tc.method, tc.path, tc.token); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s with %q and no configured token: %d", tc.method, tc.path, tc.token, rec.Code)
		}
	}
}

func TestAdminConfigRedactsSecretsAndListsWarnings(t *testing.T) {
	manager, path := newTestConfigManager(t)
	content := `dashboards:
  base_url: http://grafana:3000
logs:
  base_url: http://loki:3100
alerting:
  base_url: http://alertmanager:9093
notification_channels:
  - id: mail
    channel_type: email
    target_ref: ops@example.com
    credential_ref: env/SMTP_PASSWORD
    config:
      smtp_password: channel-secret
credential_refs:
  sql: env/SQL_PASSWORD
collectors: []
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	svc := newTestService(t, manager, newFakePollers().run)
	svc.warnings = startupWarnings(Config{AdminToken: "test-token", SQLServerTrustServerCertificate: true}, "", slog.New(slog.DiscardHandler))
	rec := serve(routes(prometheus.NewRegistry(), svc, true), http.MethodGet, "/admin/config", "Bearer test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, secret := range []string{"channel-secret", "env/SQL_PASSWORD\""} {
		if strings.Contains(body, secret) {
			t.Fatalf("/admin/config leaked %q: %s", secret, body)
		}
	}
	var diag adminConfigResponse
	decode(t, rec, &diag)
	if len(diag.Warnings) != 1 || diag.Warnings[0].Code != "sqlserver_trust_server_certificate" {
		t.Fatalf("TrustServerCertificate warning missing: %+v", diag.Warnings)
	}
	if diag.Config.NotificationChannels[0].Config["smtp_password"] != "<redacted>" || diag.Version == "" {
		t.Fatalf("unexpected config: %+v", diag.Config)
	}
}

// syncBuffer is a bytes.Buffer safe for a logger writing from other
// goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestStartupWarnings(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cfg       Config
		endpoint  string
		wantCodes []string
		wantLogs  []string
	}{
		{name: "secure", cfg: Config{AdminToken: "t"}},
		{name: "trust server certificate", cfg: Config{AdminToken: "t", SQLServerTrustServerCertificate: true}, wantCodes: []string{"sqlserver_trust_server_certificate"}, wantLogs: []string{"sqlserver_trust_server_certificate"}},
		{name: "no admin token is only logged", cfg: Config{}, wantLogs: []string{"admin_token_unset"}},
		{name: "OTLP headers over http", cfg: Config{AdminToken: "t", OTLPHeadersDir: "/h"}, endpoint: "HTTP://collector:4318", wantCodes: []string{"otlp_headers_over_http"}, wantLogs: []string{"otlp_headers_over_http"}},
		{name: "OTLP headers over https", cfg: Config{AdminToken: "t", OTLPHeadersDir: "/h"}, endpoint: "https://collector:4318"},
		{name: "http without OTLP headers", cfg: Config{AdminToken: "t"}, endpoint: "http://collector:4318"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			warnings := startupWarnings(tc.cfg, tc.endpoint, slog.New(slog.NewJSONHandler(&logs, nil)))
			var codes []string
			for _, warning := range warnings {
				codes = append(codes, warning.Code)
			}
			if strings.Join(codes, ",") != strings.Join(tc.wantCodes, ",") {
				t.Fatalf("warnings %v, want %v", codes, tc.wantCodes)
			}
			lines := strings.Count(logs.String(), `"level":"WARN"`)
			if lines != len(tc.wantLogs) {
				t.Fatalf("logged %d warnings, want %d: %s", lines, len(tc.wantLogs), logs.String())
			}
			for _, code := range tc.wantLogs {
				if !strings.Contains(logs.String(), `"warning":"`+code+`"`) {
					t.Fatalf("log missing %s: %s", code, logs.String())
				}
			}
		})
	}
}

func TestRunWarnsAboutTrustServerCertificate(t *testing.T) {
	var logs syncBuffer
	f := startRun(t, 0, testTimeouts(), func(deps *runDeps) {
		deps.cfg.SQLServerTrustServerCertificate = true
		deps.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	})
	addr := <-f.addr
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://%s/admin/config", addr), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	client := &http.Client{Timeout: 5 * time.Second}
	var body []byte
	eventually(t, 2*time.Second, func() bool {
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		body, _ = io.ReadAll(resp.Body)
		return resp.StatusCode == http.StatusOK
	}, "GET /admin/config failed: %s", body)
	if !strings.Contains(string(body), `"code":"sqlserver_trust_server_certificate"`) {
		t.Fatalf("warning missing from /admin/config: %s", body)
	}
	// The server only serves after startup logged its warnings.
	if !strings.Contains(logs.String(), `"warning":"sqlserver_trust_server_certificate"`) {
		t.Fatalf("no startup warning logged: %s", logs.String())
	}
}

func TestAdminConfigReportsOneSnapshot(t *testing.T) {
	manager, _ := newTestConfigManager(t)
	handler := routes(prometheus.NewRegistry(), newTestService(t, manager, newFakePollers().run), true)
	_, diag := adminConfig(t, handler)
	if diag.Version == "" || diag.Readiness.ConfigVersion != diag.Version || diag.Config.Version != diag.Version {
		t.Fatalf("versions differ: top %q, readiness %q, config %q", diag.Version, diag.Readiness.ConfigVersion, diag.Config.Version)
	}
}
