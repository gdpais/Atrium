package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"heartbeat/apps/api/internal/auth"
	"heartbeat/apps/api/internal/inventory"
	"heartbeat/apps/api/internal/links"
	"heartbeat/internal/config"
)

type fakeTelemetry struct {
	telemetry inventory.Telemetry
	err       error
	calls     []string
	id        uuid.UUID
}

func (f *fakeTelemetry) ApplicationTelemetry(_ context.Context, id uuid.UUID) (inventory.Telemetry, error) {
	f.calls, f.id = append(f.calls, "Application"), id
	return f.telemetry, f.err
}

func (f *fakeTelemetry) DatabaseTargetTelemetry(_ context.Context, id uuid.UUID) (inventory.Telemetry, error) {
	f.calls, f.id = append(f.calls, "DatabaseTarget"), id
	return f.telemetry, f.err
}

type fakeLinkConfig struct {
	cfg config.RuntimeConfig
	err error
}

func (f fakeLinkConfig) Config() (config.RuntimeConfig, error) { return f.cfg, f.err }

var linkConfig = fakeLinkConfig{cfg: config.RuntimeConfig{Platform: config.PlatformGrafana, Dashboards: config.Endpoint{
	BaseURL:            "http://grafana.test:3000",
	DashboardTemplates: map[string]string{"overview": "/d/overview?from=${from}&to=${to}"},
	DeepLinkTemplates: map[string]string{
		"app":    "/d/app?var-environment=${environment}&var-application=${application}",
		"target": "/d/sql?var-environment=${environment}&var-target=${target}",
	},
}}}

func linkServer(t *testing.T, telemetry *fakeTelemetry, cfg LinkConfig) *Server {
	t.Helper()
	return newServer(t, Options{Auth: roleAuth{}, Site: testSite, Telemetry: telemetry, Links: cfg})
}

const someID = "3d6f1a2b-7c8d-4e9f-a0b1-c2d3e4f5a6b7"

// Anyone logged in reads links.
func TestLinkRoles(t *testing.T) {
	for _, path := range []string{"/api/v1/dashboards", "/api/v1/applications/" + someID + "/links", "/api/v1/database-targets/" + someID + "/links"} {
		if rec := send(t, linkServer(t, &fakeTelemetry{}, linkConfig), "", http.MethodGet, path, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a session: %d, want 401", path, rec.Code)
		}
		for _, role := range auth.Roles {
			if rec := send(t, linkServer(t, &fakeTelemetry{}, linkConfig), string(role), http.MethodGet, path, ""); rec.Code != http.StatusOK {
				t.Errorf("GET %s as %s: %d %s, want 200", path, role, rec.Code, rec.Body)
			}
		}
	}
}

type linkList struct {
	Items []struct {
		Name            string  `json:"name"`
		Kind            string  `json:"kind"`
		URL             string  `json:"url"`
		ApplicationName *string `json:"application_name"`
	} `json:"items"`
}

func decodeLinks(t *testing.T, body string) linkList {
	t.Helper()
	var out linkList
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("body %s: %v", body, err)
	}
	return out
}

func TestApplicationLinks(t *testing.T) {
	telemetry := &fakeTelemetry{telemetry: inventory.Telemetry{EnvironmentSlug: "prod", ApplicationNames: []string{"Shop"}}}
	rec := send(t, linkServer(t, telemetry, linkConfig), "viewer", http.MethodGet,
		"/api/v1/applications/"+someID+"/links?from=2026-11-04T09:00:00Z&to=2026-11-04T10:00:00Z", "")
	if rec.Code != http.StatusOK || telemetry.id.String() != someID || telemetry.calls[0] != "Application" {
		t.Fatalf("GET = %d %s, calls %v %s", rec.Code, rec.Body, telemetry.calls, telemetry.id)
	}
	got := decodeLinks(t, rec.Body.String())
	if len(got.Items) != 1 || got.Items[0].Name != "app" || got.Items[0].Kind != "deep_link" ||
		got.Items[0].URL != "http://grafana.test:3000/d/app?var-environment=prod&var-application=Shop" ||
		got.Items[0].ApplicationName == nil || *got.Items[0].ApplicationName != "Shop" {
		t.Errorf("links = %s", rec.Body)
	}
}

func TestDatabaseTargetLinks(t *testing.T) {
	telemetry := &fakeTelemetry{telemetry: inventory.Telemetry{EnvironmentSlug: "prod", TargetName: "finance-prod"}}
	rec := send(t, linkServer(t, telemetry, linkConfig), "viewer", http.MethodGet, "/api/v1/database-targets/"+someID+"/links", "")
	got := decodeLinks(t, rec.Body.String())
	if rec.Code != http.StatusOK || telemetry.calls[0] != "DatabaseTarget" || len(got.Items) != 1 ||
		got.Items[0].URL != "http://grafana.test:3000/d/sql?var-environment=prod&var-target=finance-prod" || got.Items[0].ApplicationName != nil {
		t.Errorf("GET = %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "application_name") {
		t.Errorf("a target's link has an application_name: %s", rec.Body)
	}
}

func TestListDashboards(t *testing.T) {
	rec := send(t, linkServer(t, &fakeTelemetry{}, linkConfig), "viewer", http.MethodGet,
		"/api/v1/dashboards?from="+url.QueryEscape("2026-11-04T09:00:00+01:00")+"&to=2026-11-04T09:30:00Z", "")
	got := decodeLinks(t, rec.Body.String())
	if rec.Code != http.StatusOK || len(got.Items) != 1 || got.Items[0].Kind != "dashboard" ||
		got.Items[0].URL != "http://grafana.test:3000/d/overview?from=1793779200000&to=1793784600000" {
		t.Errorf("GET = %d %s", rec.Code, rec.Body)
	}
	// No config at all: no links, not an error.
	rec = send(t, newServer(t, Options{Auth: roleAuth{}, Site: testSite}), "viewer", http.MethodGet, "/api/v1/dashboards", "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"items":[]}` {
		t.Errorf("without a config: %d %s, want 200 and no items", rec.Code, rec.Body)
	}
	var none *links.Source
	rec = send(t, linkServer(t, &fakeTelemetry{}, none), "viewer", http.MethodGet, "/api/v1/dashboards", "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"items":[]}` {
		t.Errorf("with a nil Source: %d %s, want 200 and no items", rec.Code, rec.Body)
	}
}

func TestLinkFailures(t *testing.T) {
	appPath := "/api/v1/applications/" + someID + "/links"
	for _, tc := range []struct {
		name, path string
		telemetry  *fakeTelemetry
		cfg        LinkConfig
		want       int
		detail     string
	}{
		{"from without to", appPath + "?from=2026-11-04T09:00:00Z", &fakeTelemetry{}, linkConfig, http.StatusBadRequest, "give both"},
		{"to before from", "/api/v1/dashboards?from=2026-11-04T09:00:00Z&to=2026-11-04T08:00:00Z", &fakeTelemetry{}, linkConfig, http.StatusBadRequest, "after from"},
		{"not a time", "/api/v1/database-targets/" + someID + "/links?from=yesterday&to=today", &fakeTelemetry{}, linkConfig, http.StatusBadRequest, "from"},
		{"no application", appPath, &fakeTelemetry{err: inventory.ErrNotFound}, linkConfig, http.StatusNotFound, "no application has this ID"},
		{"no target", "/api/v1/database-targets/" + someID + "/links", &fakeTelemetry{err: inventory.ErrNotFound}, linkConfig, http.StatusNotFound, "no database target has this ID"},
		{"no valid config", appPath, &fakeTelemetry{}, fakeLinkConfig{err: links.ErrUnavailable}, http.StatusServiceUnavailable, "no valid integrations.yaml"},
		{"dashboards without a valid config", "/api/v1/dashboards", &fakeTelemetry{}, fakeLinkConfig{err: links.ErrUnavailable}, http.StatusServiceUnavailable, "no valid integrations.yaml"},
		{"lookup fails", appPath, &fakeTelemetry{err: errors.New("postgres down")}, linkConfig, http.StatusInternalServerError, "could not serve"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := send(t, linkServer(t, tc.telemetry, tc.cfg), "viewer", http.MethodGet, tc.path, "")
			if rec.Code != tc.want || !strings.Contains(rec.Body.String(), tc.detail) {
				t.Errorf("GET %s = %d %s, want %d with %q", tc.path, rec.Code, rec.Body, tc.want, tc.detail)
			}
			if tc.want == http.StatusBadRequest && len(tc.telemetry.calls) != 0 {
				t.Errorf("a bad request looked up %v", tc.telemetry.calls)
			}
		})
	}
}
