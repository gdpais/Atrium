package sqlserver

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestPoolMetricsFollowTheHandles(t *testing.T) {
	m, _, _ := newTestManager(t)
	registry := prometheus.NewRegistry()
	if err := registry.Register(m.Collector()); err != nil {
		t.Fatal(err)
	}
	if got, err := testutil.GatherAndCount(registry); err != nil || got != 0 {
		t.Fatalf("no handle yet, but %d series (%v)", got, err)
	}

	tg := target("a", "db-a.example", "ref-a")
	tg.EnvironmentSlug = "prod"
	db, cleanup := mustOpen(t, m, tg)
	defer cleanup()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	want := `
# HELP heartbeat_collector_db_connections Connections to a monitored database by state (in_use, idle).
# TYPE heartbeat_collector_db_connections gauge
heartbeat_collector_db_connections{environment="prod",state="idle",target="a"} 0
heartbeat_collector_db_connections{environment="prod",state="in_use",target="a"} 1
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(want), MetricConnections); err != nil {
		t.Error(err)
	}
	for _, name := range []string{MetricConnectionWaits, MetricConnectionWaitSeconds} {
		if got, err := testutil.GatherAndCount(registry, name); err != nil || got != 1 {
			t.Errorf("%s has %d series (%v), want 1", name, got, err)
		}
	}
}

// A rotated credential opens a second handle for the target while the first
// drains; two series with one set of labels would fail the whole scrape.
func TestPoolMetricsSumTheHandlesOfATarget(t *testing.T) {
	m, _, resolver := newTestManager(t)
	registry := prometheus.NewRegistry()
	if err := registry.Register(m.Collector()); err != nil {
		t.Fatal(err)
	}
	tg := target("a", "db-a.example", "ref-a")
	_, first := mustOpen(t, m, tg)
	defer first()
	resolver.creds["ref-a"] = Credential{Username: "u", Password: "rotated"}
	_, second := mustOpen(t, m, tg)
	defer second()
	if got := len(m.pool.stats()[poolOwner{target: "a"}]); got != 2 {
		t.Fatalf("%d handles for the target, want 2", got)
	}
	if _, err := registry.Gather(); err != nil {
		t.Fatalf("scrape failed: %v", err)
	}
}
