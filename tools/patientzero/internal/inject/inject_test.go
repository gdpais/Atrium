package inject

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"atrium/tools/patientzero/internal/scenario"
)

var modules = map[string][]string{
	"PatientZero_Web": {"OrderList"},
	"PatientZero_App": {"PlaceOrder"},
	"PatientZero_API": {"GetOrders"},
}

// setup returns an injector whose random numbers come from rnd and whose
// sleeps are recorded instead of waited.
func setup(rnd ...float64) (*Injector, *scenario.Registry, *[]time.Duration) {
	var slept []time.Duration
	i := 0
	inj := &Injector{
		Float: func() float64 {
			v := rnd[i%len(rnd)]
			i++
			return v
		},
		Sleep: func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		},
	}
	reg := scenario.NewRegistry()
	Register(reg, inj, modules)
	return inj, reg, &slept
}

func start(t *testing.T, reg *scenario.Registry, typ string, params map[string]any) scenario.Fault {
	t.Helper()
	f, err := reg.New(scenario.FaultSpec{Type: typ, Params: params})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestLatencyDelaysOnlyItsEntryPoints(t *testing.T) {
	inj, reg, slept := setup(0.5)
	f := start(t, reg, "latency", map[string]any{"entry_points": []any{"OrderList"}, "delay": "2s", "jitter": "1s"})
	if err := inj.Apply(context.Background(), "PlaceOrder"); err != nil || len(*slept) != 0 {
		t.Fatalf("PlaceOrder: err %v, slept %v; want no delay", err, *slept)
	}
	if err := inj.Apply(context.Background(), "OrderList"); err != nil {
		t.Fatal(err)
	}
	// 2s plus half the 1s jitter.
	if len(*slept) != 1 || (*slept)[0] != 2500*time.Millisecond {
		t.Fatalf("slept %v, want [2.5s]", *slept)
	}
	if err := f.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if inj.Active() != 0 {
		t.Fatal("Stop left the fault active")
	}
	_ = inj.Apply(context.Background(), "OrderList")
	if len(*slept) != 1 {
		t.Fatalf("a stopped fault still delays: %v", *slept)
	}
}

func TestErrorsFailTheirShare(t *testing.T) {
	// The ratio is compared with each request's random number: 0.1 < 0.5
	// fails, 0.9 does not.
	inj, reg, _ := setup(0.1, 0.9)
	start(t, reg, "errors", map[string]any{"ratio": 0.5, "message": "checkout down"})
	err := inj.Apply(context.Background(), "PlaceOrder")
	if !errors.Is(err, ErrInjected) || err.Error() != "injected by scenario: checkout down" {
		t.Fatalf("first request: %v, want the injected failure", err)
	}
	if err := inj.Apply(context.Background(), "GetOrders"); err != nil {
		t.Fatalf("second request: %v, want none (no entry_points means all, but 0.9 >= 0.5)", err)
	}
}

func TestNilInjectorInjectsNothing(t *testing.T) {
	var inj *Injector
	if err := inj.Apply(context.Background(), "OrderList"); err != nil || inj.Active() != 0 {
		t.Fatal(err)
	}
}

func TestLatencyEndsWithTheRequest(t *testing.T) {
	inj := &Injector{}
	reg := scenario.NewRegistry()
	Register(reg, inj, modules)
	start(t, reg, "latency", map[string]any{"delay": "1m"})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	began := time.Now()
	if err := inj.Apply(ctx, "OrderList"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the request's deadline", err)
	}
	if time.Since(began) > 5*time.Second {
		t.Fatal("the delay outlived the request")
	}
}

func TestParamsAreValidated(t *testing.T) {
	_, reg, _ := setup(0)
	cases := []struct {
		typ    string
		params map[string]any
		want   string
	}{
		{"latency", map[string]any{}, "delay is required"},
		{"latency", map[string]any{"delay": "fast"}, "positive duration"},
		{"latency", map[string]any{"delay": "-1s"}, "positive duration"},
		{"latency", map[string]any{"delay": "50s", "jitter": "20s"}, "at most 1m0s"},
		{"latency", map[string]any{"delay": "1s", "ratio": 0}, "ratio 0 must be"},
		{"latency", map[string]any{"delay": "1s", "entry_points": []any{"Home"}}, "entry point \"Home\""},
		{"latency", map[string]any{"delay": "1s", "speed": 2}, "unknown field \"speed\""},
		{"errors", map[string]any{}, "ratio is required"},
		{"errors", map[string]any{"ratio": 1.5}, "ratio 1.5 must be"},
		{"errors", map[string]any{"ratio": 1, "message": strings.Repeat("x", MaxMessageLen+1)}, "message must be at most"},
	}
	for _, c := range cases {
		_, err := reg.New(scenario.FaultSpec{Type: c.typ, Params: c.params})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %v: error %v, want %q", c.typ, c.params, err, c.want)
		}
	}
}

// A deploy publishes a new version of one module: its entry points fail
// with the new version's exception, the others are untouched, and the
// rollback when it stops runs the previous version again.
func TestDeployFailsItsModuleAndVersionsIt(t *testing.T) {
	inj, reg, slept := setup(0.1)
	if v := inj.Version("PatientZero_App"); v != FirstVersion {
		t.Fatalf("version before any deploy %d, want %d", v, FirstVersion)
	}
	f := start(t, reg, "deploy", map[string]any{"module": "PatientZero_App", "error_ratio": 1, "exception": "Invalid column name 'DiscountCode'."})
	err := inj.Apply(context.Background(), "PlaceOrder")
	var failure *Failure
	if !errors.As(err, &failure) || !errors.Is(err, ErrInjected) || err.Error() != "Invalid column name 'DiscountCode'." {
		t.Fatalf("PlaceOrder: %v, want the new version's exception, injected", err)
	}
	if err := inj.Apply(context.Background(), "OrderList"); err != nil {
		t.Fatalf("OrderList is in another module: %v", err)
	}
	if len(*slept) != 0 {
		t.Fatalf("a deploy without a delay slept %v", *slept)
	}
	if v, other := inj.Version("PatientZero_App"), inj.Version("PatientZero_Web"); v != FirstVersion+1 || other != FirstVersion {
		t.Fatalf("versions App %d, Web %d; want %d and %d", v, other, FirstVersion+1, FirstVersion)
	}
	if err := f.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := inj.Version("PatientZero_App"); v != FirstVersion {
		t.Fatalf("version after the rollback %d, want %d", v, FirstVersion)
	}
	if err := inj.Apply(context.Background(), "PlaceOrder"); err != nil {
		t.Fatalf("PlaceOrder after the rollback: %v", err)
	}
	// The next publish gets a new number; its rollback goes back again.
	f = start(t, reg, "deploy", map[string]any{"module": "PatientZero_App", "delay": "2s"})
	if v := inj.Version("PatientZero_App"); v != FirstVersion+2 {
		t.Fatalf("second deploy runs version %d, want %d", v, FirstVersion+2)
	}
	if err := inj.Apply(context.Background(), "PlaceOrder"); err != nil || len(*slept) != 1 || (*slept)[0] != 2*time.Second {
		t.Fatalf("a slow deploy: err %v, slept %v; want no failure and 2s", err, *slept)
	}
	_ = f.Stop(context.Background())
	if v := inj.Version("PatientZero_App"); v != FirstVersion {
		t.Fatalf("version after the second rollback %d, want %d", v, FirstVersion)
	}
}

func TestDeployFailsItsShareWithTheDefaultException(t *testing.T) {
	// 0.1 < 0.5 fails, 0.9 does not.
	inj, reg, _ := setup(0.1, 0.9)
	start(t, reg, "deploy", map[string]any{"module": "PatientZero_API", "error_ratio": 0.5})
	if err := inj.Apply(context.Background(), "GetOrders"); err == nil || err.Error() != DefaultException {
		t.Fatalf("first call: %v, want %q", err, DefaultException)
	}
	if err := inj.Apply(context.Background(), "GetOrders"); err != nil {
		t.Fatalf("second call: %v, want none", err)
	}
}

// SQL latency is not spent in PatientZero: Apply does not wait for it, and
// SQLDelay hands it to the Store, which spends it in SQL Server.
func TestSQLLatencyIsHandedToTheStore(t *testing.T) {
	inj, reg, slept := setup(0.5)
	f := start(t, reg, "sql-latency", map[string]any{"entry_points": []any{"OrderList", "GetOrders"}, "delay": "3s", "jitter": "1s"})
	if err := inj.Apply(context.Background(), "OrderList"); err != nil || len(*slept) != 0 {
		t.Fatalf("Apply: err %v, slept %v; want neither", err, *slept)
	}
	if d := inj.SQLDelay("OrderList"); d != 3500*time.Millisecond {
		t.Fatalf("OrderList SQL delay %v, want 3.5s (3s plus half the jitter)", d)
	}
	if d := inj.SQLDelay("PlaceOrder"); d != 0 {
		t.Fatalf("PlaceOrder SQL delay %v, want none", d)
	}
	_ = f.Stop(context.Background())
	if d := inj.SQLDelay("GetOrders"); d != 0 {
		t.Fatalf("a stopped fault still slows the SQL: %v", d)
	}
	var none *Injector
	if none.SQLDelay("OrderList") != 0 || none.Version("PatientZero_Web") != FirstVersion {
		t.Fatal("a nil injector must inject nothing")
	}
}

func TestSQLLatencySlowsItsShare(t *testing.T) {
	// The ratio is drawn first: 0.6 >= 0.5 is not slowed, 0.2 is (then 0.2
	// of no jitter).
	inj, reg, _ := setup(0.6, 0.2)
	start(t, reg, "sql-latency", map[string]any{"delay": "2s", "ratio": 0.5})
	if d := inj.SQLDelay("GetOrders"); d != 0 {
		t.Fatalf("first request slowed by %v, want not at all", d)
	}
	if d := inj.SQLDelay("GetOrders"); d != 2*time.Second {
		t.Fatalf("second request slowed by %v, want 2s", d)
	}
}

func TestNewFaultParamsAreValidated(t *testing.T) {
	_, reg, _ := setup(0)
	cases := []struct {
		typ    string
		params map[string]any
		want   string
	}{
		{"deploy", map[string]any{"error_ratio": 1}, "module \"\" is not one of PatientZero_API, PatientZero_App, PatientZero_Web"},
		{"deploy", map[string]any{"module": "Shop_CS", "error_ratio": 1}, "module \"Shop_CS\""},
		{"deploy", map[string]any{"module": "PatientZero_App"}, "give error_ratio, delay or both"},
		{"deploy", map[string]any{"module": "PatientZero_App", "error_ratio": 2}, "error_ratio 2 must be"},
		{"deploy", map[string]any{"module": "PatientZero_App", "delay": "2m"}, "delay must be at most 1m0s"},
		{"deploy", map[string]any{"module": "PatientZero_App", "error_ratio": 1, "exception": strings.Repeat("x", MaxMessageLen+1)}, "exception must be at most"},
		{"deploy", map[string]any{"module": "PatientZero_App", "error_ratio": 1, "entry_points": []any{"PlaceOrder"}}, "unknown field \"entry_points\""},
		{"sql-latency", map[string]any{}, "delay is required"},
		{"sql-latency", map[string]any{"delay": "50s", "jitter": "20s"}, "at most 1m0s"},
		{"sql-latency", map[string]any{"delay": "1s", "entry_points": []any{"Home"}}, "entry point \"Home\""},
	}
	for _, c := range cases {
		_, err := reg.New(scenario.FaultSpec{Type: c.typ, Params: c.params})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %v: error %v, want %q", c.typ, c.params, err, c.want)
		}
	}
}

type fakeRotator struct{ rotations int }

func (f *fakeRotator) Rotate(context.Context) (map[string]int, error) {
	f.rotations++
	return map[string]int{"screen": 2}, nil
}

// A rotation happens when its step starts and is not undone when it ends.
func TestRotationRotatesOnceAndStays(t *testing.T) {
	inj, reg, _ := setup(0)
	f, err := reg.New(scenario.FaultSpec{Type: "rotation"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "no logging database") {
		t.Fatalf("Start without a rotator: %v", err)
	}
	rot := &fakeRotator{}
	inj.Rotator = rot
	if err := f.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.Stop(context.Background()); err != nil || rot.rotations != 1 || inj.Active() != 0 {
		t.Fatalf("after Stop: %v, %d rotations, %d active; want one rotation and nothing left on", err, rot.rotations, inj.Active())
	}
	if _, err := reg.New(scenario.FaultSpec{Type: "rotation", Params: map[string]any{"week": 2}}); err == nil {
		t.Error("rotation takes no parameters")
	}
}

func TestCommitDelayHoldsEveryRequestsRows(t *testing.T) {
	inj, reg, _ := setup(0)
	if inj.CommitDelay() != 0 {
		t.Fatal("no fault, no delay")
	}
	short := start(t, reg, "commit-delay", map[string]any{"delay": "10s"})
	long := start(t, reg, "commit-delay", map[string]any{"delay": "90s"})
	if d := inj.CommitDelay(); d != 90*time.Second {
		t.Fatalf("delay %s, want the longest, 90s: one log service queue", d)
	}
	_ = long.Stop(context.Background())
	if d := inj.CommitDelay(); d != 10*time.Second {
		t.Fatalf("delay %s after the long one stopped, want 10s", d)
	}
	_ = short.Stop(context.Background())
	var none *Injector
	if inj.CommitDelay() != 0 || none.CommitDelay() != 0 {
		t.Fatal("stopped faults still delay commits")
	}
}

func TestLogFloodAddsRowsToItsEntryPoints(t *testing.T) {
	inj, reg, _ := setup(0)
	start(t, reg, "log-flood", map[string]any{"entry_points": []any{"OrderList"}, "rows": 20, "bytes": 1500})
	start(t, reg, "log-flood", map[string]any{"rows": 5})
	if n, size := inj.Flood("OrderList"); n != 25 || size != 1500 {
		t.Fatalf("OrderList: %d rows of %d bytes, want 25 of 1500", n, size)
	}
	if n, size := inj.Flood("GetOrders"); n != 5 || size != DefaultFloodBytes {
		t.Fatalf("GetOrders: %d rows of %d bytes, want 5 of %d", n, size, DefaultFloodBytes)
	}
	var none *Injector
	if n, _ := none.Flood("OrderList"); n != 0 {
		t.Fatal("a nil injector floods")
	}
}

func TestLogFaultParamsAreValidated(t *testing.T) {
	_, reg, _ := setup(0)
	for _, c := range []struct {
		typ    string
		params map[string]any
		want   string
	}{
		{"commit-delay", map[string]any{}, "delay is required"},
		{"commit-delay", map[string]any{"delay": "11m"}, "delay must be at most 10m0s"},
		{"log-flood", map[string]any{}, "rows must be 1 to 200"},
		{"log-flood", map[string]any{"rows": 201}, "rows must be 1 to 200"},
		{"log-flood", map[string]any{"rows": 1, "bytes": 2001}, "bytes must be 1 to 2000"},
		{"log-flood", map[string]any{"rows": 1, "entry_points": []any{"Home"}}, "entry point \"Home\""},
	} {
		_, err := reg.New(scenario.FaultSpec{Type: c.typ, Params: c.params})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %v: error %v, want %q", c.typ, c.params, err, c.want)
		}
	}
}
