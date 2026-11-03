package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadRuntimeConfigFiltersEnabledSQLServerCollectors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "integrations.yaml")
	content := []byte(`dashboards:
  base_url: http://grafana:3000
  dashboard_templates:
    logs: /explore
logs:
  base_url: http://loki:3100
alerting:
  base_url: http://alertmanager:9093
opentelemetry:
  endpoint: http://otel-collector:4318
collectors:
  - id: sql-prod
    kind: sqlserver
    enabled: true
    credential_ref: kv/sql-prod
    config:
      environment: prod
      scrape_interval: 30s
      target_names: [core-db]
      probes:
        - name: waits
        - name: sessions
      targets:
        - name: core-db
          host: sql.prod.local
          port: 1433
          database_name: Heartbeat
          credential_ref: kv/core-db
  - id: otel-sidecar
    kind: otel
    enabled: true
    config: {}
  - id: sql-disabled
    kind: sqlserver
    enabled: false
    credential_ref: kv/sql-disabled
    config:
      environment: prod
`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadRuntimeConfig(path)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig returned error: %v", err)
	}

	collectors := cfg.EnabledCollectors("sqlserver")
	if len(collectors) != 1 {
		t.Fatalf("expected 1 enabled sqlserver collector, got %d", len(collectors))
	}
	if collectors[0].ID != "sql-prod" {
		t.Fatalf("unexpected collector id: %s", collectors[0].ID)
	}
	if collectors[0].Environment != "prod" {
		t.Fatalf("unexpected environment filter: %s", collectors[0].Environment)
	}
	if collectors[0].ScrapeInterval.String() != "30s" {
		t.Fatalf("unexpected scrape interval: %s", collectors[0].ScrapeInterval)
	}
	target := collectors[0].Targets[0]
	if target.EnvironmentSlug != "prod" {
		t.Fatalf("unexpected target environment: %s", target.EnvironmentSlug)
	}
	if len(target.Probes) != 2 {
		t.Fatalf("expected target to inherit 2 probes, got %d", len(target.Probes))
	}
}

func TestManagerPreservesActiveSnapshotOnInvalidReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "integrations.yaml")
	valid := []byte(`dashboards:
  base_url: http://grafana:3000
  dashboard_templates: {}
logs:
  base_url: http://loki:3100
alerting:
  base_url: http://alertmanager:9093
opentelemetry:
  endpoint: http://otel-collector:4318
collectors:
  - id: sql-prod
    kind: sqlserver
    enabled: true
    credential_ref: kv/sql-prod
    config:
      environment: prod
      scrape_interval: 30s
`)
	if err := os.WriteFile(path, valid, 0o600); err != nil {
		t.Fatalf("write valid config: %v", err)
	}
	manager, err := NewManager(path)
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}
	active := manager.Snapshot().Config.Version
	if err := os.WriteFile(path, []byte("platform: elastic\n"), 0o600); err != nil {
		t.Fatalf("write invalid config: %v", err)
	}
	snapshot, err := manager.Reload()
	if err == nil {
		t.Fatal("expected reload error")
	}
	if snapshot.Config.Version != active {
		t.Fatal("invalid reload replaced the active config")
	}
	if snapshot.LastReloadErr == "" {
		t.Fatal("expected reload error to be recorded")
	}
}

func TestManagerPreservesActiveSnapshotWhenApplyFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "integrations.yaml")
	first := []byte(`dashboards:
  base_url: http://grafana:3000
  dashboard_templates: {}
logs:
  base_url: http://loki:3100
alerting:
  base_url: http://alertmanager:9093
collectors:
  - id: sql-prod
    kind: sqlserver
    enabled: true
    credential_ref: kv/sql-prod
    config:
      environment: prod
`)
	second := []byte(`dashboards:
  base_url: http://grafana:3000
  dashboard_templates: {}
logs:
  base_url: http://loki:3100
alerting:
  base_url: http://alertmanager:9093
collectors:
  - id: sql-prod
    kind: sqlserver
    enabled: true
    credential_ref: kv/sql-prod
    config:
      environment: staging
`)
	if err := os.WriteFile(path, first, 0o600); err != nil {
		t.Fatalf("write first config: %v", err)
	}
	manager, err := NewManager(path)
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}
	active := manager.Snapshot().Config.Version
	if err := os.WriteFile(path, second, 0o600); err != nil {
		t.Fatalf("write second config: %v", err)
	}

	snapshot, err := manager.ReloadApplying(func(previous, next RuntimeConfig) error {
		if previous.Version == next.Version {
			t.Fatal("expected distinct previous and next config versions")
		}
		return fmt.Errorf("collector restart failed")
	})
	if !errors.Is(err, ErrReloadApply) {
		t.Fatalf("expected ErrReloadApply, got %v", err)
	}
	if snapshot.Config.Version != active {
		t.Fatal("apply failure replaced the active config")
	}
	if manager.Snapshot().Config.Version != active {
		t.Fatal("manager active config changed after apply failure")
	}
}

func TestRedactedDoesNotMutateActiveConfig(t *testing.T) {
	cfg := RuntimeConfig{
		Dashboards:     Endpoint{DashboardTemplates: map[string]string{"sql": "/d/sql"}},
		CredentialRefs: map[string]string{"sql": "env/SQL_PASSWORD"},
		NotificationChannels: []NotificationChannel{{
			ID:            "email",
			CredentialRef: "env/SMTP_PASSWORD",
			Config:        map[string]string{"smtp_host": "smtp.example.local"},
		}},
		Collectors: []CollectorRuntimeConfig{{
			ID:            "sql",
			CredentialRef: "kv/sql",
			TargetNames:   []string{"primary"},
			Probes:        []ProbeRuntimeConfig{{Name: "waits"}},
			Targets: []TargetRuntimeConfig{{
				Name:          "primary",
				CredentialRef: "kv/target",
				Probes:        []ProbeRuntimeConfig{{Name: "sessions"}},
			}},
		}},
	}

	redacted := cfg.Redacted()
	redacted.Dashboards.DashboardTemplates["sql"] = "changed"
	redacted.Collectors[0].TargetNames[0] = "changed"
	redacted.Collectors[0].Targets[0].Probes[0].Name = "changed"
	redacted.NotificationChannels[0].Config["smtp_host"] = "changed"
	redacted.Collectors[0].Targets[0].CredentialRef = "changed"
	redacted.Collectors[0].Probes[0].Name = "changed"

	if cfg.Dashboards.DashboardTemplates["sql"] != "/d/sql" || cfg.Collectors[0].TargetNames[0] != "primary" || cfg.Collectors[0].Targets[0].Probes[0].Name != "sessions" {
		t.Fatal("redacted copy aliases nested active config")
	}
	if cfg.CredentialRefs["sql"] != "env/SQL_PASSWORD" {
		t.Fatal("credential_refs value was mutated")
	}
	if cfg.NotificationChannels[0].Config["smtp_host"] != "smtp.example.local" {
		t.Fatal("notification config was mutated")
	}
	if cfg.Collectors[0].Targets[0].CredentialRef != "kv/target" {
		t.Fatal("target credential_ref was mutated")
	}
	if cfg.Collectors[0].Probes[0].Name != "waits" {
		t.Fatal("collector probes were mutated")
	}
}

func TestLoadRuntimeConfigPlatform(t *testing.T) {
	for _, tc := range []struct {
		name, content, wantPlatform, wantErr string
	}{
		{name: "only collectors: grafana by default", content: "collectors: []\n", wantPlatform: PlatformGrafana},
		{name: "grafana named", content: "platform: grafana\ncollectors: []\n", wantPlatform: PlatformGrafana},
		{name: "generic platform", content: "platform: generic\ncollectors: []\n", wantPlatform: PlatformGeneric},
		{name: "generic platform with dashboards", content: "platform: generic\ndashboards:\n  base_url: http://grafana:3000\ncollectors: []\n", wantErr: "dashboards must be empty with platform generic"},
		{name: "unsupported platform", content: "platform: elastic\ncollectors: []\n", wantErr: `platform "elastic" is not supported`},
		{name: "logs pushed directly", content: "logs:\n  base_url: http://loki:3100\n  endpoint: http://loki:3100/loki/api/v1/push\ncollectors: []\n", wantErr: "logs.endpoint is not supported"},
		{name: "alerts pushed directly", content: "alerting:\n  endpoint: http://alertmanager:9093/api/v2/alerts\ncollectors: []\n", wantErr: "alerting.endpoint is not supported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "integrations.yaml")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			cfg, err := LoadRuntimeConfig(path)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.Platform != tc.wantPlatform {
				t.Fatalf("Platform = %q, want %q", cfg.Platform, tc.wantPlatform)
			}
		})
	}
}

func TestLoadRuntimeConfigRejectsInvalidEndpoint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "integrations.yaml")
	content := []byte(`dashboards:
  base_url: http://grafana:3000
  dashboard_templates: {}
logs:
  base_url: http://loki:3100
alerting:
  base_url: http://alertmanager:9093
opentelemetry:
  endpoint: otel-collector:4318
credential_refs:
  sql: env/SQL_PASSWORD
collectors: []
`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if _, err := LoadRuntimeConfig(path); err == nil {
		t.Fatal("expected invalid endpoint to be rejected")
	}
}

func TestLoadRuntimeConfigRejectsPlaintextCredentialRefs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "integrations.yaml")
	content := []byte(`dashboards:
  base_url: http://grafana:3000
  dashboard_templates: {}
logs:
  base_url: http://loki:3100
alerting:
  base_url: http://alertmanager:9093
credential_refs:
  sql: plaintext-password
collectors: []
`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if _, err := LoadRuntimeConfig(path); err == nil {
		t.Fatal("expected plaintext credential ref to be rejected")
	}
}

func TestDiffCollectorsUsesStableIDs(t *testing.T) {
	old := RuntimeConfig{Collectors: []CollectorRuntimeConfig{
		{ID: "a", Kind: "sqlserver", Enabled: true},
		{ID: "b", Kind: "sqlserver", Enabled: true},
	}}
	next := RuntimeConfig{Collectors: []CollectorRuntimeConfig{
		{ID: "b", Kind: "sqlserver", Enabled: true, Environment: "prod"},
		{ID: "c", Kind: "sqlserver", Enabled: true},
	}}
	diff := DiffCollectors(old, next, "sqlserver")
	if len(diff.Added) != 1 || diff.Added[0].ID != "c" {
		t.Fatalf("unexpected added collectors: %#v", diff.Added)
	}
	if len(diff.Updated) != 1 || diff.Updated[0].ID != "b" {
		t.Fatalf("unexpected updated collectors: %#v", diff.Updated)
	}
	if len(diff.Removed) != 1 || diff.Removed[0] != "a" {
		t.Fatalf("unexpected removed collectors: %#v", diff.Removed)
	}
}

func TestValidateRejectsTargetScrapedByTwoSQLServerCollectors(t *testing.T) {
	target := func(name, env string) TargetRuntimeConfig {
		return TargetRuntimeConfig{Name: name, EnvironmentSlug: env, Host: "db", Port: 1433}
	}
	collector := func(id string, enabled bool, kind string, targetNames []string, targets ...TargetRuntimeConfig) CollectorRuntimeConfig {
		return CollectorRuntimeConfig{ID: id, Kind: kind, Enabled: enabled, ScrapeInterval: time.Minute, TargetNames: targetNames, Targets: targets}
	}
	tests := []struct {
		name       string
		collectors []CollectorRuntimeConfig
		wantErr    bool
	}{
		{"same target in two enabled collectors", []CollectorRuntimeConfig{
			collector("a", true, "sqlserver", nil, target("core-db", "prod")),
			collector("b", true, "sqlserver", nil, target("core-db", "prod")),
		}, true},
		{"same name in another environment", []CollectorRuntimeConfig{
			collector("a", true, "sqlserver", nil, target("core-db", "prod")),
			collector("b", true, "sqlserver", nil, target("core-db", "staging")),
		}, false},
		{"second collector disabled", []CollectorRuntimeConfig{
			collector("a", true, "sqlserver", nil, target("core-db", "prod")),
			collector("b", false, "sqlserver", nil, target("core-db", "prod")),
		}, false},
		{"second collector of another kind", []CollectorRuntimeConfig{
			collector("a", true, "sqlserver", nil, target("core-db", "prod")),
			collector("b", true, "oracle", nil, target("core-db", "prod")),
		}, false},
		{"target_names leaves the duplicate unselected", []CollectorRuntimeConfig{
			collector("a", true, "sqlserver", nil, target("core-db", "prod")),
			collector("b", true, "sqlserver", []string{"other-db"}, target("core-db", "prod"), target("other-db", "prod")),
		}, false},
		{"target_names selects the duplicate", []CollectorRuntimeConfig{
			collector("a", true, "sqlserver", nil, target("core-db", "prod")),
			collector("b", true, "sqlserver", []string{"core-db"}, target("core-db", "prod"), target("other-db", "prod")),
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validate(RuntimeConfig{
				Dashboards: Endpoint{BaseURL: "http://grafana:3000"},
				Logs:       Endpoint{BaseURL: "http://loki:3100"},
				Alerting:   Endpoint{BaseURL: "http://alertmanager:9093"},
				Collectors: tt.collectors,
			})
			if tt.wantErr != (err != nil) {
				t.Fatalf("wantErr %v, got %v", tt.wantErr, err)
			}
			if err != nil && !strings.Contains(err.Error(), `target "core-db" in environment "prod" is scraped by collectors a and b`) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidateBoundsProbeTimeout(t *testing.T) {
	maxMS := int(MaxProbeTimeout.Milliseconds())
	tests := []struct {
		name      string
		timeoutMS int
		wantErr   string
	}{
		{"unset uses the default", 0, ""},
		{"within the maximum", 5000, ""},
		{"at the maximum", maxMS, ""},
		{"negative", -1, "timeout_ms cannot be negative"},
		{"above the maximum", maxMS + 1, fmt.Sprintf("timeout_ms %d exceeds the maximum of %d", maxMS+1, maxMS)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := RuntimeConfig{
				Dashboards: Endpoint{BaseURL: "http://grafana:3000"},
				Logs:       Endpoint{BaseURL: "http://loki:3100"},
				Alerting:   Endpoint{BaseURL: "http://alertmanager:9093"},
				Collectors: []CollectorRuntimeConfig{{
					ID: "sql", Kind: "sqlserver", Enabled: true, ScrapeInterval: time.Minute,
					Targets: []TargetRuntimeConfig{{
						Name: "core-db", Host: "db", Port: 1433,
						Probes: []ProbeRuntimeConfig{{Name: "p1", TimeoutMS: tt.timeoutMS}},
					}},
				}}}
			err := validate(cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestUserinfoDetectionAndStripping(t *testing.T) {
	for _, tc := range []struct {
		raw      string
		has      bool
		stripped string
	}{
		{raw: "http://loki:3100/loki/api/v1/push", stripped: "http://loki:3100/loki/api/v1/push"},
		{raw: "http://user:s3cret@loki:3100/push", has: true, stripped: "http://loki:3100/push"},
		{raw: "https://token@grafana.example.internal", has: true, stripped: "https://grafana.example.internal"},
		{raw: "HTTPS://a:b@c:d@host/x", has: true, stripped: "HTTPS://host/x"},
		{raw: "https://u:p@${grafana_host}/d/x?var=${target}", has: true, stripped: "https://${grafana_host}/d/x?var=${target}"},
		{raw: "//u:p@host/path", has: true, stripped: "//host/path"},
		{raw: "  http://u:p@host", has: true, stripped: "  http://host"},
		// Browser-lenient forms of special schemes.
		{raw: `http:\\u:p@host`, has: true, stripped: `http:\\host`},
		{raw: "http:u:p@host/x", has: true, stripped: "http:host/x"},
		{raw: "http:/u:p@host", has: true, stripped: "http:/host"},
		{raw: "otlp://u:p@collector:4318", has: true, stripped: "otlp://collector:4318"},
		// Browsers drop tabs and newlines anywhere before parsing.
		{raw: "ht\ttp://u:p@host", has: true, stripped: "http://host"},
		{raw: "http:/\t/u:p@host", has: true, stripped: "http://host"},
		{raw: "http://u:p\r\n@host/x", has: true, stripped: "http://host/x"},
		// '@' outside the authority is not userinfo.
		{raw: "http://host/path@x", stripped: "http://host/path@x"},
		{raw: "http://host?q=a@b", stripped: "http://host?q=a@b"},
		{raw: "http://host#a@b", stripped: "http://host#a@b"},
		{raw: `http://host\a@b`, stripped: `http://host\a@b`},
		{raw: "/explore?orgId=1&left=${logs_query}", stripped: "/explore?orgId=1&left=${logs_query}"},
		{raw: "/d/x?mail=ops@example.com", stripped: "/d/x?mail=ops@example.com"},
		{raw: "mailto:ops@example.com", stripped: "mailto:ops@example.com"},
		{raw: "ops@example.com", stripped: "ops@example.com"},
		{raw: "localhost:3100", stripped: "localhost:3100"},
		{raw: "", stripped: ""},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			if got := hasUserinfo(tc.raw); got != tc.has {
				t.Fatalf("hasUserinfo(%q) = %t, want %t", tc.raw, got, tc.has)
			}
			if got := stripUserinfo(tc.raw); got != tc.stripped {
				t.Fatalf("stripUserinfo(%q) = %q, want %q", tc.raw, got, tc.stripped)
			}
		})
	}
}

func TestRedactedMasksChannelConfigAndURLUserinfo(t *testing.T) {
	cfg := RuntimeConfig{
		Dashboards: Endpoint{
			BaseURL:            "https://admin:grafana-pass@grafana.example.internal",
			DashboardTemplates: map[string]string{"sql": "https://u:dash-pass@grafana.example.internal/d/sql"},
			DeepLinkTemplates:  map[string]string{"logs": "/explore?left=${logs_query}"},
		},
		Logs:          Endpoint{BaseURL: "http://logs:loki-pass@loki:3100"},
		Alerting:      Endpoint{BaseURL: "http://am:am-pass@alertmanager:9093"},
		OpenTelemetry: Endpoint{Endpoint: "http://otel:otel-pass@otel-collector:4318"},
		NotificationChannels: []NotificationChannel{{
			ID:            "hook",
			ChannelType:   "webhook",
			TargetRef:     "https://hook:target-pass@hooks.example.internal/alerts",
			CredentialRef: "env/HOOK_TOKEN",
			Config:        map[string]string{"smtp_password": "config-pass", "timeout": "10s"},
		}},
	}
	redacted := cfg.Redacted()

	encoded := fmt.Sprintf("%+v", redacted)
	for _, secret := range []string{"grafana-pass", "dash-pass", "loki-pass", "am-pass", "otel-pass", "target-pass", "config-pass", "10s"} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("redacted config leaks %q: %s", secret, encoded)
		}
	}
	for _, tc := range []struct{ name, got, want string }{
		{"dashboards.base_url", redacted.Dashboards.BaseURL, "https://grafana.example.internal"},
		{"dashboards dashboard template", redacted.Dashboards.DashboardTemplates["sql"], "https://grafana.example.internal/d/sql"},
		{"dashboards deep link template", redacted.Dashboards.DeepLinkTemplates["logs"], "/explore?left=${logs_query}"},
		{"logs.base_url", redacted.Logs.BaseURL, "http://loki:3100"},
		{"alerting.base_url", redacted.Alerting.BaseURL, "http://alertmanager:9093"},
		{"opentelemetry.endpoint", redacted.OpenTelemetry.Endpoint, "http://otel-collector:4318"},
		{"target_ref", redacted.NotificationChannels[0].TargetRef, "https://hooks.example.internal/alerts"},
		{"channel credential_ref", redacted.NotificationChannels[0].CredentialRef, "env/HOOK_TOKEN:<redacted>"},
		{"channel config value", redacted.NotificationChannels[0].Config["smtp_password"], redactedValue},
		{"channel config value", redacted.NotificationChannels[0].Config["timeout"], redactedValue},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
	if cfg.NotificationChannels[0].Config["smtp_password"] != "config-pass" || cfg.Logs.BaseURL != "http://logs:loki-pass@loki:3100" {
		t.Fatal("Redacted mutated the active config")
	}
}

func TestLoadRuntimeConfigRejectsCredentialsEmbeddedInURLs(t *testing.T) {
	const base = `dashboards:
  base_url: %s
  dashboard_templates:
    sql: %s
logs:
  base_url: %s
alerting:
  base_url: %s
opentelemetry:
  endpoint: %s
notification_channels:
  - id: hook
    channel_type: webhook
    target_ref: %s
collectors: []
`
	clean := []string{
		"http://grafana:3000", "/d/sql", "http://loki:3100", "http://alertmanager:9093",
		"http://otel-collector:4318", "http://alertmanager:9093/api/v2/alerts",
	}
	fields := []string{
		"dashboards.base_url", "dashboards.dashboard_templates.sql", "logs.base_url",
		"alerting.base_url", "opentelemetry.endpoint", "notification channel hook target_ref",
	}
	write := func(t *testing.T, values []string) string {
		t.Helper()
		args := make([]any, len(values))
		for i, value := range values {
			args[i] = value
		}
		path := filepath.Join(t.TempDir(), "integrations.yaml")
		if err := os.WriteFile(path, []byte(fmt.Sprintf(base, args...)), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		return path
	}
	if _, err := LoadRuntimeConfig(write(t, clean)); err != nil {
		t.Fatalf("clean config rejected: %v", err)
	}
	for i, field := range fields {
		t.Run(field, func(t *testing.T) {
			values := append([]string(nil), clean...)
			values[i] = strings.Replace(values[i], "://", "://user:hunter2@", 1)
			if !strings.Contains(values[i], "hunter2") {
				values[i] = "https://user:hunter2@grafana.example.internal" + values[i]
			}
			_, err := LoadRuntimeConfig(write(t, values))
			if err == nil {
				t.Fatalf("%s with embedded credentials was accepted", field)
			}
			if !strings.Contains(err.Error(), field) || !strings.Contains(err.Error(), "must not embed credentials") {
				t.Fatalf("error %q does not name %s", err, field)
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Fatalf("error echoes the credential: %v", err)
			}
		})
	}
}

func TestTargetRefCredentials(t *testing.T) {
	for _, tc := range []struct {
		ref      string
		has      bool
		redacted string
	}{
		{ref: "http://alertmanager:9093/api/v2/alerts", redacted: "http://alertmanager:9093/api/v2/alerts"},
		{ref: "https://hook:secret@hooks.example.internal/x", has: true, redacted: "https://hooks.example.internal/x"},
		// A bare user:password@host is read as credentials by tools like curl.
		{ref: "hook:secret@hooks.example.internal/x", has: true, redacted: "hooks.example.internal/x"},
		{ref: "hook:@hooks.example.internal", has: true, redacted: "hooks.example.internal"},
		{ref: "ops@example.com", redacted: "ops@example.com"},
		{ref: "mailto:ops@example.com", redacted: "mailto:ops@example.com"},
		{ref: "#alerts", redacted: "#alerts"},
		{ref: "hooks.example.internal/x?u=a:b@c", redacted: "hooks.example.internal/x?u=a:b@c"},
	} {
		t.Run(tc.ref, func(t *testing.T) {
			if got := targetRefCredentials(tc.ref); got != tc.has {
				t.Fatalf("targetRefCredentials(%q) = %t, want %t", tc.ref, got, tc.has)
			}
			if got := redactTargetRef(tc.ref); got != tc.redacted {
				t.Fatalf("redactTargetRef(%q) = %q, want %q", tc.ref, got, tc.redacted)
			}
		})
	}
}

func TestValidHost(t *testing.T) {
	for _, tc := range []struct {
		host string
		want bool
	}{
		{"sql.example.internal", true},
		{"heartbeat-sqlserver-dev", true},
		{"sql_01.corp", true},
		{"10.0.0.5", true},
		{"2001:db8::5", true},
		{"::1", true},
		// A port, brackets or IPv4 in IPv6 form would be joined into a broken URL.
		{"db.example:1433", false},
		{"[2001:db8::5]", false},
		{"::ffff:10.0.0.5", false},
		{"fe80::1%eth0", false},
		{"user:secret@db", false},
		{"user@db", false},
		{"db/instance", false},
		{`db\instance`, false},
		{"db example", false},
		{"db\texample", false},
		{"db\x00", false},
		{"db%2f", false},
	} {
		t.Run(tc.host, func(t *testing.T) {
			if got := ValidHost(tc.host); got != tc.want {
				t.Fatalf("ValidHost(%q) = %t, want %t", tc.host, got, tc.want)
			}
		})
	}
}

func TestLoadRuntimeConfigRejectsHostThatBreaksTheConnectionURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "integrations.yaml")
	content := []byte(`dashboards:
  base_url: http://grafana:3000
logs:
  base_url: http://loki:3100
alerting:
  base_url: http://alertmanager:9093
collectors:
  - id: sql
    kind: sqlserver
    enabled: true
    config:
      targets:
        - name: core-db
          host: db.example:1433
          port: 1433
`)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	_, err := LoadRuntimeConfig(path)
	if err == nil || !strings.Contains(err.Error(), "collector sql target core-db host must be") {
		t.Fatalf("expected a host error, got %v", err)
	}
	if strings.Contains(err.Error(), "db.example") {
		t.Fatalf("error echoes the host value: %v", err)
	}
}

// outSystemsLogsConfig is an integrations file with one outsystems_logs
// collector; settings is indented under its config and otel is the
// opentelemetry section.
func outSystemsLogsConfig(t *testing.T, otel, settings string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "integrations.yaml")
	content := `dashboards:
  base_url: http://grafana:3000
  dashboard_templates: {}
logs:
  base_url: http://loki:3100
alerting:
  base_url: http://alertmanager:9093
` + otel + `
collectors:
  - id: sql
    kind: sqlserver
    enabled: true
    config:
      environment: prod
      probes: [{name: waits}]
      targets:
        - {name: logging-db, host: sql.prod.local, port: 1433, database_name: master, credential_ref: env/sql}
  - id: oslogs
    kind: outsystems_logs
    enabled: true
    config:
      environment: prod
      probes: [{name: error}, {name: general, timeout_ms: 20000}]
      targets:
        - {name: logging-db, host: sql.prod.local, port: 1433, database_name: OSLogging, credential_ref: env/oslog}
` + settings
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const otelSection = "opentelemetry:\n  endpoint: http://otel-collector:4318\n"

func TestLoadRuntimeConfigOutSystemsLogsDefaults(t *testing.T) {
	cfg, err := LoadRuntimeConfig(outSystemsLogsConfig(t, otelSection, ""))
	if err != nil {
		t.Fatalf("LoadRuntimeConfig: %v", err)
	}
	collectors := cfg.EnabledCollectors(KindOutSystemsLogs)
	if len(collectors) != 1 {
		t.Fatalf("got %d outsystems_logs collectors, want 1", len(collectors))
	}
	got := collectors[0]
	want := OutSystemsLogsConfig{
		Lag: time.Minute, BatchLimit: 5000, InitialLookback: 15 * time.Minute,
		LogTimezone: "UTC", RecountWindow: 10 * time.Minute, MaxDerivedSeries: 50000, LogsEndpoint: "http://otel-collector:4318",
	}
	if got.OutSystemsLogs != want {
		t.Fatalf("settings = %+v, want %+v", got.OutSystemsLogs, want)
	}
	if got.ScrapeInterval != 30*time.Second {
		t.Fatalf("poll interval (scrape_interval) = %s, want the 30s default", got.ScrapeInterval)
	}
	target := got.Targets[0]
	if target.Engine != KindOutSystemsLogs || target.EnvironmentSlug != "prod" || len(target.Probes) != 2 {
		t.Fatalf("unexpected target: %+v", target)
	}
	// The SQL Server collector keeps the zero value.
	if sql := cfg.EnabledCollectors(KindSQLServer)[0]; sql.OutSystemsLogs != (OutSystemsLogsConfig{}) {
		t.Fatalf("sqlserver collector has outsystems_logs settings: %+v", sql.OutSystemsLogs)
	}
	if both := cfg.EnabledCollectors(KindSQLServer, KindOutSystemsLogs); len(both) != 2 || both[0].ID != "oslogs" || both[1].ID != "sql" {
		t.Fatalf("EnabledCollectors of both kinds = %v", both)
	}
}

func TestLoadRuntimeConfigOutSystemsLogsSettings(t *testing.T) {
	cfg, err := LoadRuntimeConfig(outSystemsLogsConfig(t, otelSection,
		"      scrape_interval: 10s\n      lag: 2m\n      batch_limit: 50000\n      initial_lookback: 1h\n      log_timezone: Europe/Lisbon\n      recount_window: 30m\n      max_derived_series: 1000000\n"))
	if err != nil {
		t.Fatalf("LoadRuntimeConfig: %v", err)
	}
	got := cfg.EnabledCollectors(KindOutSystemsLogs)[0]
	want := OutSystemsLogsConfig{
		Lag: 2 * time.Minute, BatchLimit: 50000, InitialLookback: time.Hour,
		LogTimezone: "Europe/Lisbon", RecountWindow: 30 * time.Minute, MaxDerivedSeries: 1000000, LogsEndpoint: "http://otel-collector:4318",
	}
	if got.OutSystemsLogs != want || got.ScrapeInterval != 10*time.Second {
		t.Fatalf("settings = %+v every %s, want %+v every 10s", got.OutSystemsLogs, got.ScrapeInterval, want)
	}
}

// A probe's lag replaces the collector's for that log type only.
func TestLoadRuntimeConfigOutSystemsLogsProbeLag(t *testing.T) {
	path := outSystemsLogsConfig(t, otelSection, "")
	content, _ := os.ReadFile(path)
	content = []byte(strings.Replace(string(content), "{name: general, timeout_ms: 20000}", "{name: general, timeout_ms: 20000, lag: 25m}", 1))
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadRuntimeConfig(path)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig: %v", err)
	}
	probes := cfg.EnabledCollectors(KindOutSystemsLogs)[0].Targets[0].Probes
	if len(probes) != 2 || probes[0].Lag != 0 || probes[1].Lag != 25*time.Minute {
		t.Fatalf("probes = %+v, want error without a lag and general with 25m", probes)
	}
}

func TestLoadRuntimeConfigRejectsInvalidOutSystemsLogsSettings(t *testing.T) {
	tests := []struct {
		name     string
		otel     string
		settings string
		// edit, when set, changes the file content.
		edit    func(string) string
		wantErr string
	}{
		{name: "zero lag", otel: otelSection, settings: "      lag: 0s\n", wantErr: "lag must be positive"},
		{name: "negative lag", otel: otelSection, settings: "      lag: -1m\n", wantErr: "lag must be positive"},
		{name: "unparsable lag", otel: otelSection, settings: "      lag: soon\n", wantErr: "invalid lag for collector oslogs"},
		{name: "negative lookback", otel: otelSection, settings: "      initial_lookback: -15m\n", wantErr: "initial_lookback must be positive"},
		{name: "zero batch", otel: otelSection, settings: "      batch_limit: 0\n", wantErr: "batch_limit must be between 1 and 50000"},
		{name: "batch above the maximum", otel: otelSection, settings: "      batch_limit: 50001\n", wantErr: "batch_limit must be between 1 and 50000"},
		{name: "zero series budget", otel: otelSection, settings: "      max_derived_series: 0\n", wantErr: "max_derived_series must be between 1 and 1000000"},
		{name: "series budget above the maximum", otel: otelSection, settings: "      max_derived_series: 1000001\n", wantErr: "max_derived_series must be between 1 and 1000000"},
		{name: "unknown time zone", otel: otelSection, settings: "      log_timezone: Mars/Olympus\n", wantErr: `log_timezone "Mars/Olympus" must be an IANA time zone name`},
		{name: "host time zone", otel: otelSection, settings: "      log_timezone: Local\n", wantErr: `log_timezone "Local" must be an IANA time zone name`},
		{name: "recount window below 2m", otel: otelSection, settings: "      recount_window: 1m\n", wantErr: "recount_window must be 0s (off) or whole minutes from 2m to 1h0m0s"},
		{name: "recount window above 1h", otel: otelSection, settings: "      recount_window: 61m\n", wantErr: "recount_window must be 0s (off) or whole minutes from 2m to 1h0m0s"},
		{name: "recount window not whole minutes", otel: otelSection, settings: "      recount_window: 90s\n", wantErr: "recount_window must be 0s (off) or whole minutes"},
		{name: "negative recount window", otel: otelSection, settings: "      recount_window: -10m\n", wantErr: "recount_window must be 0s (off)"},
		{
			name: "query_template the recount cannot count", otel: otelSection,
			edit: func(s string) string {
				return strings.Replace(s, "{name: general, timeout_ms: 20000}",
					`{name: general, timeout_ms: 20000, query_template: "SELECT TOP (@limit) Instant FROM dbo.oslog_General WHERE Instant > @from AND Instant <= @to ORDER BY Instant OPTION (RECOMPILE)"}`, 1)
			},
			wantErr: "probe general query_template cannot be recounted: it has an OPTION clause, which a derived table cannot hold; change the query_template or set recount_window: 0s",
		},
		{
			name: "query_template ordered without TOP", otel: otelSection,
			edit: func(s string) string {
				return strings.Replace(s, "{name: general, timeout_ms: 20000}",
					`{name: general, timeout_ms: 20000, query_template: "SELECT Instant FROM dbo.oslog_General WHERE Instant > @from AND Instant <= @to ORDER BY Instant"}`, 1)
			},
			wantErr: "probe general query_template cannot be recounted: it has an ORDER BY clause without TOP or OFFSET, which a derived table cannot hold",
		},
		{name: "zero interval", otel: otelSection, settings: "      scrape_interval: 0s\n", wantErr: "must have positive scrape interval"},
		{name: "no OTLP endpoint", wantErr: "opentelemetry.endpoint is required for outsystems_logs collectors"},
		{
			name: "log type listed twice", otel: otelSection,
			edit: func(s string) string {
				return strings.Replace(s, "{name: general, timeout_ms: 20000}", "{name: error}", 1)
			},
			wantErr: `collector oslogs target logging-db lists log type "error" more than once`,
		},
		{
			name: "zero probe lag", otel: otelSection,
			edit: func(s string) string {
				return strings.Replace(s, "{name: general, timeout_ms: 20000}", "{name: general, lag: 0s}", 1)
			},
			wantErr: "collector oslogs probe general lag must be positive",
		},
		{
			name: "unparsable probe lag", otel: otelSection,
			edit: func(s string) string {
				return strings.Replace(s, "{name: general, timeout_ms: 20000}", "{name: general, lag: soon}", 1)
			},
			wantErr: "invalid lag for collector oslogs probe general",
		},
		{
			name: "probe lag on a sqlserver collector", otel: otelSection,
			edit: func(s string) string {
				return strings.Replace(s, "probes: [{name: waits}]", "probes: [{name: waits, lag: 1m}]", 1)
			},
			wantErr: "collector sql target logging-db probe waits: lag applies only to outsystems_logs collectors",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := outSystemsLogsConfig(t, tt.otel, tt.settings)
			if tt.edit != nil {
				content, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(tt.edit(string(content))), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := LoadRuntimeConfig(path)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// The outsystems_logs settings belong to that kind only; on a SQL Server
// collector they would be silently ignored.
func TestLoadRuntimeConfigRejectsOutSystemsLogsSettingsOnOtherKinds(t *testing.T) {
	for _, setting := range []string{"lag: 1m", "batch_limit: 10", "initial_lookback: 1m", "log_timezone: UTC", "recount_window: 10m", "max_derived_series: 100"} {
		t.Run(setting, func(t *testing.T) {
			path := outSystemsLogsConfig(t, otelSection, "")
			content, _ := os.ReadFile(path)
			content = []byte(strings.Replace(string(content), "      environment: prod\n      probes: [{name: waits}]", "      environment: prod\n      "+setting+"\n      probes: [{name: waits}]", 1))
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
			name, _, _ := strings.Cut(setting, ":")
			_, err := LoadRuntimeConfig(path)
			if want := "collector sql: " + name + " applies only to outsystems_logs collectors"; err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %v, want %q", err, want)
			}
		})
	}
}

// A log collector and a SQL Server collector may read the same database: the
// one-collector-per-target rule is about SQL Server metric series only.
func TestUniqueSQLServerTargetsIgnoresOutSystemsLogs(t *testing.T) {
	path := outSystemsLogsConfig(t, otelSection, "")
	content, _ := os.ReadFile(path)
	content = append(content, []byte(`  - id: oslogs-2
    kind: outsystems_logs
    enabled: true
    config:
      environment: prod
      probes: [{name: error}]
      targets:
        - {name: logging-db, host: sql.prod.local, port: 1433, database_name: master, credential_ref: env/sql}
`)...)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRuntimeConfig(path); err != nil {
		t.Fatalf("LoadRuntimeConfig: %v", err)
	}
}

func TestValidateRejectsLogTypeExtractedByTwoOutSystemsLogsCollectors(t *testing.T) {
	target := func(host, database string, logTypes ...string) TargetRuntimeConfig {
		probes := make([]ProbeRuntimeConfig, len(logTypes))
		for i, logType := range logTypes {
			probes[i] = ProbeRuntimeConfig{Name: logType}
		}
		return TargetRuntimeConfig{Name: "logging-db", Host: host, Port: 1433, DatabaseName: database, Probes: probes}
	}
	collector := func(id string, enabled bool, targets ...TargetRuntimeConfig) CollectorRuntimeConfig {
		return CollectorRuntimeConfig{
			ID: id, Kind: KindOutSystemsLogs, Enabled: enabled, ScrapeInterval: time.Minute, Targets: targets,
			OutSystemsLogs: OutSystemsLogsConfig{Lag: time.Minute, InitialLookback: time.Minute, BatchLimit: 100, LogTimezone: "UTC", MaxDerivedSeries: 100},
		}
	}
	tests := []struct {
		name       string
		collectors []CollectorRuntimeConfig
		wantErr    bool
	}{
		{"same log type of the same database", []CollectorRuntimeConfig{
			collector("a", true, target("sql.prod.local", "OSLogging", "error")),
			collector("b", true, target("SQL.prod.local", "oslogging", "general", "error")),
		}, true},
		{"different log types", []CollectorRuntimeConfig{
			collector("a", true, target("sql.prod.local", "OSLogging", "error")),
			collector("b", true, target("sql.prod.local", "OSLogging", "general")),
		}, false},
		{"another database", []CollectorRuntimeConfig{
			collector("a", true, target("sql.prod.local", "OSLogging", "error")),
			collector("b", true, target("sql.prod.local", "OSLoggingArchive", "error")),
		}, false},
		{"second collector disabled", []CollectorRuntimeConfig{
			collector("a", true, target("sql.prod.local", "OSLogging", "error")),
			collector("b", false, target("sql.prod.local", "OSLogging", "error")),
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validate(RuntimeConfig{
				Dashboards:    Endpoint{BaseURL: "http://grafana:3000"},
				Logs:          Endpoint{BaseURL: "http://loki:3100"},
				Alerting:      Endpoint{BaseURL: "http://alertmanager:9093"},
				OpenTelemetry: Endpoint{Endpoint: "http://otel-collector:4318"},
				Collectors:    tt.collectors,
			})
			if tt.wantErr != (err != nil) {
				t.Fatalf("wantErr %v, got %v", tt.wantErr, err)
			}
			if err != nil && !strings.Contains(err.Error(), `log type "error" of database oslogging on SQL.prod.local:1433 is extracted by collectors a and b`) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestDiffCollectorsAcrossKinds(t *testing.T) {
	logs := CollectorRuntimeConfig{ID: "a", Kind: KindOutSystemsLogs, Enabled: true, OutSystemsLogs: OutSystemsLogsConfig{LogsEndpoint: "http://otel-a:4318"}}
	sql := CollectorRuntimeConfig{ID: "b", Kind: KindSQLServer, Enabled: true}
	other := CollectorRuntimeConfig{ID: "c", Kind: "oracle", Enabled: true}
	previous := RuntimeConfig{Collectors: []CollectorRuntimeConfig{logs, sql, other}}

	moved := logs
	moved.OutSystemsLogs.LogsEndpoint = "http://otel-b:4318"
	changedKind := sql
	changedKind.Kind = KindOutSystemsLogs
	next := RuntimeConfig{Collectors: []CollectorRuntimeConfig{moved, changedKind, other}}

	diff := DiffCollectors(previous, next, KindSQLServer, KindOutSystemsLogs)
	if len(diff.Updated) != 2 || diff.Updated[0].ID != "a" || diff.Updated[1].ID != "b" {
		t.Fatalf("Updated = %v, want a (new OTLP endpoint) and b (new kind)", diff.Updated)
	}
	if len(diff.Added)+len(diff.Removed)+len(diff.Unchanged) != 0 {
		t.Fatalf("unexpected diff %+v; collector c is of another kind", diff)
	}
}

func TestManagerRunsExtraValidators(t *testing.T) {
	path := outSystemsLogsConfig(t, otelSection, "")
	reject := errors.New("unknown log type")
	var fail bool
	validator := WithValidator(func(cfg RuntimeConfig) error {
		if fail {
			return reject
		}
		return nil
	})
	manager, err := NewManager(path, validator)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	version := manager.Snapshot().Config.Version
	fail = true
	if _, err := manager.Reload(); !errors.Is(err, ErrInvalidCandidate) || !strings.Contains(err.Error(), "unknown log type") {
		t.Fatalf("Reload error = %v, want an invalid candidate", err)
	}
	if got := manager.Snapshot().Config.Version; got != version {
		t.Fatal("a candidate that failed the validator replaced the active config")
	}
	if _, err := NewManager(path, validator); !errors.Is(err, reject) {
		t.Fatalf("NewManager error = %v, want the validator's error", err)
	}
}

// writeProbeConfig writes outSystemsLogsConfig with its probe lists replaced:
// sqlProbes for the sqlserver collector, logProbes for the outsystems_logs
// one (flow-style YAML lists).
func writeProbeConfig(t *testing.T, sqlProbes, logProbes string) string {
	t.Helper()
	path := outSystemsLogsConfig(t, otelSection, "")
	content, _ := os.ReadFile(path)
	text := strings.Replace(string(content), "probes: [{name: waits}]", "probes: "+sqlProbes, 1)
	text = strings.Replace(text, "probes: [{name: error}, {name: general, timeout_ms: 20000}]", "probes: "+logProbes, 1)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Every query_template, SQL Server or OutSystems, default probe list or a
// target's own, must be one read-only SELECT; the error names the probe and
// the reason but never quotes the query, which could hold a secret.
func TestLoadRuntimeConfigRejectsUnsafeQueryTemplates(t *testing.T) {
	const secret = "hunter2-literal"
	unsafe := `"SELECT 1 AS x; EXEC sp_addlogin 'eve', '` + secret + `'"`
	for _, tc := range []struct {
		name, sqlProbes, logProbes, want string
	}{
		{"sqlserver default probe", "[{name: waits, query_template: " + unsafe + "}]", "[{name: error}]",
			"collector sql probe waits query_template must be one read-only SELECT: it has more than one statement"},
		{"outsystems_logs log type", "[{name: waits}]", `[{name: error, query_template: "SELECT TOP (@limit) Instant INTO #x FROM dbo.v"}]`,
			"collector oslogs probe error query_template must be one read-only SELECT: it contains INTO"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadRuntimeConfig(writeProbeConfig(t, tc.sqlProbes, tc.logProbes))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "sp_addlogin") {
				t.Fatalf("error %q quotes the query", err)
			}
		})
	}
	t.Run("target probe", func(t *testing.T) {
		path := writeProbeConfig(t, "[{name: waits}]", "[{name: error}]")
		content, _ := os.ReadFile(path)
		text := strings.Replace(string(content), "credential_ref: env/sql}", `credential_ref: env/sql, probes: [{name: blocking, query_template: "WITH a AS (SELECT 1 AS x) DELETE FROM a"}]}`, 1)
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadRuntimeConfig(path)
		if want := "collector sql target logging-db probe blocking query_template must be one read-only SELECT: it contains DELETE"; err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want it to contain %q", err, want)
		}
	})
	t.Run("safe overrides load", func(t *testing.T) {
		path := writeProbeConfig(t, `[{name: waits, query_template: "SELECT wait_type, wait_time_ms FROM sys.dm_os_wait_stats;"}]`,
			`[{name: error, query_template: "SELECT TOP (@limit) Instant FROM dbo.oslog_Error WHERE Instant > @from AND Instant <= @to ORDER BY Instant"}]`)
		if _, err := LoadRuntimeConfig(path); err != nil {
			t.Fatalf("LoadRuntimeConfig: %v", err)
		}
	})
}

func TestLoadRuntimeConfigProbeMaxRows(t *testing.T) {
	for _, tc := range []struct {
		name, sqlProbes, logProbes, want string
		wantMaxRows                      int
	}{
		{"unset", "[{name: waits}]", "[{name: error}]", "", 0},
		{"set", "[{name: waits, max_rows: 500}]", "[{name: error}]", "", 500},
		{"at the maximum", fmt.Sprintf("[{name: waits, max_rows: %d}]", MaxProbeMaxRows), "[{name: error}]", "", MaxProbeMaxRows},
		{"negative", "[{name: waits, max_rows: -1}]", "[{name: error}]", fmt.Sprintf("collector sql probe waits max_rows must be between 1 and %d", MaxProbeMaxRows), 0},
		{"above the maximum", fmt.Sprintf("[{name: waits, max_rows: %d}]", MaxProbeMaxRows+1), "[{name: error}]", fmt.Sprintf("collector sql probe waits max_rows must be between 1 and %d", MaxProbeMaxRows), 0},
		{"on an outsystems_logs log type", "[{name: waits}]", "[{name: error, max_rows: 10}]", "collector oslogs probe error: max_rows applies only to sqlserver collectors", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadRuntimeConfig(writeProbeConfig(t, tc.sqlProbes, tc.logProbes))
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadRuntimeConfig: %v", err)
			}
			if got := cfg.EnabledCollectors(KindSQLServer)[0].Targets[0].Probes[0].MaxRows; got != tc.wantMaxRows {
				t.Fatalf("MaxRows = %d, want %d", got, tc.wantMaxRows)
			}
		})
	}
}

// recount_window 0s turns the recount off.
func TestLoadRuntimeConfigOutSystemsLogsRecountOff(t *testing.T) {
	cfg, err := LoadRuntimeConfig(outSystemsLogsConfig(t, otelSection, "      recount_window: 0s\n"))
	if err != nil {
		t.Fatalf("LoadRuntimeConfig: %v", err)
	}
	if got := cfg.EnabledCollectors(KindOutSystemsLogs)[0].OutSystemsLogs.RecountWindow; got != 0 {
		t.Fatalf("recount_window = %s, want 0 (off)", got)
	}
}
