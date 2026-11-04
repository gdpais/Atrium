package config

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Link template variables: what dashboards.dashboard_templates and
// dashboards.deep_link_templates may name as ${name}. The API fills them in
// when it renders a link (TODO 4.9); a template whose variables an entity
// does not have is not rendered for it.
const (
	// VarEnvironment is the environment's slug, the
	// deployment.environment.name of its telemetry and the environment label
	// of its metrics.
	VarEnvironment = "environment"
	// VarApplication is a telemetry source's application_name, the
	// service.name of its logs and the application label of its derived
	// metrics (ADR 0017).
	VarApplication = "application"
	// VarTarget is a database target's name, the target label of its
	// metrics.
	VarTarget = "target"
	// VarLogsQuery is the platform's log search for an application, written
	// by the API's platform adapter: with platform grafana, the Explore
	// state of a Loki query.
	VarLogsQuery = "logs_query"
	// VarFrom and VarTo are the time range, in the platform's syntax.
	VarFrom = "from"
	VarTo   = "to"
)

// LinkVariables are the variables a deep-link template may name.
var LinkVariables = []string{VarApplication, VarEnvironment, VarFrom, VarLogsQuery, VarTarget, VarTo}

// DashboardVariables are the variables a dashboard template may name: only
// the time range, since a dashboard is listed for no entity.
var DashboardVariables = []string{VarFrom, VarTo}

// TemplateVariables returns the distinct variables template names, in the
// order they first appear. It fails on a "${" with no closing "}" or an
// empty or malformed name.
func TemplateVariables(template string) ([]string, error) {
	var names []string
	rest := template
	for {
		start := strings.Index(rest, "${")
		if start < 0 {
			return names, nil
		}
		end := strings.IndexByte(rest[start:], '}')
		if end < 0 {
			return nil, fmt.Errorf("has a ${ with no closing }")
		}
		name := rest[start+2 : start+end]
		if !validVariableName(name) {
			return nil, fmt.Errorf("has a malformed variable ${%s}", name)
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
		rest = rest[start+end+1:]
	}
}

func validVariableName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && r != '_' && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// ValidateLinkTemplates checks the dashboard and deep-link templates of
// dashboards: each is a path on base_url (starting with a single "/"), names
// only the variables its kind takes, and needs an http or https base_url.
//
// LoadRuntimeConfig does not run it: the collector reads no template, and a
// typo in a link must not stop it collecting. The API runs it on every file
// it renders links from, so a typo fails there, naming the template, and the
// last valid file's links are kept.
func ValidateLinkTemplates(dashboards Endpoint) error {
	for _, set := range []struct {
		field     string
		templates map[string]string
		allowed   []string
	}{
		{"dashboard_templates", dashboards.DashboardTemplates, DashboardVariables},
		{"deep_link_templates", dashboards.DeepLinkTemplates, LinkVariables},
	} {
		keys := make([]string, 0, len(set.templates))
		for key := range set.templates {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			name := "dashboards." + set.field + "." + key
			template := set.templates[key]
			if strings.TrimSpace(key) == "" {
				return fmt.Errorf("dashboards.%s has an empty name", set.field)
			}
			if dashboards.BaseURL == "" {
				return fmt.Errorf("%s needs dashboards.base_url: templates are paths on it", name)
			}
			if scheme, _, _ := strings.Cut(dashboards.BaseURL, ":"); !strings.EqualFold(scheme, "http") && !strings.EqualFold(scheme, "https") {
				return fmt.Errorf("dashboards.base_url must be an http or https URL: links open in a browser")
			}
			if !strings.HasPrefix(template, "/") || strings.HasPrefix(template, "//") {
				return fmt.Errorf("%s must be a path on dashboards.base_url, starting with a single /", name)
			}
			variables, err := TemplateVariables(template)
			if err != nil {
				return fmt.Errorf("%s %w", name, err)
			}
			for _, variable := range variables {
				if !slices.Contains(set.allowed, variable) {
					return fmt.Errorf("%s names ${%s}, want only %s", name, variable, variableList(set.allowed))
				}
			}
		}
	}
	return nil
}

func variableList(names []string) string {
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = "${" + name + "}"
	}
	return strings.Join(out, ", ")
}
