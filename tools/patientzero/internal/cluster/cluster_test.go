package cluster

import (
	"context"
	"strings"
	"testing"

	"atrium/tools/patientzero/internal/scenario"
)

type recorder struct{ calls []string }

func (r *recorder) Do(_ context.Context, action, typ string, params map[string]any) error {
	r.calls = append(r.calls, action+" "+typ+" "+params["workload"].(string))
	return nil
}

func TestFaultsAreDelegated(t *testing.T) {
	rec := &recorder{}
	reg := scenario.NewRegistry()
	Register(reg, rec)
	for _, typ := range []string{"restart", "outage"} {
		if ft, _ := reg.Type(typ); ft.Scope != scenario.ScopeCluster {
			t.Fatalf("%s has scope %q, want cluster", typ, ft.Scope)
		}
		f, err := reg.New(scenario.FaultSpec{Type: typ, Params: map[string]any{"workload": "loki"}})
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Start(context.Background())
		_ = f.Stop(context.Background())
	}
	// A restart is over once it has started: only an outage is stopped.
	if got := strings.Join(rec.calls, "; "); got != "start restart loki; start outage loki; stop outage loki" {
		t.Fatalf("calls %q", got)
	}
	for _, params := range []map[string]any{{}, {"workload": "grafana"}, {"workload": "loki", "pod": "loki-0"}} {
		if _, err := reg.New(scenario.FaultSpec{Type: "outage", Params: params}); err == nil {
			t.Errorf("params %v accepted", params)
		}
	}
}
