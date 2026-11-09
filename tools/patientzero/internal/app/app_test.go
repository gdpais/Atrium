package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"atrium/tools/patientzero/internal/logschema"
	"atrium/tools/patientzero/internal/oslog"
	"atrium/tools/patientzero/internal/shop"
)

// fixtureDir is the db-collector's oslog fixtures, the one place the logging
// schema is defined.
const fixtureDir = "../../../../services/db-collector/internal/collectors/outsystems/testdata/oslog"

type fakeStore struct {
	customers map[string]int
	fail      error
	// delays records the SQL delay each Orders and PlaceOrder call got;
	// elapse, when set, is called with it, so a fake clock can move on.
	delays []time.Duration
	elapse func(time.Duration)
}

func (f *fakeStore) spend(ctx context.Context) {
	d := shop.Delay(ctx)
	f.delays = append(f.delays, d)
	if f.elapse != nil {
		f.elapse(d)
	}
}

func (f *fakeStore) Customer(_ context.Context, username string) (int, error) {
	if f.customers == nil {
		f.customers = map[string]int{}
	}
	if _, ok := f.customers[username]; !ok {
		f.customers[username] = 1000 + len(f.customers)
	}
	return f.customers[username], nil
}

func (f *fakeStore) Orders(ctx context.Context, _, _ int) ([]shop.Order, error) {
	f.spend(ctx)
	if f.fail != nil {
		return nil, f.fail
	}
	return []shop.Order{{ID: 7, Product: "Thermometer", Quantity: 1, Total: 12.5}}, nil
}

func (f *fakeStore) PlaceOrder(ctx context.Context, _ int, product, quantity int) (shop.Order, error) {
	f.spend(ctx)
	if f.fail != nil {
		return shop.Order{}, f.fail
	}
	if product != 1 {
		return shop.Order{}, fmt.Errorf("%w: product %d does not exist", shop.ErrInvalid, product)
	}
	return shop.Order{ID: 8, Product: "Thermometer", Quantity: quantity, Total: 12.5 * float64(quantity)}, nil
}

// recorder checks every request's rows against the logging schema, as the
// SQL writer does, and keeps them.
type recorder struct {
	schema oslog.Schema
	rows   []oslog.Row
	fail   error
}

func (r *recorder) Write(_ context.Context, rows []oslog.Row) error {
	if err := r.schema.Check(rows); err != nil {
		return err
	}
	r.rows = append(r.rows, rows...)
	return r.fail
}

func (r *recorder) byType() map[string][]map[string]any {
	m := map[string][]map[string]any{}
	for _, row := range r.rows {
		m[row.Type] = append(m[row.Type], row.Columns)
	}
	return m
}

// clock returns start, then start+250ms, ...
func clock(start time.Time) func() time.Time {
	n := 0
	return func() time.Time {
		t := start.Add(time.Duration(n) * 250 * time.Millisecond)
		n++
		return t
	}
}

var t0 = time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)

func newServer(t *testing.T, store *fakeStore) (*Server, *recorder) {
	t.Helper()
	types, err := logschema.Load(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	schema := oslog.NewSchema(types)
	rec := &recorder{schema: schema}
	return &Server{Store: store, Logs: rec, Schema: schema, FrontEnd: "FE-PZ-1", Now: clock(t0),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, rec
}

func do(t *testing.T, s *Server, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "10.244.0.9:51000"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

var maria = map[string]string{
	HeaderUser:        "maria.santos",
	HeaderSession:     "u7k2v4zz5qd3m9n1p8r6s0t2",
	"X-Forwarded-For": "10.20.30.41, 10.0.0.1",
}

// sameRequest checks that every row has one Request_Key and that the rows
// whose view has the columns carry the caller's session, user and IP.
func sameRequest(t *testing.T, rec *recorder, wantTypes string) {
	t.Helper()
	var types []string
	var key any
	for _, row := range rec.rows {
		types = append(types, row.Type)
		k := row.Columns["Request_Key"]
		if k == nil || k == "" {
			t.Fatalf("%s row has no Request_Key: %v", row.Type, row.Columns)
		}
		if key == nil {
			key = k
		} else if k != key {
			t.Fatalf("%s row has Request_Key %v, want %v", row.Type, k, key)
		}
		lt := rec.schema[row.Type]
		for col, want := range map[string]any{
			"Session_Id": maria[HeaderSession], "Login_Id": maria[HeaderSession],
			"Username": "maria.santos", "User_Id": 1000,
			"Client_IP": "10.20.30.41", "Source": "10.20.30.41",
		} {
			if !lt.HasColumn(col) {
				continue
			}
			if got := row.Columns[col]; got != want {
				t.Errorf("%s.%s = %v, want %v", row.Type, col, got, want)
			}
		}
		for _, col := range []string{"Executed_By", "Server"} {
			if lt.HasColumn(col) && row.Columns[col] != "FE-PZ-1" {
				t.Errorf("%s.%s = %v, want FE-PZ-1", row.Type, col, row.Columns[col])
			}
		}
		if row.Columns["Application_Name"] != Application {
			t.Errorf("%s.Application_Name = %v", row.Type, row.Columns["Application_Name"])
		}
	}
	if got := strings.Join(types, " "); got != wantTypes {
		t.Fatalf("log rows %q, want %q", got, wantTypes)
	}
}

func TestTraditionalScreenWritesAScreenRow(t *testing.T) {
	s, rec := newServer(t, &fakeStore{})
	w := do(t, s, http.MethodGet, PathOrderList, "", maria)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Thermometer") {
		t.Fatalf("status %d, body %s", w.Code, w.Body)
	}
	sameRequest(t, rec, "screen")
	row := rec.byType()["screen"][0]
	if row["Instant"] != t0 {
		t.Errorf("Instant %v: the screen log's Instant is the start", row["Instant"])
	}
	if row["Duration"] != int64(250) || row["Screen"] != "OrderList" || row["Espace_Name"] != "PatientZero_Web" {
		t.Errorf("row %v", row)
	}
}

// The HTML screen cannot be sniffed or framed and loads nothing, on its page
// and its error alike (the shed 503 is checked in
// TestLimiterShedsExcessWith503). The page needs nothing that policy blocks:
// no script, style, event handler, external resource, frame or form.
func TestScreenSendsSecurityHeaders(t *testing.T) {
	for _, c := range []struct {
		name   string
		store  *fakeStore
		status int
	}{
		{"page", &fakeStore{}, http.StatusOK},
		{"error", &fakeStore{fail: errors.New("Execution Timeout Expired")}, http.StatusInternalServerError},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := newServer(t, c.store)
			w := do(t, s, http.MethodGet, PathOrderList, "", maria)
			if w.Code != c.status {
				t.Fatalf("status %d, want %d: %s", w.Code, c.status, w.Body)
			}
			checkScreenHeaders(t, w)
			if c.status != http.StatusOK {
				return
			}
			if blocked := blockedMarkup.FindString(strings.ToLower(w.Body.String())); blocked != "" {
				t.Errorf("the page has %q, which its Content-Security-Policy blocks:\n%s", blocked, w.Body)
			}
		})
	}
}

// blockedMarkup is markup that default-src 'none' blocks or that loads
// something: elements and attributes, not text.
var blockedMarkup = regexp.MustCompile(`<(script|style|link|img|iframe|frame|object|embed|form|base|svg|video|audio|source)\b|\s(on[a-z]+|style|src|srcset|href|action)\s*=`)

// checkScreenHeaders checks the HTML screen's security headers, each sent
// once.
func checkScreenHeaders(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	for name, want := range map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
		"X-Frame-Options":         "DENY",
	} {
		if got := w.Header().Values(name); len(got) != 1 || got[0] != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
}

func TestReactiveServerActionWritesAMobileRequestRow(t *testing.T) {
	s, rec := newServer(t, &fakeStore{})
	w := do(t, s, http.MethodPost, PathPlaceOrder, `{"productId":1,"quantity":2}`, maria)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"quantity":2`) {
		t.Fatalf("status %d, body %s", w.Code, w.Body)
	}
	sameRequest(t, rec, "mobile_request")
	row := rec.byType()["mobile_request"][0]
	if row["Instant"] != t0.Add(250*time.Millisecond) {
		t.Errorf("Instant %v: the mobile request log's Instant is the end", row["Instant"])
	}
	if row["Error_Id"] != "" || row["Endpoint"] != "PlaceOrder" || row["Screen"] != "Checkout" || row["Id"] == "" {
		t.Errorf("row %v", row)
	}
}

func TestExposedRESTAPIWritesIntegrationAndGeneralRows(t *testing.T) {
	s, rec := newServer(t, &fakeStore{})
	w := do(t, s, http.MethodGet, PathAPIOrders, "", maria)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Thermometer") {
		t.Fatalf("status %d, body %s", w.Code, w.Body)
	}
	sameRequest(t, rec, "general integration")
	row := rec.byType()["integration"][0]
	if row["Is_Expose"] != true || row["Type"] != "REST" || row["Action"] != "GetOrders" || row["Endpoint"] != "" || row["Error_Id"] != "" || row["Id"] == "" {
		t.Errorf("row %v", row)
	}
}

func TestFailedRequestsWriteALinkedErrorRow(t *testing.T) {
	broken := errors.New("Execution Timeout Expired")
	for _, c := range []struct {
		name, method, path, body string
		status                   int
		types                    string
	}{
		{"screen", http.MethodGet, PathOrderList, "", http.StatusInternalServerError, "screen error"},
		{"invalid order", http.MethodPost, PathPlaceOrder, `{"productId":99,"quantity":1}`, http.StatusBadRequest, "mobile_request error"},
		{"bad body", http.MethodPost, PathPlaceOrder, `{`, http.StatusBadRequest, "mobile_request error"},
		{"api", http.MethodGet, PathAPIOrders, "", http.StatusInternalServerError, "general integration error"},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := &fakeStore{}
			if c.name == "screen" || c.name == "api" {
				store.fail = broken
			}
			s, rec := newServer(t, store)
			if w := do(t, s, c.method, c.path, c.body, maria); w.Code != c.status {
				t.Fatalf("status %d, want %d: %s", w.Code, c.status, w.Body)
			}
			sameRequest(t, rec, c.types)
			errRow := rec.byType()["error"][0]
			if errRow["Message"] == "" || errRow["Entrypoint_Name"] == "" {
				t.Errorf("error row %v", errRow)
			}
			// A database failure's trace is longer than the error cycle
			// table keeps, so it goes to the detail table; a refused order's
			// is not.
			detail := s.Schema["error"].Detail
			if long := len(errRow["Stack"].(string)) > detail.Limit; long != (store.fail != nil) {
				t.Errorf("Stack of %d characters with the store failing %v:\n%s", len(errRow["Stack"].(string)), store.fail != nil, errRow["Stack"])
			}
			for _, typ := range []string{"mobile_request", "integration", "general"} {
				for _, row := range rec.byType()[typ] {
					if ref := row["Error_Id"]; ref != errRow["Id"] {
						t.Errorf("%s.Error_Id %v, want the error row's Id %v", typ, ref, errRow["Id"])
					}
				}
			}
		})
	}
}

func TestAnonymousCallerGetsASessionAndUserZero(t *testing.T) {
	s, rec := newServer(t, &fakeStore{})
	do(t, s, http.MethodGet, PathOrderList, "", nil)
	row := rec.byType()["screen"][0]
	if row["User_Id"] != 0 || row["Username"] != nil {
		t.Errorf("user %v %v, want 0 and none", row["User_Id"], row["Username"])
	}
	if session, _ := row["Session_Id"].(string); len(session) != 24 {
		t.Errorf("session %q, want a generated 24-character id", session)
	}
	if row["Client_IP"] != "10.244.0.9" {
		t.Errorf("client IP %v, want the peer address", row["Client_IP"])
	}
}

func TestRequestKeysAreDistinctGUIDs(t *testing.T) {
	s, rec := newServer(t, &fakeStore{})
	do(t, s, http.MethodGet, PathOrderList, "", maria)
	do(t, s, http.MethodGet, PathOrderList, "", maria)
	a, b := rec.rows[0].Columns["Request_Key"].(string), rec.rows[1].Columns["Request_Key"].(string)
	if a == b || len(a) != 36 || a[14] != '4' {
		t.Fatalf("request keys %q and %q", a, b)
	}
}

func TestALogWriteFailureDoesNotFailTheRequest(t *testing.T) {
	s, rec := newServer(t, &fakeStore{})
	rec.fail = errors.New("log database down")
	if w := do(t, s, http.MethodGet, PathAPIOrders, "", maria); w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
}

func TestReadyz(t *testing.T) {
	s, _ := newServer(t, &fakeStore{})
	if w := do(t, s, http.MethodGet, "/healthz", "", nil); w.Code != http.StatusOK {
		t.Fatalf("healthz %d", w.Code)
	}
	s.Ready = func(context.Context) error { return errors.New("not yet") }
	if w := do(t, s, http.MethodGet, "/readyz", "", nil); w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "not yet") {
		t.Fatalf("readyz %d %s", w.Code, w.Body)
	}
	s.Ready = func(context.Context) error { return nil }
	if w := do(t, s, http.MethodGet, "/readyz", "", nil); w.Code != http.StatusOK {
		t.Fatalf("readyz %d", w.Code)
	}
}
