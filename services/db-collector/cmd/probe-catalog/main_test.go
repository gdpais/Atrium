package main

import (
	"strings"
	"testing"

	connector "atrium/services/db-collector/internal/connectors/sqlserver"
	catalogoutsystems "atrium/services/db-collector/internal/probes/outsystems"
	catalogsqlserver "atrium/services/db-collector/internal/probes/sqlserver"
)

func TestListShowsEveryMetric(t *testing.T) {
	catalog := catalogsqlserver.DefaultCatalog()
	var out strings.Builder
	if err := run(&out, catalog, catalogoutsystems.DefaultCatalog(), ""); err != nil {
		t.Fatal(err)
	}
	for _, name := range catalog.Names() {
		probe, _ := catalog.Get(name)
		for _, metric := range probe.Metrics {
			if !strings.Contains(out.String(), metric.Name) {
				t.Errorf("listing lacks %s (probe %s)", metric.Name, name)
			}
		}
	}
	want := "waits atrium_sqlserver_wait_seconds_total counter wait_time_ms 0.001 wait_type"
	for line := range strings.Lines(out.String()) {
		if strings.Join(strings.Fields(line), " ") == want {
			return
		}
	}
	t.Errorf("no line %q:\n%s", want, out.String())
}

// The printed batch must be the one the collector sends, so what a developer
// runs by hand behaves like the probe, lock timeout included.
func TestSQLPrintsTheCollectorBatch(t *testing.T) {
	catalog := catalogsqlserver.DefaultCatalog()
	probe, _ := catalog.Get("sessions")
	var out strings.Builder
	if err := run(&out, catalog, catalogoutsystems.DefaultCatalog(), "sessions"); err != nil {
		t.Fatal(err)
	}
	if want := connector.WithSessionSettings(probe.QueryTemplate) + "\n"; out.String() != want {
		t.Fatalf("got %q, want %q", out.String(), want)
	}
}

func TestSQLRejectsAnUnknownProbe(t *testing.T) {
	var out strings.Builder
	err := run(&out, catalogsqlserver.DefaultCatalog(), catalogoutsystems.DefaultCatalog(), "nope")
	if err == nil || !strings.Contains(err.Error(), "waits") || !strings.Contains(err.Error(), "error") {
		t.Fatalf("got %v, want an error listing the probes", err)
	}
}

func TestListShowsEveryLogType(t *testing.T) {
	logs := catalogoutsystems.DefaultCatalog()
	var out strings.Builder
	if err := run(&out, catalogsqlserver.DefaultCatalog(), logs, ""); err != nil {
		t.Fatal(err)
	}
	for _, name := range logs.Names() {
		if !strings.Contains(out.String(), "queries/"+name+".sql") {
			t.Errorf("listing lacks log type %s:\n%s", name, out.String())
		}
	}
}

// A log type's batch is the one the collector sends, after DECLAREs for the
// parameters it binds, so it runs as is in sqlcmd.
func TestSQLPrintsTheLogTypeBatch(t *testing.T) {
	logs := catalogoutsystems.DefaultCatalog()
	query, _ := logs.Get("error")
	var out strings.Builder
	if err := run(&out, catalogsqlserver.DefaultCatalog(), logs, "error"); err != nil {
		t.Fatal(err)
	}
	if want := logParameters + connector.WithSessionSettings(query.SQL) + "\n"; out.String() != want {
		t.Fatalf("got %q, want %q", out.String(), want)
	}
	for _, parameter := range []string{"@from datetime2", "@to datetime2", "@limit int"} {
		if !strings.Contains(out.String(), "DECLARE "+parameter) {
			t.Errorf("batch does not declare %s", parameter)
		}
	}
}
