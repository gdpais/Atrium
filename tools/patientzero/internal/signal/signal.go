// Package signal evaluates a scenario's expected signals: PromQL against
// Prometheus, LogQL against Loki, alerts against Alertmanager, and delivery
// checks against Loki and the Ledger of the rows PatientZero committed, at
// the base URLs it is given. One evaluation is one query (one per log type
// for a delivery check); the runner repeats it over the signal's window.
package signal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"atrium/tools/patientzero/internal/scenario"
)

// maxBody caps a response read from a source.
const maxBody = 4 << 20

// Sources are the base URLs signals are read from; an empty URL means that
// source is not available.
type Sources struct {
	Prometheus   string
	Loki         string
	Alertmanager string
	// Client makes the requests; one with a 10s timeout when nil.
	Client *http.Client
	// Application is the service_name a delivery check counts rows of in
	// Loki, and Ledger what PatientZero committed; a delivery check needs
	// both.
	Application string
	Ledger      Ledger
}

// Ledger is what PatientZero committed: the rows of one log type with an
// Instant in (from, to], and how many of them a commit-delay fault held
// back longer than heldOver.
type Ledger interface {
	Count(ctx context.Context, logType string, from, to time.Time, heldOver time.Duration) (rows, held int64, err error)
}

// Window is a delivery check's rows: those with an Instant in (From, To],
// widened to whole seconds.  At is when the check is evaluated, up to which
// its late_rows counter is read; zero means To.
type Window struct {
	From time.Time
	To   time.Time
	At   time.Time
}

// DeliveryCount is one log type's result of a delivery check.
type DeliveryCount struct {
	LogType string `json:"log_type"`
	// Expected is the rows Loki must hold: those written, less those held
	// back longer than the check's lost_after.
	Expected int64 `json:"expected"`
	Loki     int64 `json:"loki"`
	Written  int64 `json:"written"`
	Lost     int64 `json:"lost"`
	// Late is how much the check's late_rows counter grew, which must be
	// Lost; nil without late_rows.
	Late *int64 `json:"late,omitempty"`
}

// URLFor is the base URL of the source a signal kind reads.
func (s Sources) URLFor(kind string) string {
	switch kind {
	case scenario.KindPromQL:
		return s.Prometheus
	case scenario.KindLogQL, scenario.KindDelivery:
		return s.Loki
	case scenario.KindAlert:
		return s.Alertmanager
	}
	return ""
}

// Result is one evaluation of a signal.
type Result struct {
	// Pass is whether the condition (or the alert state) held.
	Pass bool `json:"pass"`
	// Values are the query's values: one per series of a PromQL or LogQL
	// metric query, the number of matching active alerts for an alert.
	// Sorted, at most maxValues.
	Values []float64 `json:"values"`
	// Series is how many values the query returned before the cut.
	Series int `json:"series"`
	// Delivery is a delivery check's count per log type.
	Delivery []DeliveryCount `json:"delivery,omitempty"`
}

// EvaluateDelivery runs a delivery check once: for each of its log types,
// the lines Loki holds of the Application with a timestamp in the window
// (one instant count_over_time query at w.To), against what the Ledger
// counted. It passes when they are equal for every log type, and at least
// one row was written: a missing row or a row stored twice changes the
// count, and a window with no rows checks nothing. The window is widened to whole
// seconds, From down and To up, the same on both sides. With late_rows, the
// counter's growth from From to w.At must also equal the rows lost, of each
// log type. Values are Loki's counts, in the check's log type order.
func (s Sources) EvaluateDelivery(ctx context.Context, sig scenario.Signal, w Window) (Result, error) {
	d := sig.Delivery
	if d == nil {
		return Result{}, errors.New("not a delivery check")
	}
	if s.Loki == "" || s.Ledger == nil || s.Application == "" {
		return Result{}, errors.New("a delivery check needs a Loki URL, the Ledger and the application")
	}
	if d.LateRows != "" && s.Prometheus == "" {
		return Result{}, errors.New("a delivery check with late_rows needs a Prometheus URL")
	}
	if !w.To.After(w.From) {
		return Result{}, fmt.Errorf("the rows' window %s to %s is empty", w.From.Format(time.RFC3339Nano), w.To.Format(time.RFC3339Nano))
	}
	from, to := w.From.Truncate(time.Second), w.To.Truncate(time.Second)
	if to.Before(w.To) {
		to = to.Add(time.Second)
	}
	seconds := int64(to.Sub(from) / time.Second)
	r := Result{Pass: true}
	for _, logType := range d.LogTypes {
		var written, lost int64
		var err error
		if d.LostAfter > 0 {
			written, lost, err = s.Ledger.Count(ctx, logType, from, to, d.LostAfter.D())
		} else {
			written, _, err = s.Ledger.Count(ctx, logType, from, to, 0)
		}
		if err != nil {
			return Result{}, fmt.Errorf("ledger: %w", err)
		}
		query := fmt.Sprintf("sum(count_over_time({service_name=%s, outsystems_log_type=%s} [%ds]))",
			strconv.Quote(s.Application), strconv.Quote(logType), seconds)
		values, err := s.lokiQuery(ctx, s.Loki, query, to)
		if err != nil {
			return Result{}, fmt.Errorf("%s: %w", logType, err)
		}
		var lines int64
		if len(values) > 0 {
			lines = int64(values[0])
		}
		c := DeliveryCount{LogType: logType, Expected: written - lost, Loki: lines, Written: written, Lost: lost}
		r.Pass = r.Pass && c.Loki == c.Expected
		if d.LateRows != "" {
			at := w.At
			if at.IsZero() {
				at = to
			}
			late, err := s.counterGrowth(ctx, withLabel(d.LateRows, "log_type", logType), from, at)
			if err != nil {
				return Result{}, fmt.Errorf("%s late_rows: %w", logType, err)
			}
			c.Late = &late
			r.Pass = r.Pass && late == lost
		}
		r.Delivery = append(r.Delivery, c)
		r.Values = append(r.Values, float64(lines))
	}
	r.Series = len(r.Values)
	var written int64
	for _, c := range r.Delivery {
		written += c.Written
	}
	if written == 0 {
		r.Pass = false
		return r, errors.New("PatientZero committed no rows in the window: is the load running?")
	}
	return r, nil
}

// counterGrowth is how much the sum of the counters selector selects grew
// from from to to; no series counts as 0.
func (s Sources) counterGrowth(ctx context.Context, selector string, from, to time.Time) (int64, error) {
	var at [2]float64
	for i, t := range []time.Time{from, to} {
		values, err := s.promQuery(ctx, s.Prometheus, "sum("+selector+")", t)
		if err != nil {
			return 0, err
		}
		if len(values) > 0 {
			at[i] = values[0]
		}
	}
	return int64(at[1] - at[0]), nil
}

// withLabel adds the matcher name="value" to selector, a metric name with
// optional matchers.
func withLabel(selector, name, value string) string {
	matcher := name + "=" + strconv.Quote(value)
	if strings.HasSuffix(selector, "}") {
		inside := strings.TrimSpace(selector[strings.Index(selector, "{")+1 : len(selector)-1])
		if inside == "" {
			return strings.TrimSuffix(selector, "}") + matcher + "}"
		}
		return strings.TrimSuffix(selector, "}") + ", " + matcher + "}"
	}
	return selector + "{" + matcher + "}"
}

const maxValues = 20

// Evaluate runs sig once, at time at.
func (s Sources) Evaluate(ctx context.Context, sig scenario.Signal, at time.Time) (Result, error) {
	base := s.URLFor(sig.Kind())
	if base == "" {
		return Result{}, fmt.Errorf("no %s URL is configured", sourceName(sig.Kind()))
	}
	switch sig.Kind() {
	case scenario.KindDelivery:
		return Result{}, errors.New("a delivery check is evaluated over its rows' window: EvaluateDelivery")
	case scenario.KindAlert:
		n, err := s.activeAlerts(ctx, base, sig)
		if err != nil {
			return Result{}, err
		}
		pass := n > 0
		if sig.State == scenario.StateResolved {
			pass = n == 0
		}
		return Result{Pass: pass, Values: []float64{float64(n)}, Series: 1}, nil
	default:
		cond, err := scenario.ParseCondition(sig.Condition)
		if err != nil {
			return Result{}, err
		}
		var values []float64
		if sig.Kind() == scenario.KindPromQL {
			values, err = s.promQuery(ctx, base, sig.PromQL, at)
		} else {
			values, err = s.lokiQuery(ctx, base, sig.LogQL, at)
		}
		if err != nil {
			return Result{}, err
		}
		r := Result{Pass: cond.Holds(values), Series: len(values)}
		sort.Float64s(values)
		if len(values) > maxValues {
			values = values[:maxValues]
		}
		r.Values = values
		return r, nil
	}
}

func sourceName(kind string) string {
	switch kind {
	case scenario.KindPromQL:
		return "Prometheus"
	case scenario.KindLogQL, scenario.KindDelivery:
		return "Loki"
	default:
		return "Alertmanager"
	}
}

func (s Sources) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (s Sources) get(ctx context.Context, base, path string, q url.Values, v any) error {
	u, err := url.Parse(strings.TrimRight(base, "/") + path)
	if err != nil {
		return err
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return err
	}
	if len(body) > maxBody {
		return fmt.Errorf("GET %s: response larger than %d bytes", path, maxBody)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d: %s", path, resp.StatusCode, snippet(body))
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	return nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// queryResponse is the Prometheus HTTP API's instant query response, which
// Loki's /loki/api/v1/query also uses.
type queryResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
}

func (s Sources) promQuery(ctx context.Context, base, query string, at time.Time) ([]float64, error) {
	q := url.Values{"query": {query}, "time": {strconv.FormatFloat(float64(at.UnixMilli())/1000, 'f', 3, 64)}}
	var r queryResponse
	if err := s.get(ctx, base, "/api/v1/query", q, &r); err != nil {
		return nil, err
	}
	return values(r)
}

func (s Sources) lokiQuery(ctx context.Context, base, query string, at time.Time) ([]float64, error) {
	q := url.Values{"query": {query}, "time": {strconv.FormatInt(at.UnixNano(), 10)}}
	var r queryResponse
	if err := s.get(ctx, base, "/loki/api/v1/query", q, &r); err != nil {
		return nil, err
	}
	return values(r)
}

// values reads an instant query's result: the value of every series of a
// vector, or a scalar. Loki answers a log query (no metric) on its instant
// endpoint with an error; a signal counts lines with count_over_time.
func values(r queryResponse) ([]float64, error) {
	if r.Status != "success" {
		return nil, fmt.Errorf("query failed: %s", r.Error)
	}
	switch r.Data.ResultType {
	case "vector":
		var vec []struct {
			Value [2]json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(r.Data.Result, &vec); err != nil {
			return nil, err
		}
		out := make([]float64, 0, len(vec))
		for _, v := range vec {
			f, err := sampleValue(v.Value[1])
			if err != nil {
				return nil, err
			}
			out = append(out, f)
		}
		return out, nil
	case "scalar":
		var sc [2]json.RawMessage
		if err := json.Unmarshal(r.Data.Result, &sc); err != nil {
			return nil, err
		}
		f, err := sampleValue(sc[1])
		if err != nil {
			return nil, err
		}
		return []float64{f}, nil
	case "streams":
		return nil, errors.New("the query returns log lines; a signal needs a metric query such as sum(count_over_time(...))")
	case "matrix":
		return nil, errors.New("the query returns a range vector; a signal needs an instant vector or scalar")
	}
	return nil, fmt.Errorf("unknown result type %q", r.Data.ResultType)
}

func sampleValue(raw json.RawMessage) (float64, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, fmt.Errorf("sample value %s is not a string", raw)
	}
	return strconv.ParseFloat(s, 64)
}

// activeAlerts counts the alerts Alertmanager holds active (not silenced or
// inhibited) with the signal's alert name and labels.
func (s Sources) activeAlerts(ctx context.Context, base string, sig scenario.Signal) (int, error) {
	filters := []string{matcher("alertname", sig.Alert)}
	keys := make([]string, 0, len(sig.Labels))
	for k := range sig.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		filters = append(filters, matcher(k, sig.Labels[k]))
	}
	q := url.Values{
		"active":      {"true"},
		"silenced":    {"false"},
		"inhibited":   {"false"},
		"unprocessed": {"false"},
		"filter":      filters,
	}
	var alerts []struct {
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if err := s.get(ctx, base, "/api/v2/alerts", q, &alerts); err != nil {
		return 0, err
	}
	n := 0
	for _, a := range alerts {
		if a.Status.State == "active" {
			n++
		}
	}
	return n, nil
}

// matcher is an Alertmanager equality matcher, the value quoted.
func matcher(name, value string) string {
	return name + "=" + strconv.Quote(value)
}
