package collectors

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	atriumconfig "atrium/internal/config"
)

const token = "admin-token-for-the-test"

// adminServer serves GET /admin/config as the collector does: the redacted
// runtime config of collectors, encoded from the collector's own type, so
// that a renamed field breaks this test before it breaks the observation.
func adminServer(t *testing.T, status int, collectors []atriumconfig.CollectorRuntimeConfig) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/admin/config" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version": "abc", "config": atriumconfig.RuntimeConfig{Collectors: collectors}.Redacted(),
		})
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

func target(name, env string) atriumconfig.TargetRuntimeConfig {
	return atriumconfig.TargetRuntimeConfig{Name: name, EnvironmentSlug: env, CredentialRef: "env/sqlserver-dev"}
}

func TestCoverageFollowsTheCollectorsRule(t *testing.T) {
	server, _ := adminServer(t, http.StatusOK, []atriumconfig.CollectorRuntimeConfig{
		{ID: "oslogs-prod", Kind: KindOutSystemsLogs, Enabled: true, Environment: "prod", Targets: []atriumconfig.TargetRuntimeConfig{target("logging-db", "prod")}},
		// Two targets, one selected: only the selected one's environment.
		{ID: "oslogs-shared", Kind: KindOutSystemsLogs, Enabled: true, TargetNames: []string{"eu"},
			Targets: []atriumconfig.TargetRuntimeConfig{target("eu", "prod"), target("us", "us-prod")}},
		{ID: "oslogs-off", Kind: KindOutSystemsLogs, Enabled: false, Targets: []atriumconfig.TargetRuntimeConfig{target("db", "staging")}},
		{ID: "sql-staging", Kind: "sqlserver", Enabled: true, Targets: []atriumconfig.TargetRuntimeConfig{target("db", "staging")}},
		{ID: "oslogs-unlabelled", Kind: KindOutSystemsLogs, Enabled: true, Targets: []atriumconfig.TargetRuntimeConfig{target("db", "")}},
	})
	c := &Client{URL: server.URL + "/", Token: token}
	got, err := c.OutSystemsLogs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"prod": {"oslogs-prod", "oslogs-shared"}}
	if !reflect.DeepEqual(got.ByEnvironment, want) {
		t.Fatalf("coverage %v, want %v", got.ByEnvironment, want)
	}
	if got.CheckedAt.IsZero() {
		t.Fatal("no CheckedAt")
	}
}

// Every failure is an error that says what failed, and none quotes the
// token.
func TestFailuresAreReportedNotRaised(t *testing.T) {
	ok, _ := adminServer(t, http.StatusOK, nil)
	broken, _ := adminServer(t, http.StatusServiceUnavailable, nil)
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) }))
	t.Cleanup(garbage.Close)
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	t.Cleanup(hang.Close)
	for _, tc := range []struct {
		name   string
		client *Client
		want   string
	}{
		{"no URL", &Client{}, "not configured"},
		{"wrong token", &Client{URL: ok.URL, Token: "guess"}, "refused the API's admin token (401)"},
		{"server error", &Client{URL: broken.URL, Token: token}, "answered 503"},
		{"not the config", &Client{URL: garbage.URL, Token: token}, "not its admin config"},
		{"no answer", &Client{URL: hang.URL, Token: token, HTTP: &http.Client{Timeout: 50 * time.Millisecond}}, "did not answer"},
		{"nothing listening", &Client{URL: "http://127.0.0.1:1", Token: token}, "did not answer"},
	} {
		_, err := tc.client.OutSystemsLogs(context.Background())
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), token) {
			t.Errorf("%s: the error quotes the token: %v", tc.name, err)
		}
	}
	var nilClient *Client
	if _, err := nilClient.OutSystemsLogs(context.Background()); err != ErrNotConfigured {
		t.Errorf("nil client: %v", err)
	}
}

// An answer, or a failure, is kept for TTL: a list of sources or a
// wallboard asks the collector once every TTL at most.
func TestAnswersAreKeptForTTL(t *testing.T) {
	server, calls := adminServer(t, http.StatusOK, nil)
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	c := &Client{URL: server.URL, Token: token, TTL: time.Minute, now: func() time.Time { return now }}
	for range 3 {
		if _, err := c.OutSystemsLogs(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("%d requests within the TTL, want 1", calls.Load())
	}
	now = now.Add(time.Minute)
	if _, err := c.OutSystemsLogs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("%d requests after the TTL, want 2", calls.Load())
	}
}

// A caller that gives up does not decide for everyone: the shared request
// still completes, and its answer, not the caller's cancellation, is what
// the next callers get.
func TestACancelledCallerDoesNotPoisonTheCache(t *testing.T) {
	server, calls := adminServer(t, http.StatusOK, []atriumconfig.CollectorRuntimeConfig{
		{ID: "oslogs-prod", Kind: KindOutSystemsLogs, Enabled: true, Targets: []atriumconfig.TargetRuntimeConfig{target("db", "prod")}},
	})
	c := &Client{URL: server.URL, Token: token}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.OutSystemsLogs(cancelled); err != nil {
		t.Fatalf("a cancelled caller's request: %v", err)
	}
	got, err := c.OutSystemsLogs(context.Background())
	if err != nil || len(got.ByEnvironment["prod"]) != 1 {
		t.Fatalf("the next caller got %+v, %v: the cancellation was kept", got, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("%d requests, want 1", calls.Load())
	}
}
