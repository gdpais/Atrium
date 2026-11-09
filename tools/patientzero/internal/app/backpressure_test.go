package app

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"atrium/tools/patientzero/internal/shop"
	"atrium/tools/patientzero/internal/traffic"
)

// gateStore signals entered when a call reaches it (so a test knows the
// limiter has admitted the request and it is holding its token), then blocks
// until release is closed.
type gateStore struct {
	entered chan struct{}
	release chan struct{}
}

func (g *gateStore) Customer(context.Context, string) (int, error) { return 1000, nil }

func (g *gateStore) gate(ctx context.Context) error {
	g.entered <- struct{}{}
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *gateStore) Orders(ctx context.Context, _, _ int) ([]shop.Order, error) {
	if err := g.gate(ctx); err != nil {
		return nil, err
	}
	return []shop.Order{{ID: 7, Product: "Thermometer", Quantity: 1, Total: 12.5}}, nil
}

func (g *gateStore) PlaceOrder(ctx context.Context, _, _, _ int) (shop.Order, error) {
	if err := g.gate(ctx); err != nil {
		return shop.Order{}, err
	}
	return shop.Order{ID: 8}, nil
}

// When MaxInFlight requests are already running, the next is shed with 503 and
// a Retry-After header rather than queued or allowed to open another
// connection. The three entry points share the one budget, the health and
// admin endpoints are never shed, and each shed request is counted in the
// run's traffic and reported in the server log.
func TestLimiterShedsExcessWith503(t *testing.T) {
	gate := &gateStore{entered: make(chan struct{}, 8), release: make(chan struct{})}
	s, _ := newServer(t, &fakeStore{})
	s.Store = gate
	s.MaxInFlight = 1
	s.Admin = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	var logs syncBuffer
	s.Log = slog.New(slog.NewTextHandler(&logs, nil))
	s.shedEvery = 20 * time.Millisecond
	s.Traffic = &traffic.Counter{}
	s.Traffic.Begin(t0)
	handler := s.Handler()

	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr = "10.244.0.9:51000"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}

	// Hold one request in flight (blocked in the store, token taken).
	inflight := make(chan *httptest.ResponseRecorder, 1)
	go func() { inflight <- call(http.MethodGet, PathAPIOrders, "") }()
	<-gate.entered // the request has been admitted and holds the only token

	// While it holds the only token, every entry point is shed.
	for _, r := range []struct{ method, path, body string }{
		{http.MethodGet, PathAPIOrders, ""},
		{http.MethodGet, PathOrderList, ""},
		{http.MethodPost, PathPlaceOrder, `{"productId":1,"quantity":1}`},
	} {
		shed := call(r.method, r.path, r.body)
		if shed.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s while saturated: got %d, want 503", r.method, r.path, shed.Code)
		}
		if shed.Header().Get("Retry-After") == "" {
			t.Errorf("%s %s: shed response has no Retry-After header", r.method, r.path)
		}
		if r.path == PathOrderList {
			checkScreenHeaders(t, shed)
		}
	}
	// The health and admin endpoints still answer.
	for path, want := range map[string]int{"/healthz": http.StatusOK, "/readyz": http.StatusOK, "/admin/runs": http.StatusNoContent} {
		if got := call(http.MethodGet, path, "").Code; got != want {
			t.Errorf("GET %s while saturated: got %d, want %d", path, got, want)
		}
	}

	// Release the first; it completes normally, proving the token is returned.
	close(gate.release)
	first := <-inflight
	if first.Code != http.StatusOK {
		t.Fatalf("first request: got %d, want 200", first.Code)
	}
	// A later request now gets the freed token.
	if again := call(http.MethodGet, PathAPIOrders, ""); again.Code != http.StatusOK {
		t.Fatalf("request after release: got %d, want 200", again.Code)
	}

	// The shed requests are counted apart from the two served.
	_, run := s.Traffic.End(t0.Add(time.Minute))
	if run.Shed != 3 || run.Requests != 2 {
		t.Errorf("run traffic: %d shed and %d requests, want 3 and 2", run.Shed, run.Requests)
	}
	for _, name := range EntryPoints {
		if got := run.EntryPoints[name].Shed; got != 1 {
			t.Errorf("%s: %d shed, want 1", name, got)
		}
	}
	// Every shed request reaches the log, the ones held for the end of an
	// interval included.
	deadline := time.Now().Add(5 * time.Second)
	for shedLogged(t, logs.String()) != 3 {
		if time.Now().After(deadline) {
			t.Fatalf("the log counts %d shed requests, want 3:\n%s", shedLogged(t, logs.String()), logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

var shedAttr = regexp.MustCompile(` shed=(\d+)`)

// shedLogged sums the shed counts of the shedding log lines.
func shedLogged(t *testing.T, logs string) int {
	t.Helper()
	sum := 0
	for _, m := range shedAttr.FindAllStringSubmatch(logs, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatal(err)
		}
		sum += n
	}
	return sum
}

// syncBuffer is a bytes.Buffer the shedding log's flush may write from its
// own goroutine while the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// The first shed request is logged at once; the ones shed within the
// interval after a line are held and logged together when it ends, so a
// burst is counted in full with at most one line per interval.
func TestShedReportPaces(t *testing.T) {
	now := t0
	var lines []int
	var waits []time.Duration
	var flush func()
	p := newShedReport(10*time.Second, func(shed int) { lines = append(lines, shed) })
	p.now = func() time.Time { return now }
	p.after = func(d time.Duration, f func()) { waits, flush = append(waits, d), f }
	check := func(step string, wantLines []int, wantWaits []time.Duration) {
		t.Helper()
		if !slices.Equal(lines, wantLines) || !slices.Equal(waits, wantWaits) {
			t.Fatalf("%s: lines %v and flushes after %v, want %v and %v", step, lines, waits, wantLines, wantWaits)
		}
	}

	p.add()
	check("first", []int{1}, nil)
	now = now.Add(time.Second)
	p.add()
	now = now.Add(3 * time.Second)
	p.add()
	check("within the interval", []int{1}, []time.Duration{9 * time.Second})

	now = t0.Add(10 * time.Second)
	flush()
	check("the interval's end", []int{1, 2}, []time.Duration{9 * time.Second})

	now = now.Add(4 * time.Second)
	p.add()
	check("within the next interval", []int{1, 2}, []time.Duration{9 * time.Second, 6 * time.Second})
	now = now.Add(6 * time.Second)
	flush()
	check("its end", []int{1, 2, 1}, []time.Duration{9 * time.Second, 6 * time.Second})

	now = now.Add(time.Minute)
	p.add()
	check("long after the last line", []int{1, 2, 1, 1}, []time.Duration{9 * time.Second, 6 * time.Second})
}

// A pool whose every connection is in use counts as answering: Ping must not
// wait for a connection to free up, or a slow SQL Server would fail the
// readiness probe while the entry points shed load.
func TestPingDoesNotWaitOnAFullPool(t *testing.T) {
	db := sql.OpenDB(stubConnector{})
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := Ping(context.Background(), db); err != nil {
		t.Fatalf("ping with a free connection: %v", err)
	}
	held, err := db.Conn(context.Background()) // takes the only connection
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := Ping(ctx, db); err != nil {
		t.Fatalf("ping on a full pool: %v, want nil without waiting for a connection", err)
	}
}

// stubConnector is a database/sql driver whose connections do nothing:
// enough for a pool to open them and count them in use.
type stubConnector struct{}

func (stubConnector) Connect(context.Context) (driver.Conn, error) { return stubConn{}, nil }
func (stubConnector) Driver() driver.Driver                        { return stubDriver{} }

type stubDriver struct{}

func (stubDriver) Open(string) (driver.Conn, error) { return stubConn{}, nil }

type stubConn struct{}

func (stubConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("stub: no statements") }
func (stubConn) Close() error                        { return nil }
func (stubConn) Begin() (driver.Tx, error)           { return nil, errors.New("stub: no transactions") }

// A server-action failure never reflects internal error text to the caller:
// a 5xx carries a generic message, and a refused order a fixed 400 message.
func TestServerActionErrorsAreSanitized(t *testing.T) {
	t.Run("internal error is generic 500", func(t *testing.T) {
		store := &fakeStore{fail: errors.New("mssql: login failed for user 'atrium' at tcp:secret-host:1433")}
		s, _ := newServer(t, store)
		req := httptest.NewRequest(http.MethodPost, PathPlaceOrder, strings.NewReader(`{"productId":1,"quantity":1}`))
		req.RemoteAddr = "10.244.0.9:51000"
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusInternalServerError {
			t.Fatalf("got %d, want 500", w.Code)
		}
		msg := serverActionMessage(t, w.Body.Bytes())
		if msg != "Internal Server Error" {
			t.Errorf("message = %q, want generic", msg)
		}
		if strings.Contains(w.Body.String(), "secret-host") || strings.Contains(w.Body.String(), "login failed") {
			t.Errorf("response leaked internal error text: %s", w.Body.String())
		}
	})

	t.Run("malformed body is fixed 400", func(t *testing.T) {
		s, _ := newServer(t, &fakeStore{})
		req := httptest.NewRequest(http.MethodPost, PathPlaceOrder, strings.NewReader(`{"productId":`))
		req.RemoteAddr = "10.244.0.9:51000"
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("got %d, want 400", w.Code)
		}
		if strings.Contains(w.Body.String(), "unexpected EOF") || strings.Contains(w.Body.String(), "json") {
			t.Errorf("response leaked decoder text: %s", w.Body.String())
		}
	})
}

func serverActionMessage(t *testing.T, body []byte) string {
	t.Helper()
	var out struct {
		Exception struct{ Message string } `json:"exception"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode response: %v (%s)", err, body)
	}
	return out.Exception.Message
}
