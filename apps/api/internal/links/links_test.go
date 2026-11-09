package links

import (
	"encoding/json"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"atrium/internal/config"
)

// shipped is the dashboards section of config/integrations.yaml and the
// chart's values, read through the loader.
func shipped(t *testing.T) config.RuntimeConfig {
	t.Helper()
	cfg, err := config.LoadRuntimeConfig(filepath.Join("..", "..", "..", "..", "config", "integrations.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func grafanaConfig(deepLinks, dashboards map[string]string) config.RuntimeConfig {
	return config.RuntimeConfig{Platform: config.PlatformGrafana, Dashboards: config.Endpoint{
		BaseURL: "https://grafana.example.internal/", DeepLinkTemplates: deepLinks, DashboardTemplates: dashboards,
	}}
}

func TestEscape(t *testing.T) {
	for in, want := range map[string]string{
		"finance-prod_1.a~": "finance-prod_1.a~",
		"a b&c=d?e#f/g":     "a%20b%26c%3Dd%3Fe%23f%2Fg",
		"Ünïcode":           "%C3%9Cn%C3%AFcode",
		"%41":               "%2541",
	} {
		if got := escape(in); got != want {
			t.Errorf("escape(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDashboards(t *testing.T) {
	cfg := grafanaConfig(nil, map[string]string{
		"b-ranged": "/d/b?from=${from}&to=${to}",
		"a-plain":  "/d/a",
	})
	got := Dashboards(cfg, Window{})
	want := []Link{
		{Name: "a-plain", Kind: KindDashboard, URL: "https://grafana.example.internal/d/a"},
		{Name: "b-ranged", Kind: KindDashboard, URL: "https://grafana.example.internal/d/b?from=now-1h&to=now"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Dashboards = %+v, want %+v", got, want)
	}
	from, to := time.UnixMilli(1762250400000), time.UnixMilli(1762254000000)
	if got := Dashboards(cfg, Window{From: from, To: to}); got[1].URL != "https://grafana.example.internal/d/b?from=1762250400000&to=1762254000000" {
		t.Errorf("with a window: %s", got[1].URL)
	}
}

// A template is rendered for an entity that has all its variables; one
// naming ${application} or ${logs_query} once per application name.
func TestFor(t *testing.T) {
	cfg := grafanaConfig(map[string]string{
		"app":      "/d/app?var-environment=${environment}&var-application=${application}",
		"env-only": "/d/env?var-environment=${environment}",
		"target":   "/d/sql?var-environment=${environment}&var-target=${target}&from=${from}",
	}, nil)

	got := For(cfg, Scope{Environment: "prod", Applications: []string{"Shop", "", "Billing", "Shop"}}, Window{})
	want := []Link{
		{Name: "app", Kind: KindDeepLink, URL: "https://grafana.example.internal/d/app?var-environment=prod&var-application=Billing", ApplicationName: "Billing"},
		{Name: "app", Kind: KindDeepLink, URL: "https://grafana.example.internal/d/app?var-environment=prod&var-application=Shop", ApplicationName: "Shop"},
		{Name: "env-only", Kind: KindDeepLink, URL: "https://grafana.example.internal/d/env?var-environment=prod"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("application: %+v\nwant %+v", got, want)
	}

	got = For(cfg, Scope{Environment: "prod"}, Window{})
	if len(got) != 1 || got[0].Name != "env-only" {
		t.Errorf("application without sources: %+v, want env-only alone", got)
	}

	got = For(cfg, Scope{Environment: "prod", Target: "finance prod"}, Window{})
	want = []Link{
		{Name: "env-only", Kind: KindDeepLink, URL: "https://grafana.example.internal/d/env?var-environment=prod"},
		{Name: "target", Kind: KindDeepLink, URL: "https://grafana.example.internal/d/sql?var-environment=prod&var-target=finance%20prod&from=now-1h"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("target: %+v\nwant %+v", got, want)
	}
}

// A value cannot add a parameter, a path or a fragment to the link, nor
// leave the platform's origin.
func TestForEscapesValues(t *testing.T) {
	cfg := grafanaConfig(map[string]string{"target": "/d/sql?var-target=${target}"}, nil)
	got := For(cfg, Scope{Environment: "prod", Target: "x&orgId=2#/../../evil"}, Window{})
	u, err := url.Parse(got[0].URL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "grafana.example.internal" || u.Path != "/d/sql" || u.Fragment != "" || len(u.Query()) != 1 || u.Query().Get("var-target") != "x&orgId=2#/../../evil" {
		t.Errorf("URL %s: host %q path %q query %v fragment %q", got[0].URL, u.Host, u.Path, u.Query(), u.Fragment)
	}
}

func TestNoAdapterNoLinks(t *testing.T) {
	for _, platform := range []string{config.PlatformGeneric, ""} {
		cfg := grafanaConfig(map[string]string{"env": "/d/env?var-environment=${environment}"}, map[string]string{"a": "/d/a"})
		cfg.Platform = platform
		if got := For(cfg, Scope{Environment: "prod"}, Window{}); len(got) != 0 || got == nil {
			t.Errorf("platform %q: For = %#v, want empty", platform, got)
		}
		if got := Dashboards(cfg, Window{}); len(got) != 0 || got == nil {
			t.Errorf("platform %q: Dashboards = %#v, want empty", platform, got)
		}
	}
}

// The shipped logs-explore template opens Grafana Explore on Loki with the
// application's stream selector, as the OutSystems dashboards query it.
func TestGrafanaLogsExplore(t *testing.T) {
	cfg := shipped(t)
	from := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	got := For(cfg, Scope{Environment: "local-dev", Applications: []string{`Odd "Name" \ App`}}, Window{From: from, To: from.Add(time.Hour)})
	var explore *Link
	for i := range got {
		if got[i].Name == "logs-explore" {
			explore = &got[i]
		}
	}
	if explore == nil {
		t.Fatalf("no logs-explore link in %+v", got)
	}
	u, err := url.Parse(explore.URL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme+"://"+u.Host != cfg.Dashboards.BaseURL || u.Path != "/explore" || u.Query().Get("orgId") != "1" {
		t.Fatalf("URL %s", explore.URL)
	}
	var pane explorePane
	if err := json.Unmarshal([]byte(u.Query().Get("left")), &pane); err != nil {
		t.Fatalf("left is not JSON: %v", err)
	}
	wantExpr := `{service_name="Odd \"Name\" \\ App", deployment_environment_name="local-dev"}`
	if pane.Datasource != "loki" || len(pane.Queries) != 1 || pane.Queries[0].Expr != wantExpr ||
		pane.Queries[0].Datasource != (exploreDatasource{Type: "loki", UID: "loki"}) ||
		pane.Range != (exploreRange{From: strconv.FormatInt(from.UnixMilli(), 10), To: strconv.FormatInt(from.Add(time.Hour).UnixMilli(), 10)}) {
		t.Errorf("pane = %+v, want expr %s", pane, wantExpr)
	}
	if explore.ApplicationName != `Odd "Name" \ App` {
		t.Errorf("ApplicationName = %q", explore.ApplicationName)
	}
}

// Every shipped template renders for the entity it was written for, so a
// template whose variables no entity has fails here.
func TestShippedTemplatesRender(t *testing.T) {
	cfg := shipped(t)
	rendered := map[string]bool{}
	for _, l := range Dashboards(cfg, Window{}) {
		rendered[l.Name] = true
	}
	for _, scope := range []Scope{
		{Environment: "local-dev", Applications: []string{"Shop"}},
		{Environment: "local-dev", Target: "sqlserver-dev"},
	} {
		for _, l := range For(cfg, scope, Window{}) {
			rendered[l.Name] = true
			if strings.Contains(l.URL, "${") {
				t.Errorf("%s: %s keeps a variable", l.Name, l.URL)
			}
		}
	}
	for _, templates := range []map[string]string{cfg.Dashboards.DashboardTemplates, cfg.Dashboards.DeepLinkTemplates} {
		for name := range templates {
			if !rendered[name] {
				t.Errorf("template %s renders for no entity", name)
			}
		}
	}
}

func TestSource(t *testing.T) {
	var none *Source
	if cfg, err := none.Config(); err != nil || cfg.Platform != "" {
		t.Errorf("nil Source: %+v, %v", cfg, err)
	}
	if NewSource("", nil) != nil {
		t.Error("NewSource(\"\") is not nil")
	}

	path := filepath.Join(t.TempDir(), "integrations.yaml")
	clock := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	s := &Source{path: path, logger: discard(), refresh: time.Minute, now: func() time.Time { return clock }}
	if _, err := s.Config(); err != ErrUnavailable {
		t.Fatalf("no file: %v, want ErrUnavailable", err)
	}
	write := func(base string) {
		content := "dashboards:\n  base_url: " + base + "\n  deep_link_templates:\n    env: /d/env?var-environment=${environment}\ncollectors: []\n"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("http://grafana-1:3000")
	if _, err := s.Config(); err != ErrUnavailable {
		t.Errorf("before the refresh: %v, want the failure kept", err)
	}
	clock = clock.Add(time.Minute)
	if cfg, err := s.Config(); err != nil || cfg.Dashboards.BaseURL != "http://grafana-1:3000" {
		t.Fatalf("after the refresh: %+v, %v", cfg.Dashboards, err)
	}

	// An invalid file never replaces a valid one.
	if err := os.WriteFile(path, []byte("dashboards:\n  base_url: http://grafana-2:3000\n  deep_link_templates:\n    env: /d/env?var-environment=${envronment}\ncollectors: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Minute)
	if cfg, err := s.Config(); err != nil || cfg.Dashboards.BaseURL != "http://grafana-1:3000" {
		t.Errorf("invalid file: %+v, %v; want the last valid one", cfg.Dashboards, err)
	}
	write("http://grafana-3:3000")
	clock = clock.Add(30 * time.Second)
	if cfg, _ := s.Config(); cfg.Dashboards.BaseURL != "http://grafana-1:3000" {
		t.Errorf("within the refresh: %s, want the file not reread", cfg.Dashboards.BaseURL)
	}
	clock = clock.Add(30 * time.Second)
	if cfg, _ := s.Config(); cfg.Dashboards.BaseURL != "http://grafana-3:3000" {
		t.Errorf("after the refresh: %s, want the new file", cfg.Dashboards.BaseURL)
	}
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }
