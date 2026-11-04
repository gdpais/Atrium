package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestTemplateVariables(t *testing.T) {
	for _, tc := range []struct {
		template string
		want     []string
		wantErr  string
	}{
		{template: "/d/sqlserver-overview", want: nil},
		{template: "/d/x?var-target=${target}&var-environment=${environment}&again=${target}", want: []string{"target", "environment"}},
		{template: "/explore?left=${logs_query}", want: []string{"logs_query"}},
		{template: "/d/x?var=${target", wantErr: "no closing }"},
		{template: "/d/x?var=${}", wantErr: "malformed variable ${}"},
		{template: "/d/x?var=${Target}", wantErr: "malformed variable ${Target}"},
		{template: "/d/x?var=${a b}", wantErr: "malformed variable"},
	} {
		got, err := TemplateVariables(tc.template)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("TemplateVariables(%q) error = %v, want it to contain %q", tc.template, err, tc.wantErr)
			}
			continue
		}
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("TemplateVariables(%q) = %v, %v; want %v", tc.template, got, err, tc.want)
		}
	}
}

// A typo fails the file, naming the template, instead of rendering a broken
// link, or none. The loader itself accepts it: the collector reads no
// template.
func TestValidateLinkTemplates(t *testing.T) {
	const base = "dashboards:\n  base_url: http://grafana:3000\n"
	for _, tc := range []struct {
		name, content, wantErr string
	}{
		{name: "the shipped templates", content: base +
			"  dashboard_templates:\n    sqlserver-overview: /d/sqlserver-overview?from=${from}&to=${to}\n" +
			"  deep_link_templates:\n    logs-explore: /explore?orgId=1&left=${logs_query}\n" +
			"    sqlserver-target: /d/sqlserver-overview?var-environment=${environment}&var-target=${target}\n" +
			"    outsystems-application: /d/outsystems-overview?var-environment=${environment}&var-application=${application}&from=${from}&to=${to}\n"},
		{name: "unknown deep-link variable", content: base + "  deep_link_templates:\n    x: /d/x?var-target=${targt}\n",
			wantErr: "dashboards.deep_link_templates.x names ${targt}, want only ${application}, ${environment}, ${from}, ${logs_query}, ${target}, ${to}"},
		{name: "entity variable in a dashboard", content: base + "  dashboard_templates:\n    x: /d/x?var-target=${target}\n",
			wantErr: "dashboards.dashboard_templates.x names ${target}, want only ${from}, ${to}"},
		{name: "absolute URL", content: base + "  deep_link_templates:\n    x: https://elsewhere.example/d/x\n",
			wantErr: "dashboards.deep_link_templates.x must be a path on dashboards.base_url"},
		{name: "scheme-relative URL", content: base + "  deep_link_templates:\n    x: //elsewhere.example/d/x\n",
			wantErr: "must be a path on dashboards.base_url, starting with a single /"},
		{name: "unclosed variable", content: base + "  deep_link_templates:\n    x: /d/x?var-target=${target\n",
			wantErr: "dashboards.deep_link_templates.x has a ${ with no closing }"},
		{name: "templates without a base URL", content: "dashboards:\n  deep_link_templates:\n    x: /d/x\n",
			wantErr: "dashboards.deep_link_templates.x needs dashboards.base_url"},
		{name: "a base URL a browser does not open as a page", content: "dashboards:\n  base_url: ftp://grafana:3000\n  deep_link_templates:\n    x: /d/x\n",
			wantErr: "dashboards.base_url must be an http or https URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "integrations.yaml")
			if err := os.WriteFile(path, []byte(tc.content+"collectors: []\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadRuntimeConfig(path)
			if err != nil {
				t.Fatalf("the loader refuses it: %v", err)
			}
			err = ValidateLinkTemplates(cfg.Dashboards)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
