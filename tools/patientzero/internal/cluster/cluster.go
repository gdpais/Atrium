// Package cluster holds PatientZero's cluster faults: restarts and outages
// of the workloads around it, the db-collector, the OTel Collector, Loki
// and SQL Server. PatientZero cannot apply them, on purpose; each one is a
// request to the host agent of its run (runner.Delegate), which applies it
// with the kubeconfig and Docker it already uses (scenarios/README.md,
// "Cluster faults").
package cluster

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"heartbeat/tools/patientzero/internal/scenario"
)

// Workloads a cluster fault can act on, as the host agent knows them: the
// Heartbeat StatefulSets of the same name (one pod each), and SQL Server,
// the Docker container outside the cluster.
var Workloads = []string{"db-collector", "loki", "otel-collector", "sqlserver", "tempo"}

// Delegator applies (start) or removes (stop) a cluster fault and returns
// once it has.
type Delegator interface {
	Do(ctx context.Context, action, typ string, params map[string]any) error
}

// Register adds the restart and outage fault types to reg, delegated to d.
func Register(reg *scenario.Registry, d Delegator) {
	for _, t := range []struct{ name, description string }{
		{"restart", "Restarts a workload when its step starts and waits until it is ready again: the pod is deleted, or SQL Server's container restarted"},
		{"outage", "Stops a workload for its step: the StatefulSet scaled to 0, or SQL Server's container stopped; started again, and ready, when the step ends"},
	} {
		typ := t.name
		reg.Register(scenario.FaultType{
			Name:        typ,
			Scope:       scenario.ScopeCluster,
			Description: t.description,
			New: func(params map[string]any) (scenario.Fault, error) {
				var p struct {
					Workload string `json:"workload"`
				}
				if err := scenario.DecodeParams(params, &p); err != nil {
					return nil, err
				}
				if !slices.Contains(Workloads, p.Workload) {
					return nil, fmt.Errorf("workload %q is not one of %s", p.Workload, strings.Join(Workloads, ", "))
				}
				return &fault{d: d, typ: typ, params: map[string]any{"workload": p.Workload}, restart: typ == "restart"}, nil
			},
		})
	}
}

// fault is one cluster fault, applied by the host agent.
type fault struct {
	d       Delegator
	typ     string
	params  map[string]any
	restart bool
}

func (f *fault) Start(ctx context.Context) error { return f.d.Do(ctx, "start", f.typ, f.params) }

// Stop ends an outage. A restart is over once it has started: nothing to
// remove.
func (f *fault) Stop(ctx context.Context) error {
	if f.restart {
		return nil
	}
	return f.d.Do(ctx, "stop", f.typ, f.params)
}
