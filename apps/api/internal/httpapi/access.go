package httpapi

import (
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"atrium/apps/api/internal/auth"
)

// securityScheme is the spec's one security scheme, the session cookie.
const securityScheme = "session"

// rule is who may call one operation.
type rule struct {
	// public is an operation that needs no session: `security: []`.
	public bool
	// role is the least role it needs; "" is any logged-in user, as
	// `security: [{session: []}]` declares.
	role auth.Role
}

// httpMethods are the keys of a path item that are operations.
var httpMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

// accessRules reads each operation's rule from the spec's security
// requirements, keyed by the ServeMux pattern the operation is served
// under, such as "GET /api/v1/session". Every operation must declare its
// own: `[]` for a public one, or the session scheme with no role or one
// role. One that declares nothing would inherit the top-level requirement;
// accessRules fails on it instead, so it is caught in review and at
// startup, never served on a default.
func accessRules(spec []byte) (map[string]rule, error) {
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(spec, &doc); err != nil {
		return nil, fmt.Errorf("openapi spec: %w", err)
	}
	rules := map[string]rule{}
	for path, item := range doc.Paths {
		for method, node := range item {
			if !slices.Contains(httpMethods, method) {
				continue
			}
			var op struct {
				OperationID string                 `yaml:"operationId"`
				Security    *[]map[string][]string `yaml:"security"`
			}
			if err := node.Decode(&op); err != nil {
				return nil, fmt.Errorf("%s %s: %w", strings.ToUpper(method), path, err)
			}
			r, err := ruleOf(op.Security)
			if err != nil {
				return nil, fmt.Errorf("%s %s (%s): %w", strings.ToUpper(method), path, op.OperationID, err)
			}
			rules[strings.ToUpper(method)+" "+path] = r
		}
	}
	return rules, nil
}

func ruleOf(security *[]map[string][]string) (rule, error) {
	switch {
	case security == nil:
		return rule{}, fmt.Errorf("declares no security: add `security: []` for an operation anyone may call, or `security: [{%s: [ROLE]}]` with the least role it needs", securityScheme)
	case len(*security) == 0:
		return rule{public: true}, nil
	case len(*security) > 1:
		return rule{}, fmt.Errorf("declares %d security requirements, want one", len(*security))
	}
	requirement := (*security)[0]
	roles, ok := requirement[securityScheme]
	if !ok || len(requirement) != 1 {
		return rule{}, fmt.Errorf("security %v: want the %s scheme alone", requirement, securityScheme)
	}
	switch {
	case len(roles) == 0:
		return rule{}, nil
	case len(roles) > 1:
		return rule{}, fmt.Errorf("security names roles %v: want the least one", roles)
	case !slices.Contains(auth.Roles, auth.Role(roles[0])):
		return rule{}, fmt.Errorf("security names role %q: want one of %v", roles[0], auth.Roles)
	}
	return rule{role: auth.Role(roles[0])}, nil
}
