package collectors

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	collectorconfig "atrium/internal/config"
	collectorexport "atrium/services/db-collector/internal/export"
	collectormetadata "atrium/services/db-collector/internal/metadata"
)

// observingExecutor records the probes it runs and the collector hooks it
// receives, and reports a fixed sysadmin flag.
type observingExecutor struct {
	mu     sync.Mutex
	probes []collectormetadata.ScheduledProbe
	events []string
}

func (e *observingExecutor) RunProbe(_ context.Context, item collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.probes = append(e.probes, item)
	return nil, nil, nil
}

func (e *observingExecutor) TargetSysadmin(collectormetadata.DatabaseTarget) (bool, bool) {
	return true, true
}

func (e *observingExecutor) InitCollector(collector collectorconfig.CollectorRuntimeConfig) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, "init "+collector.ID)
}

func (e *observingExecutor) ForgetCollector(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, "forget "+id)
}

func (e *observingExecutor) snapshot() ([]collectormetadata.ScheduledProbe, []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.probes), slices.Clone(e.events)
}

func logsCollector() collectorconfig.CollectorRuntimeConfig {
	return collectorconfig.CollectorRuntimeConfig{
		ID: "oslogs", Kind: collectorconfig.KindOutSystemsLogs, Enabled: true, ScrapeInterval: time.Hour,
		OutSystemsLogs: collectorconfig.OutSystemsLogsConfig{Lag: time.Minute, BatchLimit: 10, LogTimezone: "UTC"},
		Targets: []collectorconfig.TargetRuntimeConfig{{
			Name: "logging-db", EnvironmentSlug: "prod", Engine: collectorconfig.KindOutSystemsLogs,
			Probes: []collectorconfig.ProbeRuntimeConfig{{Name: "error", Lag: 2 * time.Minute}},
		}},
	}
}

func TestEngineExecutorRoutesByCollectorKind(t *testing.T) {
	logs := &observingExecutor{}
	engines := EngineExecutor{collectorconfig.KindSQLServer: fakeExecutor{}, collectorconfig.KindOutSystemsLogs: logs}
	exporter := collectorexport.NewInMemoryExporter()
	runner := NewRunner(engines, exporter, nil)

	if _, err := runner.RunOnce(context.Background(), logsCollector()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	probes, _ := logs.snapshot()
	if len(probes) != 1 || probes[0].Definition.Name != "error" {
		t.Fatalf("outsystems executor ran %+v", probes)
	}
	// The collector's log settings reach the executor with the probe, and so
	// does the log type's own lag.
	if got := probes[0].OutSystemsLogs; got.BatchLimit != 10 || got.Lag != time.Minute {
		t.Fatalf("probe carries settings %+v", got)
	}
	if got := probes[0].Definition.Lag; got != 2*time.Minute {
		t.Fatalf("probe lag = %s, want 2m", got)
	}
	if got := exporter.LastValue(MetricTargetLoginSysadmin); got != 1 {
		t.Fatalf("%s = %v, want the outsystems executor's answer", MetricTargetLoginSysadmin, got)
	}

	unknown := logsCollector()
	unknown.Targets[0].Engine = "oracle"
	_, err := runner.RunOnce(context.Background(), unknown)
	if err == nil || !strings.Contains(err.Error(), `no executor for collector kind "oracle"`) {
		t.Fatalf("RunOnce error = %v, want no executor for oracle", err)
	}
	if got := engines.Kinds(); !slices.Equal(got, []string{"outsystems_logs", "sqlserver"}) {
		t.Fatalf("Kinds = %v", got)
	}
}

// A Poller tells an observing executor when its collector starts and stops.
func TestPollerTellsTheExecutorWhenACollectorStartsAndStops(t *testing.T) {
	logs := &observingExecutor{}
	runner := NewRunner(EngineExecutor{collectorconfig.KindOutSystemsLogs: logs}, collectorexport.NewInMemoryExporter(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	cycled := make(chan struct{}, 1)
	go func() {
		done <- Poller{Runner: runner, Collector: logsCollector(), Report: func(CycleResult) {
			select {
			case cycled <- struct{}{}:
			default:
			}
		}}.Start(ctx)
	}()
	<-cycled
	cancel()
	<-done
	if _, events := logs.snapshot(); !slices.Equal(events, []string{"init oslogs", "forget oslogs"}) {
		t.Fatalf("events = %v, want init then forget", events)
	}
}
