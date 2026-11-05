package httpapi

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	api "heartbeat/apps/api"
	"heartbeat/apps/api/internal/auth"
	"heartbeat/apps/api/internal/openapi"
)

// recordingMux records the patterns the generated code registers.
type recordingMux struct {
	*http.ServeMux
	patterns []string
}

func (m *recordingMux) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	m.patterns = append(m.patterns, pattern)
	m.ServeMux.HandleFunc(pattern, handler)
}

// Every pattern the generated code serves has an access rule, keyed as
// the ServeMux reports it in r.Pattern, and every rule is served: the
// guard never meets an operation it has no rule for.
func TestEveryRegisteredPatternHasAnAccessRule(t *testing.T) {
	server := newServer(t, Options{Site: site})
	mux := &recordingMux{ServeMux: http.NewServeMux()}
	openapi.HandlerWithOptions(openapi.NewStrictHandler(server, nil), openapi.StdHTTPServerOptions{BaseRouter: mux})
	if len(mux.patterns) == 0 {
		t.Fatal("no operation registered")
	}
	for _, pattern := range mux.patterns {
		if _, ok := server.rules[pattern]; !ok {
			t.Errorf("%s is served without an access rule", pattern)
		}
	}
	for pattern := range server.rules {
		if !slices.Contains(mux.patterns, pattern) {
			t.Errorf("rule for %s, which is not served: the rules and the routes disagree on the pattern", pattern)
		}
	}

	// Through Register and the guard: an operation that needs a session
	// answers 401 without one, never the guard's 403 for a missing rule.
	for pattern, rule := range server.rules {
		method, path, _ := strings.Cut(pattern, " ")
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", site)
		rec := serveRequest(server, req)
		if !rule.public && rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a session = %d, want 401", pattern, rec.Code)
		}
	}
}

// The spec's rules: what each operation declares.
func TestSpecDeclaresTheSessionOperationsRules(t *testing.T) {
	rules, err := accessRules(api.Spec)
	if err != nil {
		t.Fatal(err)
	}
	for pattern, want := range map[string]rule{
		"GET /healthz":           {public: true},
		"GET /readyz":            {public: true},
		"POST /api/v1/session":   {public: true},
		"GET /api/v1/session":    {role: auth.Viewer},
		"DELETE /api/v1/session": {role: auth.Viewer},
	} {
		if got, ok := rules[pattern]; !ok || got != want {
			t.Errorf("%s: rule %+v (%t), want %+v", pattern, got, ok, want)
		}
	}
}

// Each operation declares the refusals the guard can answer it with: 401
// unless it is public, and 403 when it needs a role (a user may have none)
// or its method is unsafe (the Origin check).
func TestSpecDeclaresTheGuardsRefusals(t *testing.T) {
	rules, err := accessRules(api.Spec)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(api.Spec, &doc); err != nil {
		t.Fatal(err)
	}
	for path, item := range doc.Paths {
		for method, node := range item {
			if !slices.Contains(httpMethods, method) {
				continue
			}
			pattern := strings.ToUpper(method) + " " + path
			var op struct {
				Responses map[string]yaml.Node `yaml:"responses"`
			}
			if err := node.Decode(&op); err != nil {
				t.Fatalf("%s: %v", pattern, err)
			}
			r := rules[pattern]
			if _, ok := op.Responses["401"]; !r.public && !ok {
				t.Errorf("%s declares no 401", pattern)
			}
			if _, ok := op.Responses["403"]; (r.role != "" || unsafe(strings.ToUpper(method))) && !ok {
				t.Errorf("%s declares no 403", pattern)
			}
		}
	}
}

// Each write the inventory file's rows refuse (ADR 0021 §4) declares 409:
// the service answers a ConflictError for a config-managed row, and a
// handler can answer only the statuses its operation declares, so a
// missing 409 turns the refusal into a server error.
func TestSpecDeclaresTheInventoryFilesRefusals(t *testing.T) {
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(api.Spec, &doc); err != nil {
		t.Fatal(err)
	}
	var refused []string
	for _, collection := range []string{"environments", "applications", "components", "telemetry-sources", "database-targets", "probe-assignments"} {
		refused = append(refused, "PATCH /api/v1/"+collection+"/{id}", "DELETE /api/v1/"+collection+"/{id}")
	}
	refused = append(refused, "POST /api/v1/probe-assignments", "POST /api/v1/probe-assignments/{id}/approve", "POST /api/v1/probe-assignments/{id}/reject")
	for _, pattern := range refused {
		method, path, _ := strings.Cut(pattern, " ")
		node, ok := doc.Paths[path][strings.ToLower(method)]
		if !ok {
			t.Errorf("%s is not in the spec", pattern)
			continue
		}
		var op struct {
			Responses map[string]yaml.Node `yaml:"responses"`
		}
		if err := node.Decode(&op); err != nil {
			t.Fatalf("%s: %v", pattern, err)
		}
		if _, ok := op.Responses["409"]; !ok {
			t.Errorf("%s declares no 409, so a config-managed row's refusal is a 500", pattern)
		}
	}
}

func TestAccessRulesNeedEachOperationToDeclareOne(t *testing.T) {
	spec := func(security string) []byte {
		return []byte(`
openapi: 3.1.0
security:
  - session: [admin]
paths:
  /api/v1/things:
    parameters: []
    get:
      operationId: listThings
` + security + `
      responses: {}
`)
	}
	for name, tc := range map[string]struct {
		security string
		want     rule
		err      string
	}{
		"public":             {"      security: []", rule{public: true}, ""},
		"any logged-in user": {"      security:\n        - session: []", rule{}, ""},
		"least role":         {"      security:\n        - session: [operator]", rule{role: auth.Operator}, ""},
		"nothing declared":   {"", rule{}, "declares no security"},
		"two roles":          {"      security:\n        - session: [viewer, admin]", rule{}, "want the least one"},
		"unknown role":       {"      security:\n        - session: [owner]", rule{}, `role "owner"`},
		"other scheme":       {"      security:\n        - token: []", rule{}, "want the session scheme alone"},
		"two schemes":        {"      security:\n        - session: []\n          token: []", rule{}, "want the session scheme alone"},
		"two requirements":   {"      security:\n        - session: []\n        - token: []", rule{}, "want one"},
	} {
		t.Run(name, func(t *testing.T) {
			rules, err := accessRules(spec(tc.security))
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) || !strings.Contains(err.Error(), "GET /api/v1/things (listThings)") {
					t.Fatalf("accessRules = %v, want an error naming the operation and %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := rules["GET /api/v1/things"]; got != tc.want || len(rules) != 1 {
				t.Fatalf("rules %+v, want only GET /api/v1/things: %+v", rules, tc.want)
			}
		})
	}
}
