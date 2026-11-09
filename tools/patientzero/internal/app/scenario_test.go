package app

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"atrium/tools/patientzero/internal/inject"
	"atrium/tools/patientzero/internal/oslog"
	"atrium/tools/patientzero/internal/scenario"
	"atrium/tools/patientzero/internal/shop"
	"atrium/tools/patientzero/internal/traffic"
)

// switchOn starts a fault of the given type on s's injector.
func switchOn(t *testing.T, s *Server, typ string, params map[string]any) {
	t.Helper()
	if s.Faults == nil {
		s.Faults = &inject.Injector{Sleep: func(context.Context, time.Duration) error { return nil }}
	}
	reg := scenario.NewRegistry()
	inject.Register(reg, s.Faults, Modules)
	f, err := reg.New(scenario.FaultSpec{Type: typ, Params: params})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// An injected failure is a real failed request: an error row under the
// request's Request_Key, linked from its other rows, and the entry point's
// usual failure response. Its business SQL does not run.
func TestInjectedErrorWritesAnErrorRow(t *testing.T) {
	store := &fakeStore{}
	s, rec := newServer(t, store)
	switchOn(t, s, "errors", map[string]any{"entry_points": []any{EntryPlaceOrder}, "ratio": 1, "message": "checkout-errors"})

	w := do(t, s, http.MethodPost, PathPlaceOrder, `{"productId":1,"quantity":2}`, maria)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500: %s", w.Code, w.Body)
	}
	sameRequest(t, rec, "mobile_request error")
	errRow := rec.byType()["error"][0]
	if errRow["Message"] != "injected by scenario: checkout-errors" {
		t.Errorf("Message %q", errRow["Message"])
	}
	if stack := errRow["Stack"].(string); strings.Contains(stack, "System.Data") {
		t.Errorf("an injected failure is the action's own, not the data provider's:\n%s", stack)
	}
	if got := rec.byType()["mobile_request"][0]["Error_Id"]; got != errRow["Id"] {
		t.Errorf("mobile_request.Error_Id %v, want %v", got, errRow["Id"])
	}

	// Other entry points are untouched.
	rec.rows = nil
	if w := do(t, s, http.MethodGet, PathOrderList, "", maria); w.Code != http.StatusOK {
		t.Fatalf("OrderList %d with a PlaceOrder fault", w.Code)
	}
	sameRequest(t, rec, "screen")
}

// Injected latency is spent before the business SQL, so it is in the row's
// Duration as a slow database would be.
func TestInjectedLatencyIsInTheDuration(t *testing.T) {
	s, rec := newServer(t, &fakeStore{})
	var clockNow = t0
	s.Now = func() time.Time { return clockNow }
	s.Faults = &inject.Injector{Sleep: func(_ context.Context, d time.Duration) error {
		clockNow = clockNow.Add(d)
		return nil
	}}
	switchOn(t, s, "latency", map[string]any{"entry_points": []any{EntryOrderList}, "delay": "3s"})
	if w := do(t, s, http.MethodGet, PathOrderList, "", maria); w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if got := rec.byType()["screen"][0]["Duration"]; got != int64(3000) {
		t.Errorf("Duration %v, want 3000 ms", got)
	}
}

func TestTrafficIsCountedDuringARun(t *testing.T) {
	s, _ := newServer(t, &fakeStore{})
	s.Traffic = &traffic.Counter{}
	switchOn(t, s, "errors", map[string]any{"entry_points": []any{EntryGetOrders}, "ratio": 1})
	s.Traffic.Begin(t0)
	do(t, s, http.MethodGet, PathOrderList, "", maria)
	do(t, s, http.MethodGet, PathOrderList, "", nil)
	do(t, s, http.MethodPost, PathPlaceOrder, `{"productId":99,"quantity":1}`, maria) // refused: failed, not injected
	do(t, s, http.MethodGet, PathAPIOrders, "", maria)                                // injected
	_, run := s.Traffic.End(t0.Add(time.Minute))
	if run.Requests != 4 || run.Failed != 2 || run.Injected != 1 || run.Users != 1 || run.Sessions != 2 {
		t.Errorf("run summary %+v", run)
	}
	if ep := run.EntryPoints[EntryOrderList]; ep.Requests != 2 || ep.AnonymousCalls != 1 {
		t.Errorf("OrderList %+v", ep)
	}
	if ep := run.EntryPoints[EntryGetOrders]; ep.Injected != 1 {
		t.Errorf("GetOrders %+v", ep)
	}
}

func TestAdminIsMountedOnlyWhenGiven(t *testing.T) {
	s, _ := newServer(t, &fakeStore{})
	if w := do(t, s, http.MethodGet, "/admin/scenarios", "", nil); w.Code != http.StatusNotFound {
		t.Fatalf("no admin handler: %d, want 404", w.Code)
	}
	s.Admin = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	if w := do(t, s, http.MethodPost, "/admin/run/stop", "", nil); w.Code != http.StatusTeapot {
		t.Fatalf("admin handler: %d", w.Code)
	}
}

// A bad deploy fails its module's requests with the new version's
// exception, as the platform logs it: the exception's own message, not the
// errors fault's marker, and the new eSpaceVer in EnvironmentInformation.
// The rollback logs the previous version again.
func TestDeployFailsWithTheNewVersionsException(t *testing.T) {
	s, rec := newServer(t, &fakeStore{})
	s.Traffic = &traffic.Counter{}
	s.Traffic.Begin(t0)
	do(t, s, http.MethodPost, PathPlaceOrder, `{"productId":99,"quantity":1}`, maria) // refused, before the deploy
	before := rec.byType()["error"][0]
	if env := before["EnvironmentInformation"].(string); !strings.HasPrefix(env, "eSpaceVer: 1 (Id=102)") {
		t.Errorf("EnvironmentInformation before the deploy %q, want eSpaceVer 1 of PatientZero_App", env)
	}

	s.Faults = &inject.Injector{}
	reg := scenario.NewRegistry()
	inject.Register(reg, s.Faults, Modules)
	f, err := reg.New(scenario.FaultSpec{Type: "deploy", Params: map[string]any{"module": "PatientZero_App", "error_ratio": 1}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.rows = nil
	if w := do(t, s, http.MethodPost, PathPlaceOrder, `{"productId":1,"quantity":2}`, maria); w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500: %s", w.Code, w.Body)
	}
	sameRequest(t, rec, "mobile_request error")
	errRow := rec.byType()["error"][0]
	if errRow["Message"] != inject.DefaultException {
		t.Errorf("Message %q, want the exception alone", errRow["Message"])
	}
	if env := errRow["EnvironmentInformation"].(string); !strings.HasPrefix(env, "eSpaceVer: 2 (Id=102)") || !strings.Contains(env, "FE-PZ-1") {
		t.Errorf("EnvironmentInformation %q, want the new version 2 and the front-end", env)
	}
	if stack := errRow["Stack"].(string); strings.Contains(stack, "System.Data") || !strings.Contains(stack, "PatientZero_App.PlaceOrder") {
		t.Errorf("a deploy's failure is the action's own:\n%s", stack)
	}
	// The module's other entry points would fail too; other modules' do not.
	rec.rows = nil
	if w := do(t, s, http.MethodGet, PathAPIOrders, "", maria); w.Code != http.StatusOK {
		t.Fatalf("GetOrders %d during a PatientZero_App deploy", w.Code)
	}
	if err := f.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.rows = nil
	do(t, s, http.MethodPost, PathPlaceOrder, `{"productId":99,"quantity":1}`, maria)
	if env := rec.byType()["error"][0]["EnvironmentInformation"].(string); !strings.HasPrefix(env, "eSpaceVer: 1 (Id=102)") {
		t.Errorf("EnvironmentInformation after the rollback %q, want eSpaceVer 1", env)
	}
	_, run := s.Traffic.End(t0.Add(time.Minute))
	if run.Requests != 4 || run.Failed != 3 || run.Injected != 1 {
		t.Errorf("run summary %+v, want 4 requests, 3 failed, 1 injected", run)
	}
}

// SQL latency reaches the Store, which spends it in SQL Server (the fake
// store moves the clock instead). A statement slower than SlowSQL gets the
// platform's slow-query row under the request's Request_Key, which the
// collector counts as slow SQL; a fast one gets none.
func TestSlowSQLIsSpentInTheStoreAndLogged(t *testing.T) {
	now := t0
	store := &fakeStore{elapse: func(d time.Duration) { now = now.Add(d) }}
	s, rec := newServer(t, store)
	s.Now = func() time.Time { return now }
	s.SlowSQL = DefaultSlowSQL
	s.Traffic = &traffic.Counter{}
	s.Traffic.Begin(t0)

	// No fault: the statement takes no time, so no slow-query row.
	do(t, s, http.MethodGet, PathOrderList, "", maria)
	sameRequest(t, rec, "screen")

	switchOn(t, s, "sql-latency", map[string]any{"entry_points": []any{EntryOrderList, EntryGetOrders}, "delay": "3s"})
	for _, c := range []struct{ method, path, body, types string }{
		{http.MethodGet, PathOrderList, "", "screen general"},
		{http.MethodGet, PathAPIOrders, "", "general integration general"},
		{http.MethodPost, PathPlaceOrder, `{"productId":1,"quantity":1}`, "mobile_request"},
	} {
		rec.rows = nil
		if w := do(t, s, c.method, c.path, c.body, maria); w.Code != http.StatusOK {
			t.Fatalf("%s: status %d", c.path, w.Code)
		}
		sameRequest(t, rec, c.types)
		general := rec.byType()["general"]
		if c.path == PathPlaceOrder {
			continue
		}
		slow := general[len(general)-1]
		want := shop.Statements["Orders"] + " took 3000 ms"
		if slow["Module_Name"] != "SLOWSQL" || slow["Message"] != want || slow["Message_Type"] != "Info" {
			t.Errorf("%s slow-query row %v, want SLOWSQL and %q", c.path, slow, want)
		}
		if slow["Instant"] != now || slow["Espace_Name"] == nil || slow["Entrypoint_Name"] == "" {
			t.Errorf("%s slow-query row %v", c.path, slow)
		}
	}
	if got := store.delays; len(got) != 4 || got[0] != 0 || got[1] != 3*time.Second || got[2] != 3*time.Second || got[3] != 0 {
		t.Errorf("SQL delays the store got %v, want [0 3s 3s 0]", got)
	}
	// The screen's Duration holds the time spent in SQL Server.
	_, run := s.Traffic.End(now)
	if run.SlowSQL != 2 || run.EntryPoints[EntryOrderList].SlowSQL != 1 || run.EntryPoints[EntryOrderList].DurationMsMax != 3000 {
		t.Errorf("run summary %+v, want 2 slow-query rows and a 3 s OrderList", run)
	}
}

// Committed rows are counted in the Ledger by their Instant, and in the
// run's traffic per log type; a log-flood fault adds general rows of its
// module to each request, under the request's Request_Key.
func TestCommittedRowsAreCountedAndFloodRowsAdded(t *testing.T) {
	s, rec := newServer(t, &fakeStore{})
	s.Ledger = &oslog.Ledger{Now: func() time.Time { return t0 }}
	s.Traffic = &traffic.Counter{}
	s.Traffic.Begin(t0)
	switchOn(t, s, "log-flood", map[string]any{"entry_points": []any{EntryOrderList}, "rows": 3, "bytes": 50})
	do(t, s, http.MethodGet, PathOrderList, "", maria)
	sameRequest(t, rec, "screen general general general")
	flood := rec.byType()["general"]
	if msg := flood[2]["Message"].(string); len(msg) != 50 || !strings.HasPrefix(msg, "Preparation: step 3 of 3 ") {
		t.Errorf("flood Message %q, want 50 characters naming the step", msg)
	}
	if flood[0]["Module_Name"] != "PatientZero_Web" || flood[0]["Message_Type"] != "Trace" {
		t.Errorf("flood row %v", flood[0])
	}
	do(t, s, http.MethodPost, PathPlaceOrder, `{"productId":1,"quantity":1}`, maria)
	window := func(typ string) int64 { return s.Ledger.Count(typ, t0.Add(-time.Minute), t0.Add(time.Minute), 0).Rows }
	if window("screen") != 1 || window("general") != 3 || window("mobile_request") != 1 {
		t.Errorf("ledger screen %d, general %d, mobile_request %d; want 1, 3, 1", window("screen"), window("general"), window("mobile_request"))
	}
	_, run := s.Traffic.End(t0.Add(time.Minute))
	if run.Rows["screen"] != 1 || run.Rows["general"] != 3 || run.Rows["mobile_request"] != 1 {
		t.Errorf("run rows %v", run.Rows)
	}
}

// A commit-delay fault answers the request at once and commits its rows
// later; the Ledger counts them as held for that long once they are
// committed, and a failed write is not counted.
func TestHeldRowsAreCommittedLater(t *testing.T) {
	s, _ := newServer(t, &fakeStore{})
	logs := &lockedLogs{}
	s.Logs = logs
	s.Ledger = &oslog.Ledger{Now: func() time.Time { return t0 }}
	switchOn(t, s, "commit-delay", map[string]any{"delay": "50ms"})
	if w := do(t, s, http.MethodGet, PathOrderList, "", maria); w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	count := func(heldOver time.Duration) oslog.Counted {
		return s.Ledger.Count("screen", t0.Add(-time.Minute), t0.Add(time.Minute), heldOver)
	}
	if got := count(0); got.Rows != 0 || logs.written() != 0 {
		t.Fatalf("rows committed at once: ledger %+v, %d written", got, logs.written())
	}
	deadline := time.Now().Add(5 * time.Second)
	for count(0).Rows == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := count(0); got.Rows != 1 || got.Held != 1 || count(50*time.Millisecond).Held != 0 {
		t.Fatalf("ledger %+v, want the row committed and held for 50ms", got)
	}
	logs.failWith(errors.New("log database down"))
	do(t, s, http.MethodGet, PathOrderList, "", maria)
	time.Sleep(200 * time.Millisecond)
	if got := count(0); got.Rows != 1 {
		t.Fatalf("a failed write was counted: %+v", got)
	}
}

// lockedLogs is a log writer that rows can be written to from the held
// commits' goroutines.
type lockedLogs struct {
	mu   sync.Mutex
	rows int
	fail error
}

func (l *lockedLogs) Write(_ context.Context, rows []oslog.Row) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fail != nil {
		return l.fail
	}
	l.rows += len(rows)
	return nil
}

func (l *lockedLogs) written() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rows
}

func (l *lockedLogs) failWith(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fail = err
}
