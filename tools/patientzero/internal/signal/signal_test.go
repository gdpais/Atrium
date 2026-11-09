package signal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"atrium/tools/patientzero/internal/scenario"
)

var at = time.Date(2026, 10, 22, 10, 0, 0, 500_000_000, time.UTC)

// fake serves one canned body on path and records the requests it got.
type fake struct {
	t      *testing.T
	path   string
	status int
	body   string
	got    []url.Values
}

func (f *fake) server() *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != f.path {
			f.t.Errorf("request %s %s, want GET %s", r.Method, r.URL.Path, f.path)
			http.NotFound(w, r)
			return
		}
		f.got = append(f.got, r.URL.Query())
		if f.status != 0 {
			w.WriteHeader(f.status)
		}
		_, _ = w.Write([]byte(f.body))
	}))
	f.t.Cleanup(srv.Close)
	return srv
}

func promSignal(cond string) scenario.Signal {
	return scenario.Signal{Name: "s", PromQL: `sum(rate(x[1m]))`, Condition: cond}
}

func TestPromQLVector(t *testing.T) {
	f := &fake{t: t, path: "/api/v1/query", body: `{"status":"success","data":{"resultType":"vector","result":[
		{"metric":{"a":"1"},"value":[1761127200.5,"0.52"]},
		{"metric":{"a":"2"},"value":[1761127200.5,"0.31"]}]}}`}
	src := Sources{Prometheus: f.server().URL + "/"}
	res, err := src.Evaluate(context.Background(), promSignal("> 0.3"), at)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Pass || res.Series != 2 || !slices.Equal(res.Values, []float64{0.31, 0.52}) {
		t.Fatalf("result %+v, want pass with both series' values", res)
	}
	q := f.got[0]
	if q.Get("query") != `sum(rate(x[1m]))` || q.Get("time") != "1792663200.500" {
		t.Errorf("query params %v", q)
	}
	// Every series must hold.
	res, err = src.Evaluate(context.Background(), promSignal("> 0.4"), at)
	if err != nil || res.Pass {
		t.Fatalf("> 0.4: %+v, %v; want fail (one series is 0.31)", res, err)
	}
}

func TestPromQLScalarAndEmpty(t *testing.T) {
	f := &fake{t: t, path: "/api/v1/query", body: `{"status":"success","data":{"resultType":"scalar","result":[1761127200,"3"]}}`}
	src := Sources{Prometheus: f.server().URL}
	if res, err := src.Evaluate(context.Background(), promSignal("== 3"), at); err != nil || !res.Pass {
		t.Fatalf("scalar: %+v, %v", res, err)
	}
	f.body = `{"status":"success","data":{"resultType":"vector","result":[]}}`
	if res, err := src.Evaluate(context.Background(), promSignal("< 1"), at); err != nil || res.Pass || res.Series != 0 {
		t.Fatalf("no data, < 1: %+v, %v; want fail", res, err)
	}
	if res, err := src.Evaluate(context.Background(), promSignal("absent"), at); err != nil || !res.Pass {
		t.Fatalf("no data, absent: %+v, %v; want pass", res, err)
	}
}

func TestPromQLErrors(t *testing.T) {
	f := &fake{t: t, path: "/api/v1/query", status: http.StatusBadRequest,
		body: `{"status":"error","errorType":"bad_data","error":"parse error"}`}
	src := Sources{Prometheus: f.server().URL}
	if _, err := src.Evaluate(context.Background(), promSignal("> 0"), at); err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("err = %v, want the HTTP status", err)
	}
	f.status, f.body = 0, `{"status":"success","data":{"resultType":"matrix","result":[]}}`
	if _, err := src.Evaluate(context.Background(), promSignal("> 0"), at); err == nil || !strings.Contains(err.Error(), "range vector") {
		t.Fatalf("err = %v, want the range vector refused", err)
	}
	if _, err := (Sources{}).Evaluate(context.Background(), promSignal("> 0"), at); err == nil || !strings.Contains(err.Error(), "no Prometheus URL") {
		t.Fatalf("err = %v, want no URL", err)
	}
}

func TestLogQLMetricQueries(t *testing.T) {
	f := &fake{t: t, path: "/loki/api/v1/query", body: `{"status":"success","data":{"resultType":"vector","result":[
		{"metric":{},"value":[1761127200,"12"]}]}}`}
	src := Sources{Loki: f.server().URL}
	sig := scenario.Signal{Name: "l", LogQL: `sum(count_over_time({service_name="PatientZero"}[2m]))`, Condition: "> 0"}
	res, err := src.Evaluate(context.Background(), sig, at)
	if err != nil || !res.Pass || res.Values[0] != 12 {
		t.Fatalf("metric query: %+v, %v", res, err)
	}
	q := f.got[0]
	if q.Get("time") != "1792663200500000000" || q.Get("query") != sig.LogQL {
		t.Errorf("Loki params %v; want nanosecond time", q)
	}
	// A log query has no value to compare; the signal names the fix.
	f.body = `{"status":"success","data":{"resultType":"streams","result":[{"stream":{},"values":[["1","x"]]}]}}`
	sig.LogQL = `{service_name="PatientZero"} |= "boom"`
	if _, err := src.Evaluate(context.Background(), sig, at); err == nil || !strings.Contains(err.Error(), "count_over_time") {
		t.Fatalf("log query: err %v, want a metric query asked for", err)
	}
}

func TestAlerts(t *testing.T) {
	f := &fake{t: t, path: "/api/v2/alerts", body: `[{"labels":{"alertname":"OutSystemsErrorBudgetBurnFast"},"status":{"state":"active"}}]`}
	src := Sources{Alertmanager: f.server().URL}
	sig := scenario.Signal{Name: "a", Alert: "OutSystemsErrorBudgetBurnFast", State: scenario.StateFiring,
		Labels: map[string]string{"severity": "critical", "application": `Patient"Zero`}}
	res, err := src.Evaluate(context.Background(), sig, at)
	if err != nil || !res.Pass || res.Values[0] != 1 {
		t.Fatalf("firing: %+v, %v", res, err)
	}
	q := f.got[0]
	wantFilters := []string{`alertname="OutSystemsErrorBudgetBurnFast"`, `application="Patient\"Zero"`, `severity="critical"`}
	if !slices.Equal(q["filter"], wantFilters) || q.Get("active") != "true" || q.Get("silenced") != "false" || q.Get("inhibited") != "false" {
		t.Errorf("Alertmanager params %v; want filters %v and only active, unsilenced alerts", q, wantFilters)
	}
	sig.State = scenario.StateResolved
	if res, err := src.Evaluate(context.Background(), sig, at); err != nil || res.Pass {
		t.Fatalf("resolved while active: %+v, %v; want fail", res, err)
	}
	f.body = `[]`
	if res, err := src.Evaluate(context.Background(), sig, at); err != nil || !res.Pass {
		t.Fatalf("resolved with no alert: %+v, %v; want pass", res, err)
	}
}

func TestResponseSizeIsCapped(t *testing.T) {
	f := &fake{t: t, path: "/api/v1/query", body: strings.Repeat(" ", maxBody+10)}
	src := Sources{Prometheus: f.server().URL}
	if _, err := src.Evaluate(context.Background(), promSignal("> 0"), at); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("err = %v, want the size cap", err)
	}
}

// fakeLedger answers every log type with the same counts, and records the
// windows it was asked for.
type fakeLedger struct {
	rows, held int64
	asked      []string
}

func (l *fakeLedger) Count(_ context.Context, logType string, from, to time.Time, heldOver time.Duration) (int64, int64, error) {
	l.asked = append(l.asked, logType+" "+from.Format("15:04:05.000")+" "+to.Format("15:04:05.000")+" "+heldOver.String())
	if heldOver == 0 {
		return l.rows, 0, nil
	}
	return l.rows, l.held, nil
}

func TestDeliveryComparesLokiWithTheLedger(t *testing.T) {
	var queries []url.Values
	lines := map[string]string{"screen": "120", "general": "95"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		queries = append(queries, q)
		for typ, n := range lines {
			if strings.Contains(q.Get("query"), `outsystems_log_type="`+typ+`"`) {
				_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"` + n + `"]}]}}`))
				return
			}
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer srv.Close()
	ledger := &fakeLedger{rows: 120, held: 25}
	src := Sources{Loki: srv.URL, Ledger: ledger, Application: "PatientZero"}
	from := time.Date(2026, 10, 23, 10, 0, 0, 700_000_000, time.UTC)
	to := from.Add(5*time.Minute - 700*time.Millisecond)
	w := Window{From: from, To: to}

	sig := scenario.Signal{Name: "d", Delivery: &scenario.Delivery{LogTypes: []string{"screen", "general", "error"}}}
	res, err := src.EvaluateDelivery(context.Background(), sig, w)
	if err != nil {
		t.Fatal(err)
	}
	// screen matches; general has 95 of 120; error has none of 120.
	if res.Pass || !slices.Equal(res.Values, []float64{120, 95, 0}) || len(res.Delivery) != 3 || res.Delivery[1].Expected != 120 {
		t.Fatalf("result %+v, want a failure with Loki's counts in log type order", res)
	}
	q := queries[0]
	if got, want := q.Get("query"), `sum(count_over_time({service_name="PatientZero", outsystems_log_type="screen"} [300s]))`; got != want {
		t.Errorf("query %s, want %s", got, want)
	}
	if got := q.Get("time"); got != "1792749900000000000" {
		t.Errorf("time %s, want the window's end rounded up to the second, in nanoseconds", got)
	}
	if ledger.asked[0] != "screen 10:00:00.000 10:05:00.000 0s" {
		t.Errorf("ledger asked for %q, want the window widened to whole seconds", ledger.asked[0])
	}

	// With lost_after, the rows held back longer are left out: general's 95
	// of 120 are exactly what Loki should hold.
	sig.Delivery = &scenario.Delivery{LogTypes: []string{"general"}, LostAfter: scenario.Duration(30 * time.Second)}
	res, err = src.EvaluateDelivery(context.Background(), sig, w)
	if err != nil || !res.Pass || res.Delivery[0].Lost != 25 || res.Delivery[0].Expected != 95 {
		t.Fatalf("lost_after: %+v, %v; want a pass, 25 lost of 120", res, err)
	}
	if !strings.HasSuffix(ledger.asked[len(ledger.asked)-1], " 30s") {
		t.Errorf("ledger asked %q, want the held rows over 30s", ledger.asked[len(ledger.asked)-1])
	}

	// No rows written: nothing to check, so no pass.
	empty := Sources{Loki: srv.URL, Ledger: &fakeLedger{}, Application: "PatientZero"}
	if res, err := empty.EvaluateDelivery(context.Background(), scenario.Signal{Name: "d", Delivery: &scenario.Delivery{LogTypes: []string{"error"}}}, w); res.Pass || err == nil {
		t.Errorf("a window without rows: %+v, %v; want a failure", res, err)
	}
	if _, err := (Sources{Loki: srv.URL}).EvaluateDelivery(context.Background(), sig, w); err == nil {
		t.Error("a delivery check without a ledger must fail")
	}
	if _, err := src.EvaluateDelivery(context.Background(), sig, Window{From: from, To: from}); err == nil {
		t.Error("an empty window must fail")
	}
	// A window within one second is widened to it.
	if _, err := src.EvaluateDelivery(context.Background(), sig, Window{From: from, To: from.Add(200 * time.Millisecond)}); err != nil ||
		!strings.Contains(queries[len(queries)-1].Get("query"), "[1s]") {
		t.Errorf("a short window: %v, query %s; want it widened to 1s", err, queries[len(queries)-1].Get("query"))
	}
	if _, err := src.Evaluate(context.Background(), sig, at); err == nil {
		t.Error("Evaluate must refuse a delivery check")
	}
}

// With late_rows, the counter of each log type must have grown by exactly the
// rows lost, from the window's start to the evaluation.
func TestDeliveryChecksTheLateRowsCounter(t *testing.T) {
	loki := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"95"]}]}}`))
	}))
	defer loki.Close()
	from := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	evaluated := from.Add(9 * time.Minute)
	// The counter is 7 at the window's start, and 7 + late at evaluated.
	late := 25.0
	var asked []string
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		asked = append(asked, q.Get("query")+" @"+q.Get("time"))
		value := 7.0
		if q.Get("time") == strconv.FormatInt(evaluated.Unix(), 10)+".000" {
			value += late
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"` + strconv.FormatFloat(value, 'f', -1, 64) + `"]}]}}`))
	}))
	defer prom.Close()
	src := Sources{Loki: loki.URL, Prometheus: prom.URL, Ledger: &fakeLedger{rows: 120, held: 25}, Application: "PatientZero"}
	sig := scenario.Signal{Name: "d", Delivery: &scenario.Delivery{
		LogTypes: []string{"general"}, LostAfter: scenario.Duration(30 * time.Second),
		LateRows: `atrium_outsystems_late_rows_total{collector="patientzero"}`,
	}}
	w := Window{From: from, To: from.Add(5 * time.Minute), At: evaluated}
	res, err := src.EvaluateDelivery(context.Background(), sig, w)
	if err != nil || !res.Pass || res.Delivery[0].Late == nil || *res.Delivery[0].Late != 25 {
		t.Fatalf("%+v, %v; want a pass with 25 late rows counted", res, err)
	}
	want := `sum(atrium_outsystems_late_rows_total{collector="patientzero", log_type="general"})`
	if len(asked) != 2 || asked[0] != want+" @"+strconv.FormatInt(from.Unix(), 10)+".000" {
		t.Fatalf("asked Prometheus %q, want %s at the window's start and at the evaluation", asked, want)
	}
	late = 24 // one lost row the collector did not count
	if res, err := src.EvaluateDelivery(context.Background(), sig, w); err != nil || res.Pass {
		t.Fatalf("%+v, %v; want a failure", res, err)
	}
	if _, err := (Sources{Loki: loki.URL, Ledger: &fakeLedger{rows: 1}, Application: "P"}).EvaluateDelivery(context.Background(), sig, w); err == nil {
		t.Fatal("late_rows without a Prometheus URL must fail")
	}
}

func TestWithLabel(t *testing.T) {
	for in, want := range map[string]string{
		"m":               `m{log_type="error"}`,
		"m{}":             `m{log_type="error"}`,
		`m{c="p"}`:        `m{c="p", log_type="error"}`,
		`m{c="p", d!=""}`: `m{c="p", d!="", log_type="error"}`,
	} {
		if got := withLabel(in, "log_type", "error"); got != want {
			t.Errorf("withLabel(%s) = %s, want %s", in, got, want)
		}
	}
}
