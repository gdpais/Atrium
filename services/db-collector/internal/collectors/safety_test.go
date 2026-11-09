package collectors

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	connector "atrium/services/db-collector/internal/connectors/sqlserver"
	collectorexport "atrium/services/db-collector/internal/export"
	collectormetadata "atrium/services/db-collector/internal/metadata"
	catalogsqlserver "atrium/services/db-collector/internal/probes/sqlserver"
)

// sysadminExecutor is a ProbeExecutor that also implements SysadminReporter
// with a settable answer.
type sysadminExecutor struct {
	funcExecutor

	mu       sync.Mutex
	sysadmin bool
	ok       bool
	targets  []string
}

func newSysadminExecutor(sysadmin, ok bool) *sysadminExecutor {
	return &sysadminExecutor{
		funcExecutor: func(_ context.Context, item collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
			return sampleFor(item), nil, nil
		},
		sysadmin: sysadmin,
		ok:       ok,
	}
}

func (e *sysadminExecutor) set(sysadmin, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sysadmin, e.ok = sysadmin, ok
}

func (e *sysadminExecutor) TargetSysadmin(target collectormetadata.DatabaseTarget) (bool, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.targets = append(e.targets, target.Name)
	return e.sysadmin, e.ok
}

func TestRunnerExportsLoginSysadmin(t *testing.T) {
	plain := funcExecutor(func(_ context.Context, item collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
		return sampleFor(item), nil, nil
	})
	for _, tc := range []struct {
		name      string
		executor  ProbeExecutor
		want      float64
		wantFound bool
	}{
		{"executor without reporter", plain, 0, false},
		{"unknown login", newSysadminExecutor(false, false), 0, false},
		{"sysadmin login", newSysadminExecutor(true, true), 1, true},
		{"least-privilege login", newSysadminExecutor(false, true), 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exporter := collectorexport.NewInMemoryExporter()
			runner := NewRunner(tc.executor, exporter, nil).WithLogger(slog.New(slog.DiscardHandler))
			if _, err := runner.RunOnce(context.Background(), testCollector(time.Minute, testTarget("core-db", "waits", "sessions"))); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			got, found := exporter.Value(MetricTargetLoginSysadmin, healthLabels("core-db"))
			if found != tc.wantFound || got != tc.want {
				t.Fatalf("%s = %v (found %v), want %v (found %v)", MetricTargetLoginSysadmin, got, found, tc.want, tc.wantFound)
			}
			if reporter, ok := tc.executor.(*sysadminExecutor); ok {
				if len(reporter.targets) != 1 || reporter.targets[0] != "core-db" {
					t.Fatalf("TargetSysadmin calls = %v, want one per target and cycle", reporter.targets)
				}
			}
		})
	}
}

func TestLoginSysadminSeriesIsDeletedWhenUnknown(t *testing.T) {
	exporter := collectorexport.NewInMemoryExporter()
	executor := newSysadminExecutor(true, true)
	runner := NewRunner(executor, exporter, nil).WithLogger(slog.New(slog.DiscardHandler))
	collector := testCollector(time.Minute, testTarget("core-db", "waits"))
	if _, err := runner.RunOnce(context.Background(), collector); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if _, ok := exporter.Value(MetricTargetLoginSysadmin, healthLabels("core-db")); !ok {
		t.Fatal("expected the sysadmin series after the first cycle")
	}
	executor.set(false, false)
	if _, err := runner.RunOnce(context.Background(), collector); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if _, ok := exporter.Value(MetricTargetLoginSysadmin, healthLabels("core-db")); ok {
		t.Fatal("stale sysadmin series survived once the login became unknown")
	}
}

func TestProbeQueryAppliesSessionSettings(t *testing.T) {
	probe, ok := catalogsqlserver.DefaultCatalog().Get("sessions")
	if !ok {
		t.Fatal("sessions probe missing")
	}
	override := "SELECT 'x' AS status, 1 AS session_count"
	for _, tc := range []struct {
		name     string
		template string
		want     string
	}{
		{"catalog query", "", probe.QueryTemplate},
		{"query_template override", override, override},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := collectormetadata.ScheduledProbe{Definition: collectormetadata.ProbeDefinition{Name: "sessions", QueryTemplate: tc.template}}
			if got, want := probeQuery(item, probe), connector.SessionSettings+tc.want; got != want {
				t.Fatalf("probe batch = %q, want %q", got, want)
			}
		})
	}
}
