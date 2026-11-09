package traffic

import (
	"fmt"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)

func TestCountsPerIntervalAndRun(t *testing.T) {
	var c Counter
	c.Observe(Request{EntryPoint: "OrderList", User: "before"}) // no run yet: ignored
	c.Begin(t0)
	c.Observe(Request{EntryPoint: "OrderList", User: "ana", Session: "s1", Duration: 120 * time.Millisecond})
	c.Observe(Request{EntryPoint: "OrderList", User: "ana", Session: "s1", Duration: 80 * time.Millisecond})
	c.Observe(Request{EntryPoint: "PlaceOrder", User: "rui", Session: "s2", Duration: time.Second, Failed: true, Injected: true})
	first := c.Flush(t0.Add(15 * time.Second))
	c.Observe(Request{EntryPoint: "GetOrders", Session: "api-1", Duration: 30 * time.Millisecond})
	c.Observe(Request{EntryPoint: "OrderList", User: "rui", Session: "s3", Failed: true})
	last, run := c.End(t0.Add(20 * time.Second))
	c.Observe(Request{EntryPoint: "OrderList", User: "after"}) // run ended: ignored

	if first.From != t0 || first.To != t0.Add(15*time.Second) {
		t.Errorf("first interval %s to %s", first.From, first.To)
	}
	if first.Requests != 3 || first.Failed != 1 || first.Injected != 1 || first.Users != 2 || first.Sessions != 2 {
		t.Errorf("first interval: %+v", first)
	}
	ol := first.EntryPoints["OrderList"]
	if ol.Requests != 2 || ol.DurationMsSum != 200 || ol.DurationMsMax != 120 {
		t.Errorf("first interval OrderList: %+v", ol)
	}
	if last.Requests != 2 || last.Failed != 1 || last.Injected != 0 || last.Users != 1 || last.Sessions != 2 {
		t.Errorf("last interval: %+v", last)
	}
	if last.EntryPoints["GetOrders"].AnonymousCalls != 1 {
		t.Errorf("anonymous API call not counted: %+v", last.EntryPoints["GetOrders"])
	}
	// The run counts distinct users and sessions over both intervals.
	if run.Requests != 5 || run.Failed != 2 || run.Users != 2 || run.Sessions != 4 || run.From != t0 {
		t.Errorf("run: %+v", run)
	}
}

func TestDistinctSetsAreCapped(t *testing.T) {
	var c Counter
	c.Begin(t0)
	for i := 0; i < MaxDistinctPerInterval+10; i++ {
		c.Observe(Request{EntryPoint: "OrderList", User: fmt.Sprint("u", i), Session: fmt.Sprint("s", i)})
	}
	s := c.Flush(t0.Add(time.Minute))
	if s.Users != MaxDistinctPerInterval || !s.UsersCapped || !s.SessionsCapped {
		t.Fatalf("users %d capped %v/%v, want the cap and the flags", s.Users, s.UsersCapped, s.SessionsCapped)
	}
	if s.Requests != MaxDistinctPerInterval+10 {
		t.Fatalf("requests %d: the cap must not stop counting requests", s.Requests)
	}
}

// A shed request is counted apart from the requests served: it wrote no log
// rows, so it is not one of the requests Atrium reports.
func TestShedIsCountedApart(t *testing.T) {
	var c Counter
	c.Shed("GetOrders") // no run yet: ignored
	c.Begin(t0)
	c.Observe(Request{EntryPoint: "GetOrders", Session: "api-1"})
	c.Shed("GetOrders")
	c.Shed("PlaceOrder")
	last, run := c.End(t0.Add(15 * time.Second))
	c.Shed("GetOrders") // run ended: ignored

	for name, s := range map[string]Summary{"interval": last, "run": run} {
		get, place := s.EntryPoints["GetOrders"], s.EntryPoints["PlaceOrder"]
		if s.Requests != 1 || s.Shed != 2 || get.Requests != 1 || get.Shed != 1 || place.Requests != 0 || place.Shed != 1 {
			t.Errorf("%s: %+v", name, s)
		}
	}
}

func TestNilCounter(t *testing.T) {
	var c *Counter
	c.Observe(Request{EntryPoint: "OrderList"})
	c.Shed("OrderList")
}
