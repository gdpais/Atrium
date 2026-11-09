package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"atrium/tools/patientzero/internal/inject"
	"atrium/tools/patientzero/internal/journal"
	"atrium/tools/patientzero/internal/scenario"
	"atrium/tools/patientzero/internal/signal"
	"atrium/tools/patientzero/internal/traffic"
)

const token = "s3cret-admin-token"

type failingFault struct{}

func (failingFault) Start(context.Context) error { return errors.New("no such pod") }
func (failingFault) Stop(context.Context) error  { return nil }

// env is a runner over fake Prometheus, Loki and Alertmanager servers whose
// answers follow the injector: the error ratio is 0.5 while a fault is on.
type env struct {
	t       *testing.T
	inj     *inject.Injector
	counter *traffic.Counter
	runner  *Runner
	admin   http.Handler
	dir     string
}

const scenarioTemplate = `
version: 1
name: %s
description: Test scenario.
load: none
steps:
  - name: warm
    duration: 30ms
  - name: hurt
    duration: %s
    fault:
      type: %s
      params: %s
signals:
%s
`

const passingSignals = `
  - name: ratio-rises
    promql: error_ratio
    condition: "> 0.3"
    within: 400ms
  - name: errors-in-loki
    logql: 'sum(count_over_time({service_name="PatientZero"} |= "injected" [1m]))'
    condition: "> 0"
    within: 400ms
  - name: alert-fires
    alert: OutSystemsErrorBudgetBurnFast
    state: firing
    within: 400ms
  - name: ratio-recovers
    promql: error_ratio
    condition: "< 0.1"
    from: end
    within: 400ms
`

func newEnv(t *testing.T, files ...string) *env {
	t.Helper()
	e := &env{t: t, inj: &inject.Injector{}, counter: &traffic.Counter{}, dir: t.TempDir()}
	active := func() bool { return e.inj.Active() > 0 }
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := "0.03"
		if active() {
			v = "0.5"
		}
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[0,"%s"]}]}}`, v)
	}))
	loki := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := "0"
		if active() {
			n = "4"
		}
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[0,"%s"]}]}}`, n)
	}))
	am := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if active() {
			_, _ = w.Write([]byte(`[{"status":{"state":"active"}}]`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(func() { prom.Close(); loki.Close(); am.Close() })

	reg := scenario.NewRegistry()
	inject.Register(reg, e.inj, map[string][]string{"PatientZero_Web": {"OrderList"}, "PatientZero_App": {"PlaceOrder"}, "PatientZero_API": {"GetOrders"}})
	reg.Register(scenario.FaultType{Name: "restart-loki", Scope: scenario.ScopeCluster,
		New: func(map[string]any) (scenario.Fault, error) { return failingFault{}, nil }})
	reg.Register(scenario.FaultType{Name: "broken", Scope: scenario.ScopeInProcess,
		New: func(map[string]any) (scenario.Fault, error) { return failingFault{}, nil }})
	if len(files) == 0 {
		files = []string{fmt.Sprintf(scenarioTemplate, "checkout-errors", "150ms", "errors", "{ratio: 1, entry_points: [PlaceOrder]}", passingSignals)}
	}
	var list []*scenario.Scenario
	for _, f := range files {
		s, err := scenario.Parse([]byte(f), reg)
		if err != nil {
			t.Fatal(err)
		}
		list = append(list, s)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	e.runner = New(ctx, Config{
		Scenarios:       list,
		Registry:        reg,
		Scopes:          []scenario.Scope{scenario.ScopeInProcess},
		Sources:         signal.Sources{Prometheus: prom.URL, Loki: loki.URL, Alertmanager: am.URL},
		Traffic:         e.counter,
		JournalDir:      e.dir,
		SummaryInterval: 40 * time.Millisecond,
		PollInterval:    10 * time.Millisecond,
		Host:            "patientzero-0",
		Log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	e.admin = e.runner.Handler(token)
	t.Cleanup(e.runner.Wait)
	return e
}

func (e *env) do(method, path string, headers ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, nil)
	for _, h := range headers {
		k, v, _ := strings.Cut(h, ": ")
		req.Header.Add(k, v)
	}
	w := httptest.NewRecorder()
	e.admin.ServeHTTP(w, req)
	return w
}

func (e *env) authed(method, path string) *httptest.ResponseRecorder {
	return e.do(method, path, "Authorization: Bearer "+token)
}

// traffic sends requests to the counter until stop is closed, the way the
// entry points report them.
func (e *env) traffic(stop <-chan struct{}) *sync.WaitGroup {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
			err := e.inj.Apply(context.Background(), "PlaceOrder")
			e.counter.Observe(traffic.Request{EntryPoint: "PlaceOrder", User: fmt.Sprint("shopper.", i%3),
				Session: fmt.Sprint("s", i%5), Duration: time.Millisecond, Failed: err != nil, Injected: errors.Is(err, inject.ErrInjected)})
		}
	}()
	return &wg
}

type line struct {
	journal.Line
	data map[string]any
}

func (e *env) journal(run string) []line {
	e.t.Helper()
	w := e.authed("GET", "/admin/runs/"+run+"/journal")
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/x-ndjson" {
		e.t.Fatalf("GET journal: %d %s", w.Code, w.Body)
	}
	var out []line
	sc := bufio.NewScanner(w.Body)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var l line
		if err := json.Unmarshal(sc.Bytes(), &l.Line); err != nil {
			e.t.Fatalf("journal line %q: %v", sc.Text(), err)
		}
		if err := json.Unmarshal(l.Data, &l.data); err != nil {
			e.t.Fatalf("journal data %q: %v", l.Data, err)
		}
		out = append(out, l)
	}
	return out
}

func events(lines []line, event string) []line {
	var out []line
	for _, l := range lines {
		if l.Event == event {
			out = append(out, l)
		}
	}
	return out
}

func TestRunPassesAndWritesTheJournal(t *testing.T) {
	e := newEnv(t)
	stop := make(chan struct{})
	wg := e.traffic(stop)
	w := e.authed("POST", "/admin/scenarios/checkout-errors/start")
	if w.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", w.Code, w.Body)
	}
	var st Status
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	if st.State != StateRunning || !journal.ValidRunID(st.Run) || st.Journal != "/admin/runs/"+st.Run+"/journal" {
		t.Fatalf("start status %+v", st)
	}
	e.runner.Wait()
	close(stop)
	wg.Wait()

	w = e.authed("GET", "/admin/run")
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	if st.State != StatePassed || st.Ended == nil {
		t.Fatalf("final status %+v", st)
	}
	for _, c := range st.Checks {
		if c.Result != ResultPass {
			t.Errorf("check %+v, want pass", c)
		}
	}
	if e.inj.Active() != 0 {
		t.Fatal("the fault is still on after the run")
	}

	lines := e.journal(st.Run)
	for i, l := range lines {
		if l.V != journal.Version || l.Seq != i+1 || l.Run != st.Run {
			t.Fatalf("line %d envelope %+v", i, l.Line)
		}
	}
	if lines[0].Event != journal.RunStart || lines[len(lines)-1].Event != journal.RunEnd {
		t.Fatalf("journal runs %s .. %s, want run_start .. run_end", lines[0].Event, lines[len(lines)-1].Event)
	}
	start := lines[0].data
	if start["scenario"] != "checkout-errors" || start["host"] != "patientzero-0" || len(start["scenario_sha256"].(string)) != 64 {
		t.Errorf("run_start %v", start)
	}
	def := start["definition"].(map[string]any)
	if steps := def["steps"].([]any); len(steps) != 2 {
		t.Errorf("run_start definition steps %v", steps)
	}

	fs, fe := events(lines, journal.FaultStart), events(lines, journal.FaultEnd)
	if len(fs) != 1 || len(fe) != 1 {
		t.Fatalf("%d fault_start, %d fault_end; want 1 each", len(fs), len(fe))
	}
	if fs[0].data["type"] != "errors" || fs[0].data["step"] != "hurt" || fs[0].data["scope"] != "in-process" {
		t.Errorf("fault_start %v", fs[0].data)
	}
	if p := fs[0].data["params"].(map[string]any); p["ratio"] != 1.0 {
		t.Errorf("fault_start params %v", p)
	}
	if fe[0].data["reason"] != "completed" {
		t.Errorf("fault_end %v", fe[0].data)
	}
	// The fault lasted its step: 150ms.
	if d := fe[0].Time.Sub(fs[0].Time); d < 140*time.Millisecond || d > 2*time.Second {
		t.Errorf("fault lasted %s, want about 150ms", d)
	}

	tr := events(lines, journal.Traffic)
	var intervals, failed int64
	var run map[string]any
	for _, l := range tr {
		switch l.data["scope"] {
		case "interval":
			intervals++
			failed += int64(l.data["failed"].(float64))
		case "run":
			run = l.data
		}
	}
	if intervals < 2 || run == nil {
		t.Fatalf("%d interval summaries and run summary %v; want several and one", intervals, run)
	}
	if run["requests"].(float64) == 0 || run["users"] != 3.0 || run["sessions"] != 5.0 {
		t.Errorf("run summary %v; want requests from 3 users in 5 sessions", run)
	}
	if run["failed"].(float64) != float64(failed) || run["injected"].(float64) == 0 {
		t.Errorf("run summary failed %v, the intervals' %d; injected %v", run["failed"], failed, run["injected"])
	}

	checks := events(lines, journal.Check)
	if len(checks) != 4 {
		t.Fatalf("%d check events, want 4", len(checks))
	}
	byName := map[string]map[string]any{}
	for _, c := range checks {
		byName[c.data["signal"].(string)] = c.data
	}
	rec := byName["ratio-recovers"]
	if rec["result"] != ResultPass || rec["kind"] != "promql" || rec["from"] != "end" || rec["condition"] != "< 0.1" {
		t.Errorf("ratio-recovers check %v", rec)
	}
	// Its window opened at the end of hurt, 180ms after the start, once the
	// fault was off: never before its fault_end.
	ws, _ := time.Parse(time.RFC3339Nano, rec["window_start"].(string))
	if d := ws.Sub(lines[0].Time); d < 180*time.Millisecond || d > 2*time.Second || ws.Before(fe[0].Time) {
		t.Errorf("ratio-recovers window opened %s after the start (fault_end at %s), want from 180ms and after the fault ended", d, fe[0].Time.Sub(lines[0].Time))
	}
	if byName["alert-fires"]["kind"] != "alert" || byName["alert-fires"]["state"] != "firing" {
		t.Errorf("alert-fires check %v", byName["alert-fires"])
	}
	end := lines[len(lines)-1].data
	if end["state"] != StatePassed || end["checks"].(map[string]any)["pass"] != 4.0 || end["requests"] != run["requests"] {
		t.Errorf("run_end %v", end)
	}

	w = e.authed("GET", "/admin/runs")
	if !strings.Contains(w.Body.String(), st.Run) {
		t.Errorf("GET /admin/runs: %s", w.Body)
	}
}

func TestSignalThatNeverHoldsFailsTheRun(t *testing.T) {
	e := newEnv(t, fmt.Sprintf(scenarioTemplate, "never", "200ms", "errors", "{ratio: 1}", `
  - name: impossible
    promql: error_ratio
    condition: "> 5"
    within: 60ms
  - name: stays-low-fails
    promql: error_ratio
    condition: "< 0.1"
    mode: always
    within: 60ms
`))
	st, err := e.runner.Start("never")
	if err != nil {
		t.Fatal(err)
	}
	e.runner.Wait()
	st, _ = e.runner.Status()
	if st.State != StateFailed {
		t.Fatalf("state %s, want failed", st.State)
	}
	checks := events(e.journal(st.Run), journal.Check)
	for _, c := range checks {
		if c.data["result"] != ResultFail {
			t.Errorf("check %v, want fail", c.data)
		}
	}
	for _, c := range checks {
		switch c.data["signal"] {
		case "impossible":
			// Evaluated until the window closed, the last value kept.
			if c.data["attempts"].(float64) < 3 || c.data["values"].([]any)[0] != 0.5 {
				t.Errorf("impossible: %v", c.data)
			}
		case "stays-low-fails":
			// always fails at the first evaluation that does not hold.
			if c.data["attempts"] != 1.0 {
				t.Errorf("stays-low-fails: %v", c.data)
			}
		}
	}
}

func TestStopEndsTheRun(t *testing.T) {
	e := newEnv(t, fmt.Sprintf(scenarioTemplate, "long", "1h", "latency", "{delay: 1s}", passingSignals))
	if _, err := e.runner.Start("long"); err != nil {
		t.Fatal(err)
	}
	// Until the fault is on.
	deadline := time.Now().Add(5 * time.Second)
	for e.inj.Active() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// One run at a time.
	w := e.authed("POST", "/admin/scenarios/long/start")
	if w.Code != http.StatusConflict {
		t.Fatalf("second start: %d %s, want 409", w.Code, w.Body)
	}
	w = e.authed("POST", "/admin/run/stop")
	var st Status
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	if w.Code != http.StatusOK || st.State != StateStopped {
		t.Fatalf("stop: %d %s", w.Code, w.Body)
	}
	if e.inj.Active() != 0 {
		t.Fatal("stopping the run left its fault on")
	}
	lines := e.journal(st.Run)
	fe := events(lines, journal.FaultEnd)
	if len(fe) != 1 || fe[0].data["reason"] != "stopped" {
		t.Fatalf("fault_end %v, want reason stopped", fe)
	}
	if rec := events(lines, journal.Check); len(rec) != 4 {
		t.Fatalf("%d checks recorded, want all 4", len(rec))
	}
	for _, c := range events(lines, journal.Check) {
		if c.data["signal"] == "ratio-recovers" && c.data["result"] != ResultSkipped {
			t.Errorf("ratio-recovers %v, want skipped: its window never opened", c.data)
		}
	}
	if end := lines[len(lines)-1]; end.Event != journal.RunEnd || end.data["state"] != StateStopped {
		t.Fatalf("last line %s %v", end.Event, end.data)
	}
	if w := e.authed("POST", "/admin/run/stop"); w.Code != http.StatusConflict {
		t.Fatalf("stop with no run: %d, want 409", w.Code)
	}
}

func TestFaultThatFailsToStartIsAnError(t *testing.T) {
	e := newEnv(t, fmt.Sprintf(scenarioTemplate, "broken", "1h", "broken", "{}", passingSignals))
	if _, err := e.runner.Start("broken"); err != nil {
		t.Fatal(err)
	}
	e.runner.Wait()
	st, _ := e.runner.Status()
	if st.State != StateError || !strings.Contains(st.Error, "no such pod") {
		t.Fatalf("status %+v, want error with the fault's", st)
	}
	lines := e.journal(st.Run)
	if fe := events(lines, journal.FaultEnd); len(fe) != 1 || fe[0].data["reason"] != "error" || len(events(lines, journal.FaultStart)) != 0 {
		t.Fatalf("fault events %v", fe)
	}
}

func TestAdminAuthComesFirst(t *testing.T) {
	e := newEnv(t)
	paths := []struct{ method, path string }{
		{"GET", "/admin/scenarios"},
		{"POST", "/admin/scenarios/checkout-errors/start"},
		{"GET", "/admin/run"},
		{"POST", "/admin/run/stop"},
		{"GET", "/admin/runs"},
		{"DELETE", "/admin/scenarios"},
		{"GET", "/admin/nothing-here"},
	}
	for _, p := range paths {
		for _, h := range [][]string{
			nil,
			{"Authorization: Bearer wrong"},
			{"Authorization: Basic " + token},
			{"Authorization: Bearer " + token, "Authorization: Bearer " + token},
			{"Authorization: Bearer"},
		} {
			w := e.do(p.method, p.path, h...)
			if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") == "" || w.Body.Len() != 0 {
				t.Errorf("%s %s with %v: %d %q, want a bare 401", p.method, p.path, h, w.Code, w.Body)
			}
		}
	}
	if st, ok := e.runner.Status(); ok {
		t.Fatalf("an unauthenticated request started a run: %+v", st)
	}
	// Authenticated: routing applies; the scheme is case-insensitive.
	if w := e.do("DELETE", "/admin/scenarios", "Authorization: bearer "+token); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE with the token: %d, want 405", w.Code)
	}
	if w := e.authed("GET", "/admin/run"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"idle"`) || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("GET /admin/run before any run: %d %s", w.Code, w.Body)
	}

	// No token configured: everything is refused.
	closed := e.runner.Handler(" ")
	req := httptest.NewRequest("GET", "/admin/scenarios", nil)
	req.Header.Set("Authorization", "Bearer ")
	w := httptest.NewRecorder()
	closed.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no token configured: %d, want 401", w.Code)
	}
}

func TestListingAndRefusals(t *testing.T) {
	cluster := fmt.Sprintf(scenarioTemplate, "loki-restart", "1m", "restart-loki", "{}", passingSignals)
	e := newEnv(t, fmt.Sprintf(scenarioTemplate, "checkout-errors", "150ms", "errors", "{ratio: 1}", passingSignals), cluster)
	w := e.authed("GET", "/admin/scenarios")
	var listing struct {
		Scenarios  []Listing          `json:"scenarios"`
		FaultTypes []FaultTypeListing `json:"fault_types"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listing); err != nil || len(listing.Scenarios) != 2 {
		t.Fatalf("listing %s: %v", w.Body, err)
	}
	ce, lr := listing.Scenarios[0], listing.Scenarios[1]
	if !ce.Runnable || ce.Load != "none" || ce.MaxDurationSeconds < 0.58 || ce.MaxDurationSeconds > 0.59 || len(ce.Signals) != 4 {
		t.Errorf("checkout-errors listing %+v; max duration is 30ms + 150ms + 400ms", ce)
	}
	if lr.Runnable || !strings.Contains(lr.Reason, "acts on the cluster") {
		t.Errorf("loki-restart listing %+v, want not runnable here", lr)
	}
	for _, ft := range listing.FaultTypes {
		if ft.Name == "restart-loki" && (ft.Runnable || ft.Scope != "cluster") {
			t.Errorf("fault type listing %+v", ft)
		}
	}
	if w := e.authed("POST", "/admin/scenarios/loki-restart/start"); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("start a cluster scenario: %d %s, want 422", w.Code, w.Body)
	}
	if w := e.authed("POST", "/admin/scenarios/nope/start"); w.Code != http.StatusNotFound {
		t.Errorf("start an unknown scenario: %d, want 404", w.Code)
	}
	for _, p := range []string{"/admin/runs/..%2F..%2Fetc%2Fpasswd/journal", "/admin/runs/20261022T100000Z-x-0000/journal"} {
		if w := e.authed("GET", p); w.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", p, w.Code)
		}
	}

	// A scenario whose signals need a source this runner lacks.
	e.runner.cfg.Sources.Loki = ""
	if w := e.authed("POST", "/admin/scenarios/checkout-errors/start"); w.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(w.Body.String(), "needs a logql URL") {
		t.Errorf("start without Loki: %d %s, want 422", w.Code, w.Body)
	}
}

// windowLedger answers every log type with rows, and records the windows a
// delivery check asked it for.
type windowLedger struct {
	mu    sync.Mutex
	rows  int64
	asked [][2]time.Time
}

func (l *windowLedger) Count(_ context.Context, _ string, from, to time.Time, _ time.Duration) (int64, int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.asked = append(l.asked, [2]time.Time{from, to})
	return l.rows, 0, nil
}

// A delivery check counts the rows of its steps as they actually ran: from
// the start of its from_step to the end of its to_step, once its window has
// opened, and the journal records that window and each log type's counts.
func TestDeliveryCheckCountsTheRowsOfItsSteps(t *testing.T) {
	loki := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[0,"7"]}]}}`)
	}))
	defer loki.Close()
	reg := scenario.NewRegistry()
	inject.Register(reg, &inject.Injector{}, map[string][]string{"PatientZero_Web": {"OrderList"}})
	file := fmt.Sprintf(scenarioTemplate, "deliver", "60ms", "latency", "{delay: 1ms}", `
  - name: every-row
    delivery: {log_types: [screen, general]}
    step: hurt
    from: end
    within: 300ms
`)
	s, err := scenario.Parse([]byte(file), reg)
	if err != nil {
		t.Fatal(err)
	}
	ledger := &windowLedger{rows: 7}
	cfg := Config{
		Scenarios: []*scenario.Scenario{s}, Registry: reg, Scopes: []scenario.Scope{scenario.ScopeInProcess},
		Sources:    signal.Sources{Loki: loki.URL},
		JournalDir: t.TempDir(), PollInterval: 10 * time.Millisecond, SummaryInterval: 40 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if r := New(context.Background(), cfg); r.Scenarios()[0].Runnable {
		t.Fatal("a delivery check without a ledger must make the scenario not runnable")
	}
	cfg.Sources.Ledger, cfg.Sources.Application = ledger, "PatientZero"
	r := New(context.Background(), cfg)
	started := time.Now()
	st, err := r.Start("deliver")
	if err != nil {
		t.Fatal(err)
	}
	r.Wait()
	if got, _ := r.Status(); got.State != StatePassed {
		t.Fatalf("run %s: %+v", got.State, got.Checks)
	}
	ledger.mu.Lock()
	window := ledger.asked[0]
	ledger.mu.Unlock()
	if window[0].Before(started.Add(-time.Second)) || window[1].Sub(window[0]) < 90*time.Millisecond {
		t.Errorf("rows' window %v, want from the run's start to the end of hurt, at least 90ms", window)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin/runs/"+st.Run+"/journal", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	r.Handler(token).ServeHTTP(w, req)
	var check map[string]any
	for _, l := range strings.Split(strings.TrimSpace(w.Body.String()), "\n") {
		var jl journal.Line
		if err := json.Unmarshal([]byte(l), &jl); err != nil {
			t.Fatal(err)
		}
		if jl.Event == journal.Check {
			_ = json.Unmarshal(jl.Data, &check)
		}
	}
	delivery, _ := check["delivery"].([]any)
	if check["kind"] != "delivery" || check["rows_from"] == nil || check["rows_to"] == nil || len(delivery) != 2 {
		t.Fatalf("check line %v, want the delivery's window and two log types", check)
	}
	if first := delivery[0].(map[string]any); first["log_type"] != "screen" || first["loki"] != 7.0 || first["expected"] != 7.0 {
		t.Errorf("screen count %v", first)
	}
}
