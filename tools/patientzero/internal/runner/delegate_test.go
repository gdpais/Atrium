package runner

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"atrium/tools/patientzero/internal/cluster"
	"atrium/tools/patientzero/internal/scenario"
	"atrium/tools/patientzero/internal/signal"
)

// agentRunner is a runner with the cluster faults and a delegate, over a
// Prometheus that always answers 1.
func agentRunner(t *testing.T) (*Runner, *Delegate, http.Handler) {
	t.Helper()
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[0,"1"]}]}}`)
	}))
	t.Cleanup(prom.Close)
	reg := scenario.NewRegistry()
	d := &Delegate{Timeout: 2 * time.Second}
	cluster.Register(reg, d)
	s, err := scenario.Parse([]byte(`
version: 1
name: loki-outage
description: Test scenario.
load: none
steps:
  - name: warm
    duration: 30ms
  - name: down
    duration: 100ms
    fault: {type: outage, params: {workload: loki}}
signals:
  - name: up
    promql: up
    condition: "> 0"
    within: 300ms
`), reg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r := New(ctx, Config{
		Scenarios: []*scenario.Scenario{s}, Registry: reg,
		Scopes:   []scenario.Scope{scenario.ScopeInProcess, scenario.ScopeCluster},
		Delegate: d, Sources: signal.Sources{Prometheus: prom.URL},
		JournalDir: t.TempDir(), PollInterval: 10 * time.Millisecond, SummaryInterval: 40 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t.Cleanup(r.Wait)
	return r, d, r.Handler(token)
}

func adminCall(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// waitRequest polls the run's status, as the host agent does, until it
// asks for action.
func waitRequest(t *testing.T, r *Runner, action string) *FaultRequest {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := r.Status(); st.FaultRequest != nil && st.FaultRequest.Action == action {
			return st.FaultRequest
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no %s request in the run's status", action)
	return nil
}

// A cluster fault runs only with a host agent attached; the runner asks it
// to apply the fault when the step starts and to remove it when the step
// ends, and waits for each answer.
func TestClusterFaultsGoToTheHostAgent(t *testing.T) {
	r, _, h := agentRunner(t)
	if l := r.Scenarios()[0]; !l.Agent || !l.Runnable {
		t.Fatalf("listing %+v, want runnable with an agent", l)
	}
	if w := adminCall(t, h, "POST", "/admin/scenarios/loki-outage/start", ""); w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "host agent") {
		t.Fatalf("start without an agent: %d %s, want 422", w.Code, w.Body)
	}
	if w := adminCall(t, h, "POST", "/admin/scenarios/loki-outage/start?agent=host", ""); w.Code != http.StatusAccepted {
		t.Fatalf("start with an agent: %d %s", w.Code, w.Body)
	}
	start := waitRequest(t, r, ActionStart)
	if start.Type != "outage" || start.Params["workload"] != "loki" {
		t.Fatalf("request %+v", start)
	}
	if w := adminCall(t, h, "POST", "/admin/run/faults/nope", `{"error": ""}`); w.Code != http.StatusNotFound {
		t.Fatalf("answer to an unknown request: %d", w.Code)
	}
	if w := adminCall(t, h, "POST", "/admin/run/faults/"+start.ID, `{"error": ""}`); w.Code != http.StatusNoContent {
		t.Fatalf("answer: %d %s", w.Code, w.Body)
	}
	stop := waitRequest(t, r, ActionStop)
	adminCall(t, h, "POST", "/admin/run/faults/"+stop.ID, `{}`)
	r.Wait()
	if st, _ := r.Status(); st.State != StatePassed || st.FaultRequest != nil {
		t.Fatalf("run %s %s, request %+v", st.State, st.Error, st.FaultRequest)
	}
}

// An agent that fails to apply the fault ends the run in error; one that
// detaches makes a stopping run end at once instead of waiting for it.
func TestHostAgentFailuresEndTheRun(t *testing.T) {
	r, _, h := agentRunner(t)
	adminCall(t, h, "POST", "/admin/scenarios/loki-outage/start?agent=host", "")
	req := waitRequest(t, r, ActionStart)
	adminCall(t, h, "POST", "/admin/run/faults/"+req.ID, `{"error": "kubectl: forbidden"}`)
	r.Wait()
	if st, _ := r.Status(); st.State != StateError || !strings.Contains(st.Error, "kubectl: forbidden") {
		t.Fatalf("run %s %q, want error naming the agent's failure", st.State, st.Error)
	}

	adminCall(t, h, "POST", "/admin/scenarios/loki-outage/start?agent=host", "")
	waitRequest(t, r, ActionStart)
	began := time.Now()
	if w := adminCall(t, h, "POST", "/admin/run/stop?detach=1", ""); w.Code != http.StatusOK {
		t.Fatalf("stop: %d %s", w.Code, w.Body)
	}
	if time.Since(began) > time.Second {
		t.Fatal("a detached agent's run waited for it")
	}
	if st, _ := r.Status(); st.State != StateStopped || !strings.Contains(st.Error, ErrNoAgent.Error()) {
		t.Fatalf("run %s %q, want stopped, with the detached agent named", st.State, st.Error)
	}
}

func TestAgentTimeout(t *testing.T) {
	d := &Delegate{Timeout: 30 * time.Millisecond}
	if err := d.Do(context.Background(), ActionStart, "restart", nil); err != ErrNoAgent {
		t.Fatalf("no agent attached: %v", err)
	}
	d.attach()
	err := d.Do(context.Background(), ActionStart, "restart", map[string]any{"workload": "loki"})
	if err == nil || !strings.Contains(err.Error(), "did not start the restart fault within 30ms") || d.Pending() != nil {
		t.Fatalf("timeout: %v, pending %+v", err, d.Pending())
	}
}
