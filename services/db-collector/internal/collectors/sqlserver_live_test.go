//go:build sqlserver

package collectors

import (
	"context"
	"fmt"
	"maps"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	connector "atrium/services/db-collector/internal/connectors/sqlserver"
	collectorexport "atrium/services/db-collector/internal/export"
	collectormetadata "atrium/services/db-collector/internal/metadata"
	catalogsqlserver "atrium/services/db-collector/internal/probes/sqlserver"
)

// Credential references resolved through EnvCredentialResolver.
const (
	// liveCredentialRef is ATRIUM_CREDENTIAL_SQLSERVER_TEST, the
	// least-privilege collector login ("username:password").
	liveCredentialRef = "sqlserver-test"
	// liveSACredentialRef is ATRIUM_CREDENTIAL_SQLSERVER_TEST_SA, the sa
	// login.  Only the sysadmin check uses it; probes never run as sa.
	liveSACredentialRef = "sqlserver-test-sa"
	// liveControlCredentialRef is ATRIUM_CREDENTIAL_SQLSERVER_TEST_CONTROL,
	// a login holding CONTROL SERVER without being in sysadmin.  Only the
	// sysadmin check uses it.
	liveControlCredentialRef = "sqlserver-test-control"
)

// liveExecutor returns an executor for the disposable test server and its
// target logged in with credentialRef.  It skips the test outside make
// test-sqlserver.
func liveExecutor(t *testing.T, credentialRef string) (SQLExecutor, collectormetadata.DatabaseTarget) {
	t.Helper()
	addr := os.Getenv("ATRIUM_TEST_SQLSERVER_ADDR")
	if addr == "" {
		t.Skip("ATRIUM_TEST_SQLSERVER_ADDR is not set; run make test-sqlserver")
	}
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("ATRIUM_TEST_SQLSERVER_ADDR: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("ATRIUM_TEST_SQLSERVER_ADDR port: %v", err)
	}

	manager := connector.NewManager(connector.EnvCredentialResolver{})
	// The disposable test server uses a self-signed certificate.
	manager.TrustServerCertificate = true
	t.Cleanup(func() { _ = manager.Close() })
	executor := SQLExecutor{Manager: manager, Catalog: catalogsqlserver.DefaultCatalog()}
	target := collectormetadata.DatabaseTarget{
		EnvironmentSlug: "test",
		Name:            "sqlserver-test",
		Engine:          "sqlserver",
		Host:            host,
		Port:            port,
		DatabaseName:    "master",
		CredentialRef:   credentialRef,
	}
	return executor, target
}

// runLiveProbe runs the catalog probe name, with query as its query_template
// override when not empty, through the production executor path.
func runLiveProbe(t *testing.T, executor SQLExecutor, target collectormetadata.DatabaseTarget, name, query string) []collectorexport.Sample {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	samples, _, err := executor.RunProbe(ctx, collectormetadata.ScheduledProbe{
		CollectorID: "sqlserver-test",
		Target:      target,
		Definition:  collectormetadata.ProbeDefinition{Name: name, QueryTemplate: query},
		Assignment:  collectormetadata.ProbeAssignment{IntervalSeconds: 30},
	})
	if err != nil {
		t.Fatalf("run probe %s: %v", name, err)
	}
	return samples
}

// Runs every built-in probe against a real SQL Server (make test-sqlserver),
// logged in as the least-privilege collector login, so a probe that needs
// more than the documented grants fails here.  Fakes cannot catch what the
// catalog queries return on a live server, such as several rows mapping to
// one label set, nchar padding in label values, or a pivoted counter column
// that is NULL; the exporter keeps only the last of duplicate series.  Samples
// are also exported through a Prometheus registry, as in production, so type,
// value and label-value errors fail here.
func TestCatalogProbesAgainstSQLServer(t *testing.T) {
	executor, target := liveExecutor(t, liveCredentialRef)

	// blocking returns rows only while a request is blocked.
	mayBeEmpty := map[string]bool{"blocking": true}
	// cpu returns no row until the scheduler monitor writes its first record,
	// about a minute after SQL Server starts.
	waitForRows := map[string]time.Duration{"cpu": 3 * time.Minute}
	// SQL Server on Linux, which the test server runs, reports no system
	// idle time, so the cpu probe leaves other-process CPU out there.
	mayBeAbsent := map[string]bool{"atrium_sqlserver_cpu_other_process_ratio": true}
	registry := prometheus.NewRegistry()
	exporter := collectorexport.NewPrometheusExporter(registry)
	for _, name := range executor.Catalog.Names() {
		t.Run(name, func(t *testing.T) {
			run := func() ([]collectorexport.Sample, time.Duration) {
				t.Helper()
				started := time.Now()
				samples := runLiveProbe(t, executor, target, name, "")
				return samples, time.Since(started)
			}
			samples, _ := run()
			for deadline := time.Now().Add(waitForRows[name]); len(samples) == 0 && time.Now().Before(deadline); {
				time.Sleep(5 * time.Second)
				samples, _ = run()
			}
			if len(samples) == 0 && !mayBeEmpty[name] {
				t.Fatal("probe returned no samples")
			}
			// A second run on the pooled connection measures the query alone.
			_, elapsed := run()
			t.Logf("%d samples, query round trip %s", len(samples), elapsed)
			probe, _ := executor.Catalog.Get(name)
			emitted := map[string]bool{}
			seen := map[string]bool{}
			for _, sample := range samples {
				emitted[sample.Metric] = true
				if sample.Type == collectorexport.Counter && sample.Value < 0 {
					t.Errorf("counter %s is negative: %v", sample.Metric, sample.Value)
				}
				for label, value := range sample.Labels {
					if value != strings.TrimSpace(value) {
						t.Errorf("%s label %s=%q has surrounding whitespace", sample.Metric, label, value)
					}
				}
				key := seriesKey(sample.Metric, sample.Labels)
				if seen[key] {
					t.Errorf("duplicate series %s", key)
				}
				seen[key] = true
			}
			// Every descriptor yields a sample, so no column of a pivoted
			// row is NULL on a stock server.
			for _, metric := range probe.Metrics {
				if !emitted[metric.Name] && !mayBeEmpty[name] && !mayBeAbsent[metric.Name] {
					t.Errorf("no %s sample", metric.Name)
				}
			}
			scope := collectorexport.Scope{Collector: "sqlserver-test", Target: target.Name, Probe: name}
			if err := exporter.RecordScope(scope, samples); err != nil {
				t.Errorf("export: %v", err)
			}
		})
	}
	if _, err := registry.Gather(); err != nil {
		t.Fatalf("Gather: %v", err)
	}
}

// Reads the session's lock timeout and deadlock priority from inside a probe
// batch, through a query_template override of a catalog probe, so it covers
// the exact path every probe and every operator override takes.  The second
// run reuses the pooled connection after go-mssqldb's session reset.
func TestSessionSettingsAgainstSQLServer(t *testing.T) {
	executor, target := liveExecutor(t, liveCredentialRef)
	// sessions decodes the gauge session_count labelled by status.
	const query = `SELECT 'lock_timeout' AS status, @@LOCK_TIMEOUT AS session_count
UNION ALL
SELECT 'deadlock_priority', deadlock_priority FROM sys.dm_exec_sessions WHERE session_id = @@SPID`
	want := map[string]float64{
		"lock_timeout":      float64(connector.LockTimeout.Milliseconds()),
		"deadlock_priority": -5, // LOW
	}
	for run := range 2 {
		got := map[string]float64{}
		for _, sample := range runLiveProbe(t, executor, target, "sessions", query) {
			got[sample.Labels["status"]] = sample.Value
		}
		if !maps.Equal(got, want) {
			t.Fatalf("run %d: session settings = %v, want %v", run+1, got, want)
		}
	}
}

// Checks the sysadmin-equivalence flag the Manager records when it creates a
// pool: 0 for the collector login, 1 for sa and for a CONTROL SERVER login.
func TestSysadminCheckAgainstSQLServer(t *testing.T) {
	for _, tc := range []struct {
		name          string
		credentialRef string
		env           string
		want          bool
	}{
		{"least-privilege login", liveCredentialRef, "", false},
		{"sa", liveSACredentialRef, "ATRIUM_CREDENTIAL_SQLSERVER_TEST_SA", true},
		{"control server login", liveControlCredentialRef, "ATRIUM_CREDENTIAL_SQLSERVER_TEST_CONTROL", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" && os.Getenv(tc.env) == "" {
				t.Skip(tc.env + " is not set")
			}
			executor, target := liveExecutor(t, tc.credentialRef)
			if _, ok := executor.TargetSysadmin(target); ok {
				t.Fatal("sysadmin flag known before the first connection")
			}
			runLiveProbe(t, executor, target, "sessions", "")
			got, ok := executor.TargetSysadmin(target)
			if !ok || got != tc.want {
				t.Fatalf("TargetSysadmin = %v, %v; want %v, true", got, ok, tc.want)
			}
		})
	}
}

// seriesKey identifies a series by metric name and sorted labels.
func seriesKey(metric string, labels map[string]string) string {
	var b strings.Builder
	b.WriteString(metric)
	for _, name := range slices.Sorted(maps.Keys(labels)) {
		fmt.Fprintf(&b, " %s=%q", name, labels[name])
	}
	return b.String()
}
