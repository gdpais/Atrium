package collectors

import (
	"context"
	"fmt"
	"maps"
	"slices"

	collectorconfig "atrium/internal/config"
	collectorexport "atrium/services/db-collector/internal/export"
	collectormetadata "atrium/services/db-collector/internal/metadata"
)

// EngineExecutor is a [ProbeExecutor] that runs each probe with the executor
// of its target's engine, which is the collector kind ("sqlserver",
// "outsystems_logs").  It lets one [Runner], with its backoff, hard deadline
// and self-metrics, run collectors of every kind.
//
// It implements [SysadminReporter] and [CollectorObserver] by forwarding to
// the executors that implement them.
type EngineExecutor map[string]ProbeExecutor

// RunProbe implements [ProbeExecutor].
func (e EngineExecutor) RunProbe(ctx context.Context, item collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
	executor, ok := e[item.Target.Engine]
	if !ok {
		return nil, nil, fmt.Errorf("no executor for collector kind %q", item.Target.Engine)
	}
	return executor.RunProbe(ctx, item)
}

// TargetSysadmin implements [SysadminReporter].
func (e EngineExecutor) TargetSysadmin(target collectormetadata.DatabaseTarget) (sysadmin, ok bool) {
	reporter, ok := e[target.Engine].(SysadminReporter)
	if !ok {
		return false, false
	}
	return reporter.TargetSysadmin(target)
}

// InitCollector implements [CollectorObserver]: it tells the executor of
// collector's kind.
func (e EngineExecutor) InitCollector(collector collectorconfig.CollectorRuntimeConfig) {
	if observer, ok := e[collector.Kind].(CollectorObserver); ok {
		observer.InitCollector(collector)
	}
}

// ForgetCollector implements [CollectorObserver]: it tells every executor,
// since the collector's kind may have changed since it started.
func (e EngineExecutor) ForgetCollector(collectorID string) {
	for _, kind := range slices.Sorted(maps.Keys(e)) {
		if observer, ok := e[kind].(CollectorObserver); ok {
			observer.ForgetCollector(collectorID)
		}
	}
}

// Kinds returns the collector kinds e runs, sorted.
func (e EngineExecutor) Kinds() []string {
	return slices.Sorted(maps.Keys(e))
}
