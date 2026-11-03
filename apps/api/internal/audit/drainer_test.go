package audit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"

	"heartbeat/apps/api/internal/audit"
	"heartbeat/apps/api/internal/audit/audittest"
	"heartbeat/apps/api/internal/store"
	"heartbeat/internal/otlp"
	"heartbeat/internal/telemetry"
)

// memoryOutbox is an outbox in memory, for one drainer.
type memoryOutbox struct {
	mu     sync.Mutex
	rows   []audit.Row
	rounds int
	// limits records the limit of each round.
	limits []int
}

func (o *memoryOutbox) Drain(ctx context.Context, limit int, deliver func(context.Context, []audit.Row) error) (int, error) {
	o.mu.Lock()
	o.rounds++
	o.limits = append(o.limits, limit)
	claimed := append([]audit.Row(nil), o.rows[:min(limit, len(o.rows))]...)
	o.mu.Unlock()
	if len(claimed) == 0 {
		return 0, nil
	}
	if err := deliver(ctx, claimed); err != nil {
		return 0, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.rows = o.rows[len(claimed):]
	return len(claimed), nil
}

func (o *memoryOutbox) left() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.rows)
}

// rows returns n valid records, as the outbox holds them.
func rows(t *testing.T, n int) []audit.Row {
	t.Helper()
	out := make([]audit.Row, n)
	for i := range out {
		db := &recorder{}
		record, err := audit.Write(context.Background(), store.New(db), validChange())
		if err != nil {
			t.Fatal(err)
		}
		// PostgreSQL returns jsonb with spaces after its separators.
		spaced := strings.ReplaceAll(string(db.execs[0][1].([]byte)), `":`, `": `)
		out[i] = audit.Row{Seq: int64(i + 1), ID: record.ID, Record: []byte(spaced)}
	}
	return out
}

// testDrainer is a Drainer of outbox to collector, with waits short enough
// for tests.
func testDrainer(outbox *memoryOutbox, collector *audittest.Collector, logs *bytes.Buffer) *audit.Drainer {
	client := otlp.NewClient()
	// Two attempts a round, so a failing Collector fails rounds; in
	// production the client retries until ExportTimeout.
	client.InitialBackoff, client.MaxBackoff, client.MaxAttempts = time.Millisecond, 2*time.Millisecond, 2
	d := &audit.Drainer{
		Outbox:   outbox,
		Exporter: client,
		Endpoint: collector.URL,
		Resource: audit.Resource(telemetry.Identity{
			Service: "api", Version: "1.2.3", InstanceID: "api-7d9f-xk2p", Environment: "kind",
		}),
		Delivered:     prometheus.NewCounter(prometheus.CounterOpts{Name: audit.MetricRecordsDelivered}),
		Logger:        slog.New(slog.NewJSONHandler(logs, nil)),
		BatchSize:     audit.DefaultBatchSize,
		PollInterval:  5 * time.Millisecond,
		ExportTimeout: 200 * time.Millisecond,
		MinBackoff:    time.Millisecond,
		MaxBackoff:    5 * time.Millisecond,
	}
	return d
}

// run starts d and stops it when the test ends.
func run(t *testing.T, d *audit.Drainer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func eventually(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(time.Millisecond)
	}
}

// Each record reaches the Collector as an OTLP log record with ADR 0016's
// identity: service.name heartbeat-api, event.name heartbeat.audit, the
// record's id, and the record, valid against its schema, as the body.
func TestDrainerSendsEachRecordAsAnOTLPLogRecord(t *testing.T) {
	collector := audittest.NewCollector(t)
	outbox := &memoryOutbox{rows: rows(t, 3)}
	want := append([]audit.Row(nil), outbox.rows...)
	var logs bytes.Buffer
	d := testDrainer(outbox, collector, &logs)
	run(t, d)
	eventually(t, func() bool { return outbox.left() == 0 }, "the outbox did not empty")

	accepted := collector.Accepted()
	if len(accepted) != 1 || len(accepted[0].ResourceLogs) != 1 {
		t.Fatalf("accepted %d requests, want one with one resource", len(accepted))
	}
	resource := map[string]string{}
	for _, kv := range accepted[0].ResourceLogs[0].Resource.Attributes {
		resource[kv.Key] = kv.Value.GetStringValue()
	}
	wantResource := map[string]string{
		"service.name": "heartbeat-api", "service.version": "1.2.3",
		"service.instance.id": "api-7d9f-xk2p", "deployment.environment.name": "kind",
	}
	if fmt.Sprint(resource) != fmt.Sprint(wantResource) {
		t.Fatalf("resource = %v, want %v", resource, wantResource)
	}
	if scope := accepted[0].ResourceLogs[0].ScopeLogs[0].Scope.GetName(); scope != "heartbeat/api/audit" {
		t.Fatalf("scope = %q", scope)
	}
	records := collector.Records()
	if len(records) != len(want) {
		t.Fatalf("got %d records, want %d", len(records), len(want))
	}
	for i, record := range records {
		if got := audittest.Attribute(record, "event.name"); got != "heartbeat.audit" {
			t.Errorf("record %d: event.name = %q", i, got)
		}
		if got := audittest.Attribute(record, "log.record.uid"); got != want[i].ID {
			t.Errorf("record %d: log.record.uid = %q, want the record's id %s (in order)", i, got, want[i].ID)
		}
		if record.SeverityNumber != logspb.SeverityNumber_SEVERITY_NUMBER_INFO || record.ObservedTimeUnixNano == 0 {
			t.Errorf("record %d: severity %v, observed %d", i, record.SeverityNumber, record.ObservedTimeUnixNano)
		}
		body := []byte(record.Body.GetStringValue())
		audittest.Validate(t, body)
		if bytes.Contains(body, []byte(`": `)) {
			t.Errorf("record %d: the body is not compact JSON: %s", i, body)
		}
		var head struct {
			ID   string    `json:"id"`
			Time time.Time `json:"time"`
		}
		if err := json.Unmarshal(body, &head); err != nil {
			t.Fatal(err)
		}
		if head.ID != want[i].ID || record.TimeUnixNano != uint64(head.Time.UnixNano()) {
			t.Errorf("record %d: body id %s at %v, log record time %d", i, head.ID, head.Time, record.TimeUnixNano)
		}
	}
	// Run counts a round after Drain returns, so after the outbox empties.
	eventually(t, func() bool { return testutil.ToFloat64(d.Delivered) == 3 }, audit.MetricRecordsDelivered+" did not reach 3")
}

// A full batch means more may wait: the next round starts at once instead
// of after the poll interval.
func TestDrainerDrainsABacklogWithoutWaiting(t *testing.T) {
	collector := audittest.NewCollector(t)
	outbox := &memoryOutbox{rows: rows(t, 250)}
	d := testDrainer(outbox, collector, &bytes.Buffer{})
	d.PollInterval = time.Hour
	run(t, d)
	eventually(t, func() bool { return outbox.left() == 0 }, "the backlog was not drained without waiting for the poll interval")
	if got := len(collector.Records()); got != 250 {
		t.Fatalf("delivered %d records, want 250", got)
	}
	if outbox.limits[0] != audit.DefaultBatchSize || len(collector.Accepted()) != 3 {
		t.Fatalf("limits %v, %d requests: want batches of %d in 3 requests", outbox.limits, len(collector.Accepted()), audit.DefaultBatchSize)
	}
}

// While the Collector fails, the records stay in the outbox and are sent
// again, with one warning, not one per attempt; once it accepts, they leave
// the outbox and the recovery is logged.
func TestDrainerKeepsRecordsWhileTheCollectorFails(t *testing.T) {
	for name, status := range map[string]int{
		"unavailable": http.StatusServiceUnavailable,
		"rejected":    http.StatusBadRequest,
	} {
		t.Run(name, func(t *testing.T) {
			collector := audittest.NewCollector(t)
			const failures = 20
			collector.Answer(func(n int) int {
				if n <= failures {
					return status
				}
				return http.StatusOK
			})
			outbox := &memoryOutbox{rows: rows(t, 2)}
			var logs syncBuffer
			d := testDrainer(outbox, collector, &bytes.Buffer{})
			d.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			run(t, d)
			eventually(t, func() bool { return collector.Attempts() >= failures/2 }, "the drainer did not retry")
			if outbox.left() != 2 {
				t.Fatalf("%d records left the outbox while the Collector failed", 2-outbox.left())
			}
			eventually(t, func() bool { return outbox.left() == 0 }, "the records did not leave the outbox once the Collector accepted them")
			if got := len(collector.Records()); got != 2 {
				t.Fatalf("delivered %d records, want 2", got)
			}
			// Run logs the recovery and counts the round after Drain
			// returns, so after the outbox empties.
			eventually(t, func() bool { return strings.Contains(logs.String(), "audit: delivery resumed") }, "the recovery was not logged")
			if text := logs.String(); strings.Count(text, "audit: delivery failed") != 1 {
				t.Fatalf("logs: want one failure:\n%s", text)
			}
			eventually(t, func() bool { return testutil.ToFloat64(d.Delivered) == 2 }, audit.MetricRecordsDelivered+" did not reach 2")
		})
	}
}

// The wait after failed rounds doubles up to MaxBackoff.
func TestDrainerBacksOff(t *testing.T) {
	collector := audittest.NewCollector(t)
	collector.Answer(func(int) int { return http.StatusBadRequest })
	outbox := &memoryOutbox{rows: rows(t, 1)}
	d := testDrainer(outbox, collector, &bytes.Buffer{})
	d.MinBackoff, d.MaxBackoff = 20*time.Millisecond, 40*time.Millisecond
	run(t, d)
	time.Sleep(300 * time.Millisecond)
	// 20 + 40 + 40 ... ms: about 8 rounds in 300 ms; without the backoff,
	// hundreds. A busy machine runs fewer, never more.
	if got := collector.Attempts(); got < 2 || got > 12 {
		t.Fatalf("%d attempts in 300 ms, want about 8 and at most 12", got)
	}
}

// An empty outbox is polled every PollInterval, not in a busy loop.
func TestDrainerPollsAnEmptyOutbox(t *testing.T) {
	outbox := &memoryOutbox{}
	d := testDrainer(outbox, audittest.NewCollector(t), &bytes.Buffer{})
	d.PollInterval = 50 * time.Millisecond
	run(t, d)
	time.Sleep(260 * time.Millisecond)
	outbox.mu.Lock()
	rounds := outbox.rounds
	outbox.mu.Unlock()
	// About 6; a busy machine runs fewer, never more.
	if rounds < 2 || rounds > 7 {
		t.Fatalf("%d rounds in 260 ms with a 50 ms poll interval, want about 6 and at most 7", rounds)
	}
}

// Run returns when its context ends, even with an export in flight that
// the Collector holds; the records stay for a later round.
func TestDrainerStopsWhenItsContextEnds(t *testing.T) {
	collector := audittest.NewCollector(t)
	hold := make(chan struct{})
	reached := make(chan struct{}, 1)
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case reached <- struct{}{}:
		default:
		}
		select {
		case <-hold:
		case <-r.Context().Done():
		}
	})
	server := newServer(t, slow)
	// Before the server closes, which waits for the handler.
	t.Cleanup(func() { close(hold) })
	outbox := &memoryOutbox{rows: rows(t, 1)}
	d := testDrainer(outbox, collector, &bytes.Buffer{})
	d.Endpoint = server
	d.ExportTimeout = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.Run(ctx)
	}()
	<-reached
	began := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
	if elapsed := time.Since(began); elapsed > time.Second {
		t.Fatalf("Run took %v to stop", elapsed)
	}
	if outbox.left() != 1 {
		t.Fatal("the record of the abandoned export left the outbox")
	}
}
