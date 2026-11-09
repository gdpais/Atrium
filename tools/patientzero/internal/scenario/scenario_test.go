package scenario

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

type nopFault struct{}

func (nopFault) Start(context.Context) error { return nil }
func (nopFault) Stop(context.Context) error  { return nil }

// testRegistry has an in-process type with one required parameter and a
// cluster type.
func testRegistry() *Registry {
	reg := NewRegistry()
	reg.Register(FaultType{Name: "slow", Scope: ScopeInProcess, New: func(p map[string]any) (Fault, error) {
		var v struct {
			Delay string `json:"delay"`
		}
		if err := DecodeParams(p, &v); err != nil {
			return nil, err
		}
		if v.Delay == "" {
			return nil, errors.New("delay is required")
		}
		return nopFault{}, nil
	}})
	reg.Register(FaultType{Name: "restart", Scope: ScopeCluster, New: func(map[string]any) (Fault, error) { return nopFault{}, nil }})
	return reg
}

const valid = `
version: 1
name: demo
description: A demo.
steps:
  - name: warm
    duration: 30s
  - name: hurt
    duration: 2m
    fault:
      type: slow
      params: {delay: 1s}
signals:
  - name: rises
    promql: sum(up)
    condition: "> 0"
    within: 3m
  - name: falls
    logql: sum(count_over_time({a="b"}[1m]))
    condition: "== 0"
    from: end
    within: 4m
    mode: always
  - name: pages
    alert: SomethingBad
    labels: {severity: critical}
    state: firing
    step: warm
    within: 1m
`

func TestParseFillsDefaults(t *testing.T) {
	s, err := Parse([]byte(valid), testRegistry())
	if err != nil {
		t.Fatal(err)
	}
	if s.Load != "steady" {
		t.Errorf("load = %q, want the steady default", s.Load)
	}
	rises := s.Signals[0]
	if rises.Step != "hurt" || rises.From != FromStart || rises.Mode != ModeEventually {
		t.Errorf("defaults: step %q from %q mode %q; want hurt (the first faulted step), start, eventually", rises.Step, rises.From, rises.Mode)
	}
	if got := s.Anchor(rises); got != 30*time.Second {
		t.Errorf("anchor of rises = %s, want 30s", got)
	}
	if got := s.Anchor(s.Signals[1]); got != 150*time.Second {
		t.Errorf("anchor of falls (end of hurt) = %s, want 2m30s", got)
	}
	if got := s.Anchor(s.Signals[2]); got != 0 {
		t.Errorf("anchor of pages (start of warm) = %s, want 0", got)
	}
	if got := s.StepsDuration(); got != 150*time.Second {
		t.Errorf("steps take %s, want 2m30s", got)
	}
	// falls closes last: 2m30s + 4m.
	if got := s.MaxDuration(); got != 390*time.Second {
		t.Errorf("max duration = %s, want 6m30s", got)
	}
	if got := s.FaultTypes(); len(got) != 1 || got[0] != "slow" {
		t.Errorf("fault types = %v", got)
	}
	if s.Signals[0].Kind() != KindPromQL || s.Signals[1].Kind() != KindLogQL || s.Signals[2].Kind() != KindAlert {
		t.Error("kinds not read from the source field")
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		name, from, to, want string
	}{
		{"unknown field", "version: 1\n", "version: 1\ncolour: red\n", "field colour not found"},
		{"wrong version", "version: 1", "version: 2", "version must be 1"},
		{"bad name", "name: demo", "name: Demo_1", "name \"Demo_1\""},
		{"no description", "description: A demo.", "description: ' '", "description is required"},
		{"bad load", "description: A demo.", "description: A demo.\nload: heavy", "load must be one of"},
		{"unknown fault type", "type: slow", "type: melt", "type \"melt\" is not a registered fault type"},
		{"bad fault params", "params: {delay: 1s}", "params: {delay: 1s, extra: 2}", "unknown field \"extra\""},
		{"missing fault param", "params: {delay: 1s}", "params: {}", "delay is required"},
		{"numeric duration", "duration: 30s", "duration: 30", "a duration is a string"},
		{"bad duration", "duration: 30s", "duration: soon", "not a duration"},
		{"zero duration", "duration: 30s", "duration: 0s", "duration must be more than 0"},
		{"duplicate step", "name: warm", "name: hurt", "step name \"hurt\" is used twice"},
		{"two sources", "promql: sum(up)", "promql: sum(up)\n    logql: '{a=\"b\"}'", "exactly one of promql, logql, alert or delivery"},
		{"no condition", "    condition: \"> 0\"\n    within: 3m", "    within: 3m", "condition \"\" must be"},
		{"bad condition", `condition: "> 0"`, `condition: "about 3"`, "condition \"about 3\""},
		{"alert without state", "    state: firing\n", "", "needs state firing or resolved"},
		{"alert with condition", "state: firing", "state: firing\n    condition: \"> 1\"", "has a state, not a condition"},
		{"unknown step", "step: warm", "step: cool", "step \"cool\" is not a step"},
		{"bad from", "from: end", "from: middle", "from must be start or end"},
		{"bad mode", "mode: always", "mode: sometimes", "mode must be eventually or always"},
		{"no window", "within: 1m\n", "within: 0s\n", "within must be more than 0"},
		{"two documents", "within: 1m\n", "within: 1m\n---\nname: other\n", "one YAML document"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(valid, c.from) {
				t.Fatalf("test case: %q not in the valid scenario", c.from)
			}
			doc := strings.Replace(valid, c.from, c.to, 1)
			_, err := Parse([]byte(doc), testRegistry())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want one containing %q", err, c.want)
			}
		})
	}
}

func TestParseEmpty(t *testing.T) {
	if _, err := Parse(nil, testRegistry()); err == nil {
		t.Fatal("an empty file parsed")
	}
}

func TestLoadDirNamesFilesAfterScenarios(t *testing.T) {
	fsys := fstest.MapFS{
		"demo.yaml":  {Data: []byte(valid)},
		"notes.txt":  {Data: []byte("not a scenario")},
		"other.yaml": {Data: []byte(strings.Replace(valid, "name: demo", "name: another", 1))},
	}
	_, err := LoadDir(fsys, ".", testRegistry())
	if err == nil || !strings.Contains(err.Error(), "other.yaml: name is \"another\"") {
		t.Fatalf("error = %v, want the file/name mismatch", err)
	}
	delete(fsys, "other.yaml")
	list, err := LoadDir(fsys, ".", testRegistry())
	if err != nil || len(list) != 1 || list[0].Name != "demo" {
		t.Fatalf("LoadDir = %v, %v; want demo only", list, err)
	}
}

func TestCondition(t *testing.T) {
	cases := []struct {
		cond   string
		values []float64
		want   bool
	}{
		{"> 0.25", []float64{0.3}, true},
		{"> 0.25", []float64{0.25}, false},
		{">=0.25", []float64{0.25}, true},
		{"< 1", []float64{0.5, 0.9}, true},
		{"< 1", []float64{0.5, 1.5}, false}, // every series must hold
		{"<= 1", []float64{1}, true},
		{"== 0", []float64{0}, true},
		{"!= 0", []float64{0}, false},
		{"> 0", nil, false}, // no data never satisfies a comparison
		{"present", []float64{0}, true},
		{"present", nil, false},
		{"absent", nil, true},
		{"absent", []float64{1}, false},
		{" > -1e3 ", []float64{0}, true},
	}
	for _, c := range cases {
		cond, err := ParseCondition(c.cond)
		if err != nil {
			t.Fatalf("%q: %v", c.cond, err)
		}
		if got := cond.Holds(c.values); got != c.want {
			t.Errorf("%q holds for %v = %v, want %v", c.cond, c.values, got, c.want)
		}
	}
	for _, bad := range []string{"", "> ", "=> 1", "~ 1", "> one"} {
		if _, err := ParseCondition(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestRegistry(t *testing.T) {
	reg := testRegistry()
	if got := reg.Types(); len(got) != 2 || got[0].Name != "restart" || got[1].Name != "slow" {
		t.Fatalf("types = %v", got)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("registering a type twice did not panic")
		}
	}()
	reg.Register(FaultType{Name: "slow", Scope: ScopeInProcess, New: func(map[string]any) (Fault, error) { return nopFault{}, nil }})
}

const withDelivery = `
version: 1
name: demo
description: A demo.
steps:
  - name: warm
    duration: 30s
  - name: hurt
    duration: 2m
    fault:
      type: slow
      params: {delay: 1s}
  - name: calm
    duration: 1m
signals:
  - name: every-row
    delivery:
      log_types: [screen, general]
    step: calm
    from: end
    within: 3m
  - name: late-rows-lost
    delivery:
      log_types: [error]
      from_step: hurt
      to_step: hurt
      lost_after: 30s
      late_rows: atrium_outsystems_late_rows_total{collector="pz"}
    step: calm
    within: 3m
`

func TestParseDelivery(t *testing.T) {
	s, err := Parse([]byte(withDelivery), testRegistry())
	if err != nil {
		t.Fatal(err)
	}
	every, late := s.Signals[0], s.Signals[1]
	if every.Kind() != KindDelivery || every.Delivery.FromStep != "warm" || every.Delivery.ToStep != "calm" || every.Delivery.LostAfter != 0 {
		t.Errorf("defaults: %+v, want the rows of every step and none lost", every.Delivery)
	}
	if late.Delivery.LostAfter.D() != 30*time.Second || !strings.Contains(late.Query(), "but those committed more than 30s late, which atrium_outsystems_late_rows_total{collector=\"pz\"} counts") {
		t.Errorf("lost_after and late_rows: %+v, query %q", late.Delivery, late.Query())
	}
}

func TestParseRejectsBadDelivery(t *testing.T) {
	for _, c := range []struct{ name, from, to, want string }{
		{"no log types", "log_types: [screen, general]", "log_types: []", "delivery needs log_types"},
		{"unknown log type", "log_types: [screen, general]", "log_types: [screen, audit]", "log type \"audit\" is not one of"},
		{"twice", "log_types: [screen, general]", "log_types: [screen, screen]", "listed twice"},
		{"unknown step", "from_step: hurt", "from_step: cold", "from_step \"cold\" is not a step"},
		{"out of order", "from_step: hurt\n      to_step: hurt", "from_step: calm\n      to_step: hurt", "comes after to_step"},
		{"window too early", "    step: calm\n    from: end", "    step: hurt\n    from: end", "opens at or after the end of its to_step \"calm\""},
		{"with a condition", "    step: calm\n    within: 3m", "    step: calm\n    condition: \"> 0\"\n    within: 3m", "no condition, state or labels"},
		{"with a query", "{collector=\"pz\"}\n", "{collector=\"pz\"}\n    promql: sum(up)\n", "exactly one of promql, logql, alert or delivery"},
		{"always", "    step: calm\n    from: end", "    step: calm\n    from: end\n    mode: always", "mode eventually, not always"},
		{"too far back", "    duration: 1m\nsignals:", "    duration: 6h\nsignals:", "at most 6h0m0s back"},
		{"late_rows without lost_after", "      lost_after: 30s\n", "", "late_rows needs lost_after"},
		{"late_rows with log_type", `{collector="pz"}`, `{log_type="error"}`, "without log_type"},
		{"late_rows not a selector", `atrium_outsystems_late_rows_total{collector="pz"}`, `sum(x)`, "late_rows must be a counter's name"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(withDelivery, c.from) {
				t.Fatalf("test case: %q not in the scenario", c.from)
			}
			_, err := Parse([]byte(strings.Replace(withDelivery, c.from, c.to, 1)), testRegistry())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want one containing %q", err, c.want)
			}
		})
	}
}
