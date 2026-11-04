package links

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// grafanaLokiDatasource is the uid of the Loki datasource the chart
// provisions in Grafana (grafana.datasources in the chart's values). A
// Grafana whose Loki datasource has another uid opens Explore on its
// default datasource instead.
const grafanaLokiDatasource = "loki"

// grafana is the adapter for platform grafana: Grafana with Loki.
type grafana struct{}

// timeRange is Unix milliseconds, or Grafana's relative now-1h and now for
// the zero Window.
func (grafana) timeRange(w Window) (string, string) {
	if w.From.IsZero() || w.To.IsZero() {
		return "now-1h", "now"
	}
	return strconv.FormatInt(w.From.UnixMilli(), 10), strconv.FormatInt(w.To.UnixMilli(), 10)
}

// logsQuery is the state of an Explore pane, as Grafana's `left` URL
// parameter takes it: a LogQL stream selector on the labels the OTel
// Collector gives the application's logs in Loki, service_name and
// deployment_environment_name, as the OutSystems dashboards query them.
func (g grafana) logsQuery(application, environment string, w Window) string {
	from, to := g.timeRange(w)
	selector := "{service_name=" + logQLString(application) + ", deployment_environment_name=" + logQLString(environment) + "}"
	state := explorePane{
		Datasource: grafanaLokiDatasource,
		Queries:    []exploreQuery{{RefID: "A", Datasource: exploreDatasource{Type: "loki", UID: grafanaLokiDatasource}, Expr: selector}},
		Range:      exploreRange{From: from, To: to},
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(state); err != nil {
		panic(err) // strings only
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// explorePane is Grafana Explore's pane state.
type explorePane struct {
	Datasource string         `json:"datasource"`
	Queries    []exploreQuery `json:"queries"`
	Range      exploreRange   `json:"range"`
}

type exploreQuery struct {
	RefID      string            `json:"refId"`
	Datasource exploreDatasource `json:"datasource"`
	Expr       string            `json:"expr"`
}

type exploreDatasource struct {
	Type string `json:"type"`
	UID  string `json:"uid"`
}

type exploreRange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// logQLString quotes s as a LogQL string literal. LogQL reads them with Go's
// strconv.Unquote, so a quote or backslash in a name cannot end the
// selector.
func logQLString(s string) string {
	return strconv.Quote(s)
}
