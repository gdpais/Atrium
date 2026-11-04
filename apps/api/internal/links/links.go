// Package links renders links into the observability platform's UI from the
// templates of integrations.yaml (TODO 4.9): dashboards, and deep links to
// an application's or a database target's telemetry. Nothing is stored: the
// templates are the integrations config's, the variables the inventory's,
// so a template added to the file is offered wherever its variables exist.
//
// What cannot be neutral, the log search a ${logs_query} stands for and the
// time range's syntax, comes from the platform's adapter (ADR 0011 §5).
// Grafana, with Loki, is the only one.
package links

import (
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"heartbeat/internal/config"
)

// Link kinds.
const (
	// KindDashboard is a rendered dashboards.dashboard_templates entry.
	KindDashboard = "dashboard"
	// KindDeepLink is a rendered dashboards.deep_link_templates entry.
	KindDeepLink = "deep_link"
)

// Link is one rendered template.
type Link struct {
	// Name is the template's key in integrations.yaml.
	Name string
	// Kind is KindDashboard or KindDeepLink.
	Kind string
	// URL is the template on dashboards.base_url, its variables filled in.
	URL string
	// ApplicationName is the application_name an application's link was
	// rendered for, when its template names ${application} or
	// ${logs_query}; "" otherwise.
	ApplicationName string
}

// Window is a time range. The zero Window is the last hour, relative to
// when a link is opened.
type Window struct {
	From, To time.Time
}

// Scope is what an entity's telemetry is filtered by. "" is a variable the
// entity does not have.
type Scope struct {
	// Environment is the environment's slug: deployment.environment.name.
	Environment string
	// Applications are the application_names of an application's sources,
	// each a service.name (ADR 0017).
	Applications []string
	// Target is a database target's name: the target label.
	Target string
}

// adapter writes what a platform's links need that the config cannot
// write neutrally.
type adapter interface {
	// timeRange is the window's from and to, as the platform's URLs take
	// them.
	timeRange(w Window) (from, to string)
	// logsQuery is the value of ${logs_query}: a log search for one
	// application's telemetry in an environment over w.
	logsQuery(application, environment string, w Window) string
}

// adapters are the platforms Heartbeat has links for: with
// config.PlatformGeneric there are none, and the loader keeps its
// dashboards empty.
var adapters = map[string]adapter{
	config.PlatformGrafana: grafana{},
}

// Dashboards renders every dashboard template of cfg for w, by name.
func Dashboards(cfg config.RuntimeConfig, w Window) []Link {
	a, ok := adapters[cfg.Platform]
	if !ok {
		return []Link{}
	}
	from, to := a.timeRange(w)
	vars := map[string]string{config.VarFrom: from, config.VarTo: to}
	out := []Link{}
	for _, name := range sortedKeys(cfg.Dashboards.DashboardTemplates) {
		if url, ok := render(cfg.Dashboards.BaseURL, cfg.Dashboards.DashboardTemplates[name], vars); ok {
			out = append(out, Link{Name: name, Kind: KindDashboard, URL: url})
		}
	}
	return out
}

// For renders the deep-link templates of cfg that scope has every variable
// of, for w: by name, then application name. A template that names
// ${application} or ${logs_query} is rendered once per application name,
// any other once.
func For(cfg config.RuntimeConfig, scope Scope, w Window) []Link {
	a, ok := adapters[cfg.Platform]
	if !ok {
		return []Link{}
	}
	from, to := a.timeRange(w)
	common := map[string]string{config.VarFrom: from, config.VarTo: to}
	if scope.Environment != "" {
		common[config.VarEnvironment] = scope.Environment
	}
	if scope.Target != "" {
		common[config.VarTarget] = scope.Target
	}
	applications := slices.Clone(scope.Applications)
	sort.Strings(applications)
	applications = slices.Compact(applications)
	applications = slices.DeleteFunc(applications, func(name string) bool { return name == "" })

	out := []Link{}
	for _, name := range sortedKeys(cfg.Dashboards.DeepLinkTemplates) {
		template := cfg.Dashboards.DeepLinkTemplates[name]
		variables, err := config.TemplateVariables(template)
		if err != nil {
			continue // the loader refuses these
		}
		if !perApplication(variables) {
			if url, ok := render(cfg.Dashboards.BaseURL, template, common); ok {
				out = append(out, Link{Name: name, Kind: KindDeepLink, URL: url})
			}
			continue
		}
		for _, application := range applications {
			vars := maps.Clone(common)
			vars[config.VarApplication] = application
			if scope.Environment != "" {
				vars[config.VarLogsQuery] = a.logsQuery(application, scope.Environment, w)
			}
			if url, ok := render(cfg.Dashboards.BaseURL, template, vars); ok {
				out = append(out, Link{Name: name, Kind: KindDeepLink, URL: url, ApplicationName: application})
			}
		}
	}
	return out
}

// perApplication says whether a template is rendered once per application
// name.
func perApplication(variables []string) bool {
	return slices.Contains(variables, config.VarApplication) || slices.Contains(variables, config.VarLogsQuery)
}

// render fills template's variables from vars, each percent-encoded, and
// puts the result on base. It reports false when vars lacks one.
func render(base, template string, vars map[string]string) (string, bool) {
	var b strings.Builder
	b.WriteString(strings.TrimRight(base, "/"))
	rest := template
	for {
		start := strings.Index(rest, "${")
		if start < 0 {
			b.WriteString(rest)
			return b.String(), true
		}
		end := strings.IndexByte(rest[start:], '}')
		if end < 0 {
			return "", false
		}
		value, ok := vars[rest[start+2:start+end]]
		if !ok {
			return "", false
		}
		b.WriteString(rest[:start])
		b.WriteString(escape(value))
		rest = rest[start+end+1:]
	}
}

// escape percent-encodes every byte of s but RFC 3986's unreserved
// characters, so a value is one path segment or one query value wherever
// the template puts it, and can add no parameter, path or fragment.
func escape(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&15])
	}
	return b.String()
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
