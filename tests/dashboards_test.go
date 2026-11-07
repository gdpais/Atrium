package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const dashboardDir = "infra/helm/heartbeat/files/grafana/dashboards"

type dashboardTarget struct {
	Datasource struct {
		Type string `json:"type"`
		UID  string `json:"uid"`
	} `json:"datasource"`
	Expr  string `json:"expr"`
	RefID string `json:"refId"`
}

type dashboardPanel struct {
	Type       string `json:"type"`
	Title      string `json:"title"`
	Datasource *struct {
		Type string `json:"type"`
		UID  string `json:"uid"`
	} `json:"datasource"`
	Targets     []dashboardTarget `json:"targets"`
	FieldConfig struct {
		Defaults struct {
			Unit   string   `json:"unit"`
			Max    *float64 `json:"max"`
			Custom struct {
				AxisSoftMax *float64 `json:"axisSoftMax"`
			} `json:"custom"`
		} `json:"defaults"`
	} `json:"fieldConfig"`
}

type dashboard struct {
	UID        string           `json:"uid"`
	Title      string           `json:"title"`
	Tags       []string         `json:"tags"`
	Panels     []dashboardPanel `json:"panels"`
	Templating struct {
		List []struct {
			Name string `json:"name"`
		} `json:"list"`
	} `json:"templating"`
}

// grafanaDatasources is the uid → type of every datasource the chart
// provisions.
func grafanaDatasources(t *testing.T) map[string]string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(repoRoot(t), "infra/helm/heartbeat/values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		Grafana struct {
			Datasources map[string]struct {
				Datasources []struct {
					UID  string `yaml:"uid"`
					Type string `yaml:"type"`
				} `yaml:"datasources"`
			} `yaml:"datasources"`
		} `yaml:"grafana"`
	}
	if err := yaml.Unmarshal(content, &values); err != nil {
		t.Fatal(err)
	}
	uids := map[string]string{}
	for _, file := range values.Grafana.Datasources {
		for _, ds := range file.Datasources {
			uids[ds.UID] = ds.Type
		}
	}
	if len(uids) == 0 {
		t.Fatal("found no Grafana datasources in values.yaml; update grafanaDatasources if they moved")
	}
	return uids
}

// Grafana's own variables, which dashboards use without defining them.
var grafanaBuiltins = map[string]bool{
	"__rate_interval": true, "__range": true, "__interval": true, "__url_time_range": true,
	"__value": true, "__data": true, "__from": true, "__to": true, "__all": true,
}

var dashboardVariableRef = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)`)
var dashboardLinkRef = regexp.MustCompile(`/d/([a-z0-9-]+)/`)

// The dashboards are hand-checked JSON: an unknown datasource, an undefined
// variable or a link to a dashboard that does not exist renders as an empty
// panel or a dead link, not as an error.
func TestDashboardsAreConsistent(t *testing.T) {
	root := repoRoot(t)
	datasources := grafanaDatasources(t)
	files, err := filepath.Glob(filepath.Join(root, dashboardDir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no dashboards in %s: %v", dashboardDir, err)
	}
	raw := map[string]string{}
	boards := map[string]dashboard{}
	for _, file := range files {
		content, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var d dashboard
		if err := json.Unmarshal(content, &d); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		name := filepath.Base(file)
		if d.UID == "" || d.Title == "" {
			t.Errorf("%s has no uid or title", name)
		}
		if other, ok := boards[d.UID]; ok {
			t.Errorf("%s and %s share uid %s", name, other.Title, d.UID)
		}
		if strings.TrimSuffix(name, ".json") != d.UID {
			t.Errorf("%s has uid %s; name the file after its uid", name, d.UID)
		}
		boards[d.UID] = d
		raw[d.UID] = string(content)
	}

	for uid, d := range boards {
		defined := map[string]bool{}
		for _, v := range d.Templating.List {
			defined[v.Name] = true
		}
		for _, p := range d.Panels {
			if p.Type == "row" || p.Type == "text" {
				continue
			}
			if p.Datasource == nil {
				t.Errorf("%s: panel %q has no datasource", uid, p.Title)
				continue
			}
			if typ, ok := datasources[p.Datasource.UID]; !ok || typ != p.Datasource.Type {
				t.Errorf("%s: panel %q uses datasource %s/%s, which the chart does not provision",
					uid, p.Title, p.Datasource.Type, p.Datasource.UID)
			}
			if len(p.Targets) == 0 {
				t.Errorf("%s: panel %q has no queries", uid, p.Title)
			}
			// With every value 0, Grafana scales the axis to 0–100, which a
			// 0–1 ratio shows as 0–10000%.
			if defaults := p.FieldConfig.Defaults; p.Type == "timeseries" && defaults.Unit == "percentunit" &&
				defaults.Max == nil && defaults.Custom.AxisSoftMax == nil {
				t.Errorf("%s: panel %q is a ratio with no max or axisSoftMax", uid, p.Title)
			}
			for _, target := range p.Targets {
				if target.Datasource.UID != "" && target.Datasource.UID != p.Datasource.UID {
					t.Errorf("%s: panel %q query %s uses %s, the panel %s",
						uid, p.Title, target.RefID, target.Datasource.UID, p.Datasource.UID)
				}
				if strings.TrimSpace(target.Expr) == "" {
					t.Errorf("%s: panel %q query %s is empty", uid, p.Title, target.RefID)
				}
			}
		}
		for _, m := range dashboardVariableRef.FindAllStringSubmatch(raw[uid], -1) {
			if !defined[m[1]] && !grafanaBuiltins[m[1]] {
				t.Errorf("%s uses $%s, which it does not define", uid, m[1])
			}
		}
		for _, m := range dashboardLinkRef.FindAllStringSubmatch(raw[uid], -1) {
			if _, ok := boards[m[1]]; !ok {
				t.Errorf("%s links to dashboard %s, which does not exist", uid, m[1])
			}
		}
	}
}

// The inventory dashboard's data links are the API's deep links [4.9]:
// each deep_link_template of the chart's integrations, with Grafana's
// field values for the API's variables. A template changed, added or
// removed without its link fails here.
func TestInventoryDashboardLinksAreTheLinkTemplates(t *testing.T) {
	root := repoRoot(t)
	content, err := os.ReadFile(filepath.Join(root, "infra/helm/heartbeat/values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		Integrations struct {
			Dashboards struct {
				DashboardTemplates map[string]string `yaml:"dashboard_templates"`
				DeepLinkTemplates  map[string]string `yaml:"deep_link_templates"`
			} `yaml:"dashboards"`
		} `yaml:"integrations"`
	}
	if err := yaml.Unmarshal(content, &values); err != nil {
		t.Fatal(err)
	}
	templates := values.Integrations.Dashboards.DeepLinkTemplates
	if len(templates) == 0 {
		t.Fatal("no integrations.dashboards.deep_link_templates in values.yaml")
	}
	if got := values.Integrations.Dashboards.DashboardTemplates["heartbeat-inventory"]; got != "/d/heartbeat-inventory" {
		t.Errorf("dashboard_templates lists heartbeat-inventory as %q, want /d/heartbeat-inventory", got)
	}

	raw, err := os.ReadFile(filepath.Join(root, dashboardDir, "heartbeat-inventory.json"))
	if err != nil {
		t.Fatal(err)
	}
	var board struct {
		Panels []struct {
			FieldConfig struct {
				Overrides []struct {
					Matcher struct {
						ID      string `json:"id"`
						Options string `json:"options"`
					} `json:"matcher"`
					Properties []struct {
						ID    string          `json:"id"`
						Value json.RawMessage `json:"value"`
					} `json:"properties"`
				} `json:"overrides"`
			} `json:"fieldConfig"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(raw, &board); err != nil {
		t.Fatal(err)
	}
	// The links on each column, by the column's (label's) name.
	links := map[string][]string{}
	for _, p := range board.Panels {
		for _, o := range p.FieldConfig.Overrides {
			for _, prop := range o.Properties {
				if prop.ID != "links" {
					continue
				}
				var ls []struct {
					URL string `json:"url"`
				}
				if err := json.Unmarshal(prop.Value, &ls); err != nil {
					t.Fatal(err)
				}
				for _, l := range ls {
					links[o.Matcher.Options] = append(links[o.Matcher.Options], l.URL)
				}
			}
		}
	}

	// The API's variables as Grafana fills them in a row of the table: the
	// clicked value is the application_name or the target, the row's
	// environment, and the dashboard's time range.
	grafana := strings.NewReplacer(
		"${environment}", "${__data.fields.environment}",
		"${application}", "${__value.raw}",
		"${target}", "${__value.raw}",
		"from=${from}&to=${to}", "${__url_time_range}",
	)
	for name, template := range templates {
		column := "target"
		if strings.Contains(template, "${application}") || strings.Contains(template, "${logs_query}") {
			column = "application_name"
		}
		prefix, isLogs := strings.CutSuffix(template, "${logs_query}")
		found := false
		for _, url := range links[column] {
			if !isLogs {
				found = found || url == grafana.Replace(template)
				continue
			}
			// ${logs_query} is the Explore pane the API's Grafana adapter
			// writes: the application's Loki stream selector.
			rest, ok := strings.CutPrefix(url, prefix)
			if !ok {
				continue
			}
			var pane struct {
				Datasource string `json:"datasource"`
				Queries    []struct {
					Datasource struct{ Type, UID string } `json:"datasource"`
					Expr       string                     `json:"expr"`
				} `json:"queries"`
				Range struct{ From, To string } `json:"range"`
			}
			if err := json.Unmarshal([]byte(rest), &pane); err != nil {
				t.Errorf("the %s link's pane is not JSON: %v", name, err)
				continue
			}
			found = found || pane.Datasource == "loki" && len(pane.Queries) == 1 &&
				pane.Queries[0].Datasource.UID == "loki" && pane.Queries[0].Datasource.Type == "loki" &&
				pane.Queries[0].Expr == `{service_name="${__value.raw}", deployment_environment_name="${__data.fields.environment}"}` &&
				pane.Range.From == "${__from}" && pane.Range.To == "${__to}"
		}
		if !found {
			t.Errorf("deep link template %s (%s) has no link on the inventory dashboard's %s column; its links: %q",
				name, template, column, links[column])
		}
	}
}
