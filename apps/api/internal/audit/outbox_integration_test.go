//go:build integration

package audit_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"heartbeat/apps/api/internal/audit"
	"heartbeat/apps/api/internal/audit/audittest"
	"heartbeat/apps/api/internal/store"
	"heartbeat/apps/api/internal/testenv"
	"heartbeat/internal/otlp"
	"heartbeat/internal/telemetry"
)

// write commits n records, one transaction each, and returns their ids in
// order.
func write(t *testing.T, pool *pgxpool.Pool, n int) []string {
	t.Helper()
	ids := make([]string, n)
	for i := range ids {
		err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
			record, err := audit.Write(context.Background(), store.New(tx), validChange())
			ids[i] = record.ID
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return ids
}

func outboxRows(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	stats, err := store.New(pool).AuditOutboxStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return stats.Rows
}

func pgDrainer(pool *pgxpool.Pool, collector *audittest.Collector, logs *syncBuffer) *audit.Drainer {
	client := otlp.NewClient()
	client.InitialBackoff, client.MaxBackoff, client.MaxAttempts = time.Millisecond, 2*time.Millisecond, 2
	d := audit.NewDrainer(audit.Outbox{DB: pool}, client, collector.URL, telemetry.Identity{Version: "test"},
		slog.New(slog.NewJSONHandler(logs, nil)))
	d.PollInterval, d.MinBackoff, d.MaxBackoff = 10*time.Millisecond, time.Millisecond, 10*time.Millisecond
	d.Delivered = prometheus.NewCounter(prometheus.CounterOpts{Name: audit.MetricRecordsDelivered})
	return d
}

// The outbox against PostgreSQL: a record is written with the change that
// commits and not with one that rolls back; the drainer sends what was
// committed, oldest first, valid against the schema, and deletes it once
// the Collector accepts it; while the Collector fails, the rows stay and
// the outbox metrics show them waiting.
func TestOutboxAgainstPostgres(t *testing.T) {
	t.Parallel()
	_, _, pool := testenv.MigratedPostgres(t)
	ctx := context.Background()

	rolledBack := errors.New("the change failed")
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := audit.Write(ctx, store.New(tx), validChange()); err != nil {
			return err
		}
		return rolledBack
	})
	if !errors.Is(err, rolledBack) {
		t.Fatal(err)
	}
	if n := outboxRows(t, pool); n != 0 {
		t.Fatalf("a rolled-back change left %d records", n)
	}

	ids := write(t, pool, 5)
	registry := prometheus.NewRegistry()
	registry.MustRegister(audit.OutboxCollector{Queries: store.New(pool)})
	time.Sleep(1100 * time.Millisecond)
	if got := scrape(t, registry, audit.MetricOutboxRows); got != 5 {
		t.Fatalf("%s = %v, want 5", audit.MetricOutboxRows, got)
	}
	if age := scrape(t, registry, audit.MetricOutboxOldestAge); age < 1 || age > 60 {
		t.Fatalf("%s = %v, want the age of the first record, about 1 s", audit.MetricOutboxOldestAge, age)
	}

	collector := audittest.NewCollector(t)
	var failing atomic.Bool
	failing.Store(true)
	collector.Answer(func(int) int {
		if failing.Load() {
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	})
	var logs syncBuffer
	d := pgDrainer(pool, collector, &logs)
	run(t, d)
	eventually(t, func() bool { return collector.Attempts() >= 6 }, "the drainer did not try again")
	if n := outboxRows(t, pool); n != 5 {
		t.Fatalf("%d rows left while the Collector failed, want all 5", n)
	}
	failing.Store(false)
	eventually(t, func() bool { return outboxRows(t, pool) == 0 }, "the outbox did not empty once the Collector accepted")

	records := collector.Records()
	if len(records) != len(ids) {
		t.Fatalf("delivered %d records, want %d", len(records), len(ids))
	}
	for i, record := range records {
		if got := audittest.Attribute(record, audit.AttributeRecordID); got != ids[i] {
			t.Fatalf("record %d is %s, want %s: oldest first", i, got, ids[i])
		}
		audittest.Validate(t, []byte(record.Body.GetStringValue()))
	}
	if got := scrape(t, registry, audit.MetricOutboxOldestAge); got != 0 {
		t.Fatalf("%s = %v with an empty outbox, want 0", audit.MetricOutboxOldestAge, got)
	}
	if !strings.Contains(logs.String(), "audit: delivery resumed") {
		t.Fatalf("logs lack the recovery:\n%s", logs.String())
	}
}

// Replicas drain the same outbox at once: FOR UPDATE SKIP LOCKED gives each
// round rows no other holds, so every record arrives once and the outbox
// empties.
func TestDrainersOfSeveralReplicasShareTheOutbox(t *testing.T) {
	t.Parallel()
	_, _, pool := testenv.MigratedPostgres(t)
	ids := write(t, pool, 600)
	collector := audittest.NewCollector(t)
	var drained sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	drainers := make([]*audit.Drainer, 3)
	for i := range drainers {
		drainers[i] = pgDrainer(pool, collector, &syncBuffer{})
		drainers[i].BatchSize = 50
		drained.Go(func() { drainers[i].Run(ctx) })
	}
	eventually(t, func() bool { return outboxRows(t, pool) == 0 }, "the outbox did not empty")
	cancel()
	drained.Wait()

	seen := map[string]int{}
	for _, record := range collector.Records() {
		seen[audittest.Attribute(record, audit.AttributeRecordID)]++
	}
	for _, id := range ids {
		if seen[id] != 1 {
			t.Errorf("record %s delivered %d times, want once", id, seen[id])
		}
	}
	var each []string
	total := 0.0
	for _, d := range drainers {
		n := testutil.ToFloat64(d.Delivered)
		each = append(each, fmt.Sprint(n))
		total += n
	}
	if total != 600 {
		t.Fatalf("drainers delivered %v records, %v in all; want 600", each, total)
	}
	t.Logf("delivered by each drainer: %v", each)
}

// A drainer stopped mid-round leaves its rows for another: none is lost.
func TestADrainerStoppedMidRoundLosesNothing(t *testing.T) {
	t.Parallel()
	_, _, pool := testenv.MigratedPostgres(t)
	ids := write(t, pool, 3)
	hold := make(chan struct{})
	reached := make(chan struct{}, 1)
	server := newServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case reached <- struct{}{}:
		default:
		}
		select {
		case <-hold:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(hold) })
	stuck := pgDrainer(pool, audittest.NewCollector(t), &syncBuffer{})
	stuck.Endpoint = server
	stuck.ExportTimeout = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		stuck.Run(ctx)
	}()
	<-reached
	cancel()
	<-done

	collector := audittest.NewCollector(t)
	run(t, pgDrainer(pool, collector, &syncBuffer{}))
	eventually(t, func() bool { return outboxRows(t, pool) == 0 }, "the next drainer did not deliver the stopped one's rows")
	if got := len(collector.Records()); got != len(ids) {
		t.Fatalf("delivered %d records, want %d", got, len(ids))
	}
}

// scrape returns the value of gauge name, which must have one sample.
func scrape(t *testing.T, registry *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == name && len(family.Metric) == 1 {
			return family.Metric[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("no %s in the scrape", name)
	return 0
}
