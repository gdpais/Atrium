package scale

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MaxQuerySeries is Loki's max_query_series in the chart's values: the most
// series one query may hold. A distinct count in LogQL,
// count(sum by (Session_Id) (...)), holds one series per value counted, so a
// panel's own value is how close it comes.
const MaxQuerySeries = 20000

// Loki is the query API of the Loki that holds a generated log's rows, as
// the patientzero-scale collector delivered them.
type Loki struct {
	URL  string
	HTTP *http.Client
}

// Sample is one series of an instant query.
type Sample struct {
	Labels map[string]string
	Value  float64
}

// Query runs an instant LogQL metric query at t.
func (l Loki) Query(ctx context.Context, query string, t time.Time) ([]Sample, error) {
	q := url.Values{"query": {query}, "time": {t.UTC().Format(time.RFC3339Nano)}}
	var result []struct {
		Metric map[string]string `json:"metric"`
		Value  [2]any            `json:"value"`
	}
	if err := l.get(ctx, "/loki/api/v1/query", q, "vector", &result); err != nil {
		return nil, err
	}
	out := make([]Sample, 0, len(result))
	for _, r := range result {
		v, err := sampleValue(r.Value[1])
		if err != nil {
			return nil, err
		}
		out = append(out, Sample{Labels: r.Metric, Value: v})
	}
	return out, nil
}

// QueryRange runs a LogQL metric query over [start, end] every step and
// returns each series' largest value.
func (l Loki) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]Sample, error) {
	q := url.Values{"query": {query}, "start": {start.UTC().Format(time.RFC3339Nano)},
		"end": {end.UTC().Format(time.RFC3339Nano)}, "step": {strconv.FormatFloat(step.Seconds(), 'f', -1, 64)}}
	var result []struct {
		Metric map[string]string `json:"metric"`
		Values [][2]any          `json:"values"`
	}
	if err := l.get(ctx, "/loki/api/v1/query_range", q, "matrix", &result); err != nil {
		return nil, err
	}
	out := make([]Sample, 0, len(result))
	for _, r := range result {
		s := Sample{Labels: r.Metric}
		for _, p := range r.Values {
			v, err := sampleValue(p[1])
			if err != nil {
				return nil, err
			}
			s.Value = max(s.Value, v)
		}
		out = append(out, s)
	}
	return out, nil
}

func (l Loki) get(ctx context.Context, path string, q url.Values, resultType string, result any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(l.URL, "/")+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	client := l.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("loki: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var r struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string          `json:"resultType"`
			Result     json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return fmt.Errorf("loki: %w", err)
	}
	if r.Status != "success" || r.Data.ResultType != resultType {
		return fmt.Errorf("loki: status %q, result type %q, want success and %s", r.Status, r.Data.ResultType, resultType)
	}
	return json.Unmarshal(r.Data.Result, result)
}

func sampleValue(v any) (float64, error) {
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("loki: sample value %v is not a string", v)
	}
	return strconv.ParseFloat(s, 64)
}

// Panel is a dashboard panel whose query a check runs: a distinct count, or
// a top list whose distinct count it also takes.
type Panel struct {
	Dashboard string
	Title     string
	Expr      string
}

// CheckedPanels are the panels a production-sized log could break: distinct
// counts of users, sessions and error fingerprints, which Loki holds one
// series per value of.
var CheckedPanels = []struct{ File, Title string }{
	{"outsystems-overview.json", "Active users (5m)"},
	{"outsystems-overview.json", "Active sessions (5m)"},
	{"outsystems-errors.json", "Distinct errors"},
	{"outsystems-errors.json", "Users affected"},
	{"outsystems-errors.json", "Top errors"},
}

// LoadPanels reads the checked panels' queries from the chart's dashboards.
func LoadPanels(dir string) ([]Panel, error) {
	var out []Panel
	for _, c := range CheckedPanels {
		b, err := os.ReadFile(filepath.Join(dir, c.File))
		if err != nil {
			return nil, err
		}
		var d struct {
			Title  string `json:"title"`
			Panels []struct {
				Title   string `json:"title"`
				Targets []struct {
					Expr string `json:"expr"`
				} `json:"targets"`
			} `json:"panels"`
		}
		if err := json.Unmarshal(b, &d); err != nil {
			return nil, fmt.Errorf("%s: %w", c.File, err)
		}
		found := false
		for _, p := range d.Panels {
			if p.Title == c.Title && len(p.Targets) == 1 {
				out = append(out, Panel{Dashboard: d.Title, Title: c.Title, Expr: p.Targets[0].Expr})
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("%s has no panel %q with one query: update CheckedPanels", c.File, c.Title)
		}
	}
	return out, nil
}

var (
	unresolved = regexp.MustCompile(`\$\{?\w`)
	topk       = regexp.MustCompile(`^topk\(\d+,\s*(.*)\)$`)
)

// Resolve is a panel's query as Grafana sends it with every variable on All,
// the environment set to env and the dashboard's time range rng.
func (p Panel) Resolve(env string, rng time.Duration) (string, error) {
	q := strings.NewReplacer(
		"${fingerprint:regex}", "",
		"$environment", regexp.QuoteMeta(env),
		"$application", ".+",
		"$module", ".+",
		"$__range", promDuration(rng),
	).Replace(p.Expr)
	if unresolved.MatchString(q) {
		return "", fmt.Errorf("%s / %s: a variable is left in %s", p.Dashboard, p.Title, q)
	}
	return q, nil
}

// Distinct is the query's distinct count: the query itself when it is one
// (count(...)), else count() of a topk's inner aggregation.
func Distinct(q string) string {
	if m := topk.FindStringSubmatch(q); m != nil {
		return "count(" + m[1] + ")"
	}
	return q
}

func promDuration(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return fmt.Sprintf("%ds", d/time.Second)
}

// CheckOptions say which rows in Loki belong to the generated log.
type CheckOptions struct {
	// Environment is the deployment.environment.name the patientzero-scale
	// collector gives the log.
	Environment string
	// From is where its extraction started: the collector's start minus
	// its initial_lookback. Hours that start earlier are not checked.
	From time.Time
}

// HourCheck is one hour of the log in Loki next to its known answers.
type HourCheck struct {
	Expected Bucket
	// Rows are the rows per log type in Loki; the collector delivers at
	// least once, so a redelivered batch makes more.
	Rows map[string]int
	// Users and Sessions count screen rows, whose Instant is where a request
	// starts, as expected.json counts them. PanelUsers and PanelSessions
	// count every log type, as the dashboards do.
	Users, Sessions, PanelUsers, PanelSessions int
	ErrorKinds                                 int
}

// Complete is whether Loki holds every row of the hour.
func (h HourCheck) Complete() bool {
	for t, n := range h.Expected.Rows {
		if h.Rows[t] < n {
			return false
		}
	}
	return true
}

// Exact is whether the distinct counts from screen and error rows match
// expected.json.
func (h HourCheck) Exact() bool {
	return h.Users == h.Expected.Users && h.Sessions == h.Expected.Sessions && h.ErrorKinds == h.Expected.ErrorKinds
}

// PanelCheck is one panel's query over one dashboard time range, or, for
// the 5-minute panels, its largest value across the log.
type PanelCheck struct {
	Panel Panel
	Range string
	At    time.Time
	// Value is the panel's distinct count: the series Loki held for it.
	Value   float64
	Elapsed time.Duration
	Err     string
}

// DayCheck is one whole day's distinct error fingerprints, as Distinct
// errors shows them over 24 hours, next to the day's error kinds.
type DayCheck struct {
	Expected   Bucket
	ErrorKinds int
}

// CheckResult is what a check found.
type CheckResult struct {
	Environment string
	From, To    time.Time
	Hours       []HourCheck
	Days        []DayCheck
	Panels      []PanelCheck
}

// Passed is whether every hour Loki holds in full matched expected.json,
// every day's fingerprints matched its error kinds and every panel query
// ran.
func (r CheckResult) Passed() bool {
	for _, h := range r.Hours {
		if !h.Complete() || !h.Exact() {
			return false
		}
	}
	for _, d := range r.Days {
		if d.ErrorKinds != d.Expected.ErrorKinds {
			return false
		}
	}
	for _, p := range r.Panels {
		if p.Err != "" {
			return false
		}
	}
	return len(r.Hours) > 0
}

// selector picks the generated log's streams in Loki.
func (o CheckOptions) selector() string {
	return fmt.Sprintf(`deployment_environment_name=%q`, o.Environment)
}

// hours are the hours of the log a check reads: those from opts.From on.
func (o CheckOptions) hours(exp *Expected) []Bucket {
	var out []Bucket
	for _, b := range exp.Hours {
		if !b.From.Before(o.From) {
			out = append(out, b)
		}
	}
	return out
}

// hourRows are the rows per log type Loki holds for hour b. expected.json's
// hour is [From, To). Read a nanosecond before To, a [1h] range is that hour
// for the log's millisecond Instants whichever end of the range Loki
// includes.
func hourRows(ctx context.Context, loki Loki, sel string, b Bucket) (map[string]int, error) {
	samples, err := loki.Query(ctx, fmt.Sprintf(`sum by (outsystems_log_type) (count_over_time({%s} [1h]))`, sel), b.To.Add(-time.Nanosecond))
	if err != nil {
		return nil, err
	}
	rows := map[string]int{}
	for _, s := range samples {
		rows[s.Labels["outsystems_log_type"]] = int(s.Value)
	}
	return rows, nil
}

// Progress is how far Loki is through the hours a check reads, while the
// patientzero-scale collector catches up.
type Progress struct {
	Hours int
	// Complete are the hours Loki holds in full, counted from the first up
	// to one it does not.
	Complete int
	// Done is whether Loki holds the last hour in full. The collector
	// delivers each log type in order, so it then holds the others too,
	// unless rows were lost on the way, which the check finds.
	Done bool
}

// CaughtUp reads how far Loki is through the log, one rows query an hour: a
// cheap pass to repeat before Check, which is not.
func CaughtUp(ctx context.Context, loki Loki, exp *Expected, opts CheckOptions) (Progress, error) {
	hours := opts.hours(exp)
	p := Progress{Hours: len(hours)}
	sel := opts.selector()
	for _, b := range hours {
		rows, err := hourRows(ctx, loki, sel, b)
		if err != nil {
			return p, err
		}
		if !(HourCheck{Expected: b, Rows: rows}).Complete() {
			break
		}
		p.Complete++
	}
	if p.Complete == len(hours) {
		p.Done = true
		return p, nil
	}
	last := hours[len(hours)-1]
	rows, err := hourRows(ctx, loki, sel, last)
	if err != nil {
		return p, err
	}
	p.Done = HourCheck{Expected: last, Rows: rows}.Complete()
	return p, nil
}

// Check compares what Loki holds for the generated log with its known
// answers: every hour from opts.From to the log's end, every whole day in
// it, and the checked panels over 1 hour, 24 hours and the whole range
// (their [$__range] panels) or at their busiest (the 5-minute ones).
func Check(ctx context.Context, loki Loki, exp *Expected, panels []Panel, opts CheckOptions) (CheckResult, error) {
	res := CheckResult{Environment: opts.Environment, From: opts.From, To: exp.End}
	sel := opts.selector()
	value := func(q string, t time.Time) (int, error) {
		s, err := loki.Query(ctx, q, t)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", q, err)
		}
		if len(s) == 0 {
			return 0, nil
		}
		return int(s[0].Value), nil
	}
	distinct := func(by, logType, filter string) string {
		stream := sel
		if logType != "" {
			stream += fmt.Sprintf(`,outsystems_log_type=%q`, logType)
		}
		return fmt.Sprintf(`count(sum by (%s) (count_over_time({%s}%s [1h])))`, by, stream, filter)
	}
	users, sessions := ` | User_Id!="" | User_Id!="0"`, ` | Session_Id!=""`
	for _, b := range opts.hours(exp) {
		// The distinct counts are read where hourRows reads the rows.
		at := b.To.Add(-time.Nanosecond)
		rows, err := hourRows(ctx, loki, sel, b)
		if err != nil {
			return res, err
		}
		h := HourCheck{Expected: b, Rows: rows}
		for _, c := range []struct {
			dst                 *int
			by, logType, filter string
		}{
			{&h.Users, "User_Id", "screen", users},
			{&h.Sessions, "Session_Id", "screen", sessions},
			{&h.PanelUsers, "User_Id", "", users},
			{&h.PanelSessions, "Session_Id", "", sessions},
			{&h.ErrorKinds, "error_fingerprint", "error", ""},
		} {
			if *c.dst, err = value(distinct(c.by, c.logType, c.filter), at); err != nil {
				return res, err
			}
		}
		res.Hours = append(res.Hours, h)
	}
	for _, d := range exp.Days {
		if d.From.Before(opts.From) || d.To.After(exp.End) {
			continue
		}
		n, err := value(fmt.Sprintf(`count(sum by (error_fingerprint) (count_over_time({%s,outsystems_log_type="error"} [24h])))`, sel), d.To.Add(-time.Nanosecond))
		if err != nil {
			return res, err
		}
		res.Days = append(res.Days, DayCheck{Expected: d, ErrorKinds: n})
	}

	// The panels at the log's end, over the dashboards' default hour, a day
	// and everything collected; the 5-minute ones across the whole log.
	end := exp.End
	whole := end.Sub(opts.From).Truncate(time.Hour)
	for _, p := range panels {
		if !strings.Contains(p.Expr, "$__range") {
			q, err := p.Resolve(opts.Environment, 0)
			if err != nil {
				return res, err
			}
			started := time.Now()
			s, err := loki.QueryRange(ctx, q, opts.From, end, 5*time.Minute)
			pc := PanelCheck{Panel: p, Range: "busiest 5 minutes of " + promDuration(whole), At: end, Elapsed: time.Since(started)}
			if err != nil {
				pc.Err = err.Error()
			} else if len(s) > 0 {
				pc.Value = s[0].Value
			}
			res.Panels = append(res.Panels, pc)
			continue
		}
		for _, rng := range []time.Duration{time.Hour, 24 * time.Hour, whole} {
			q, err := p.Resolve(opts.Environment, rng)
			if err != nil {
				return res, err
			}
			started := time.Now()
			s, err := loki.Query(ctx, Distinct(q), end)
			pc := PanelCheck{Panel: p, Range: promDuration(rng), At: end, Elapsed: time.Since(started)}
			if err != nil {
				pc.Err = err.Error()
			} else if len(s) > 0 {
				pc.Value = s[0].Value
			}
			res.Panels = append(res.Panels, pc)
		}
	}
	return res, nil
}

// CheckReport writes a check as Markdown.
func CheckReport(w io.Writer, r CheckResult) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# Loki against expected.json for %s\n\n", r.Environment)
	fmt.Fprintf(&b, "Checked %s: the log from %s to %s, as the patientzero-scale collector delivered it.\n\n",
		time.Now().UTC().Format(time.RFC3339), r.From.Format(time.DateTime), r.To.Format(time.DateTime))
	var complete, exact int
	for _, h := range r.Hours {
		if h.Complete() {
			complete++
			if h.Exact() {
				exact++
			}
		}
	}
	fmt.Fprintf(&b, "## Hours\n\n%d of %d hours are in Loki in full; %d of those match expected.json on users, sessions (from screen rows, where a request starts) and error fingerprints. The dashboards count users and sessions on every log type, so a request that runs over the hour also counts in the next one.\n\n",
		complete, len(r.Hours), exact)
	b.WriteString("| Hour | Rows (expected) | Users (expected) | Sessions (expected) | Panel users | Panel sessions | Fingerprints (error kinds) |\n| --- | --- | ---: | ---: | ---: | ---: | ---: |\n")
	for _, h := range r.Hours {
		var got, want int
		for _, n := range h.Rows {
			got += n
		}
		for _, n := range h.Expected.Rows {
			want += n
		}
		mark := ""
		if !h.Complete() {
			mark = " (incomplete)"
		} else if !h.Exact() {
			mark = " **(differs)**"
		}
		fmt.Fprintf(&b, "| %s%s | %d (%d) | %d (%d) | %d (%d) | %d | %d | %d (%d) |\n",
			h.Expected.From.Format("01-02 15:04"), mark, got, want, h.Users, h.Expected.Users, h.Sessions, h.Expected.Sessions,
			h.PanelUsers, h.PanelSessions, h.ErrorKinds, h.Expected.ErrorKinds)
	}
	if len(r.Days) > 0 {
		b.WriteString("\n## Days\n\n| Day | Fingerprints over 24 h | Error kinds |\n| --- | ---: | ---: |\n")
		for _, d := range r.Days {
			fmt.Fprintf(&b, "| %s | %d | %d |\n", d.Expected.From.Format(time.DateOnly), d.ErrorKinds, d.Expected.ErrorKinds)
		}
	}
	fmt.Fprintf(&b, "\n## Panels\n\nEach panel's distinct count is the series Loki holds for its query, against max_query_series = %d.\n\n", MaxQuerySeries)
	b.WriteString("| Dashboard | Panel | Range | Value | Share of max_query_series | Elapsed (ms) |\n| --- | --- | --- | ---: | ---: | ---: |\n")
	sorted := append([]PanelCheck(nil), r.Panels...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Panel.Dashboard < sorted[j].Panel.Dashboard })
	for _, p := range sorted {
		value := fmt.Sprintf("%.0f", p.Value)
		share := fmt.Sprintf("%.1f%%", 100*p.Value/MaxQuerySeries)
		if p.Err != "" {
			value, share = "error: "+p.Err, ""
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %d |\n", p.Panel.Dashboard, p.Panel.Title, p.Range, value, share, p.Elapsed.Milliseconds())
	}
	verdict := "passed"
	if !r.Passed() {
		verdict = "failed"
	}
	fmt.Fprintf(&b, "\nThe check %s.\n", verdict)
	_, err := io.WriteString(w, b.String())
	return err
}
