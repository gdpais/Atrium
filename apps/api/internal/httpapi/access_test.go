package httpapi

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

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
