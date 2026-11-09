package sqlserver

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
	"time"

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
# HELP atrium_collector_db_connections Connections to a monitored database by state (in_use, idle).
# TYPE atrium_collector_db_connections gauge
atrium_collector_db_connections{environment="prod",state="idle",target="a"} 0
atrium_collector_db_connections{environment="prod",state="in_use",target="a"} 1
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

// A target renamed in the config keeps its pool, which must not keep
// reporting under the old name.
func TestPoolMetricsFollowARenamedTarget(t *testing.T) {
	m, _, _ := newTestManager(t)
	now := time.Unix(1000, 0)
	m.pool.now = func() time.Time { return now }
	old := target("a", "db.example", "ref-a")
	_, release := mustOpen(t, m, old)
	release()
	renamed := old
	renamed.Name = "b"
	if got := ownersOf(m.pool.stats()); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("owners = %v, want [a]", got)
	}
	now = now.Add(time.Minute)
	_, release = mustOpen(t, m, renamed)
	release()
	// Both used it recently: the first in name order, so they do not take turns.
	if got := ownersOf(m.pool.stats()); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("owners = %v, want [a]", got)
	}
	now = now.Add(m.pool.idleTTL)
	_, release = mustOpen(t, m, renamed)
	release()
	if got := ownersOf(m.pool.stats()); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("owners = %v, want [b] once a has gone unused", got)
	}
}

// Waits of a closed handle stay in its owner's counters, so a rotation that
// closes the old handle does not lower them.
func TestPoolWaitCountersSurviveAClosedHandle(t *testing.T) {
	m, _, _ := newTestManager(t)
	tg := target("a", "db.example", "ref-a")
	_, release := mustOpen(t, m, tg)
	defer release()
	owner := poolOwner{target: "a"}
	m.pool.mu.Lock()
	m.pool.retired[owner] = sql.DBStats{WaitCount: 5, WaitDuration: 2 * time.Second}
	m.pool.retired[poolOwner{target: "gone"}] = sql.DBStats{WaitCount: 9}
	m.pool.mu.Unlock()
	var waits int64
	for _, s := range m.pool.stats()[owner] {
		waits += s.WaitCount
	}
	if waits < 5 {
		t.Errorf("waits = %d, want the closed handle's 5 kept", waits)
	}
	if _, ok := m.pool.stats()[poolOwner{target: "gone"}]; ok {
		t.Error("an owner with no handle left is still reported")
	}
}

func ownersOf(stats map[poolOwner][]sql.DBStats) []string {
	var out []string
	for owner := range stats {
		out = append(out, owner.target)
	}
	slices.Sort(out)
	return out
}
