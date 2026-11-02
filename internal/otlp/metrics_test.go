package otlp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
)

func TestClientMetricsCountEachAttempt(t *testing.T) {
	statuses := []int{http.StatusServiceUnavailable, http.StatusOK, http.StatusBadRequest}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(statuses[0])
		statuses = statuses[1:]
	}))
	defer server.Close()

	metrics := NewMetrics()
	registry := prometheus.NewRegistry()
	for _, collector := range metrics.Collectors() {
		if err := registry.Register(collector); err != nil {
			t.Fatal(err)
		}
	}
	client := NewClient()
	client.InitialBackoff, client.MaxBackoff = time.Millisecond, time.Millisecond
	client.Metrics = metrics.Client("test")

	logs := []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{SeverityText: "INFO"}}}}}}
	if err := client.Export(context.Background(), server.URL, logs); err != nil {
		t.Fatal(err)
	}
	if err := client.Export(context.Background(), server.URL, logs); err == nil {
		t.Fatal("a 400 must fail the export")
	}

	want := `
# HELP heartbeat_otlp_export_attempts_total OTLP/HTTP export attempts, by client and outcome (accepted, retryable, rejected).
# TYPE heartbeat_otlp_export_attempts_total counter
heartbeat_otlp_export_attempts_total{client="test",outcome="accepted"} 1
heartbeat_otlp_export_attempts_total{client="test",outcome="rejected"} 1
heartbeat_otlp_export_attempts_total{client="test",outcome="retryable"} 1
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(want), MetricExportAttempts); err != nil {
		t.Error(err)
	}
	if got := testutil.ToFloat64(metrics.bytes.WithLabelValues("test")); got <= 0 {
		t.Errorf("accepted bytes = %v, want some", got)
	}
	if got, err := testutil.GatherAndCount(registry, MetricExportAttemptDuration); err != nil || got != 1 {
		t.Errorf("%s has %d series (%v), want 1", MetricExportAttemptDuration, got, err)
	}
}

func TestNilMetricsRecordNothing(t *testing.T) {
	var metrics *Metrics
	client := metrics.Client("x")
	if client != nil {
		t.Fatal("a nil Metrics must give a nil ClientMetrics")
	}
	client.observe(nil, time.Second, 1)
	client.rejectRecords(1)
}

func TestClientMetricsStartAtZero(t *testing.T) {
	metrics := NewMetrics()
	metrics.Client("c")
	if got := testutil.CollectAndCount(metrics.attempts); got != len(exportOutcomes) {
		t.Errorf("%d attempt series, want one per outcome at 0", got)
	}
	if got := testutil.CollectAndCount(metrics.rejected); got != 1 {
		t.Errorf("%d rejected-record series, want one at 0", got)
	}
}

// Shutdown cancels the attempts in flight; that is not the receiver's doing.
func TestCancelledAttemptsAreNotCounted(t *testing.T) {
	metrics := NewMetrics()
	client := metrics.Client("c")
	client.observe(context.Canceled, time.Second, 10)
	client.observe(fmt.Errorf("post: %w", context.Canceled), time.Second, 10)
	if got := testutil.ToFloat64(metrics.attempts.WithLabelValues("c", outcomeRetryable)); got != 0 {
		t.Errorf("retryable = %v, want 0", got)
	}
	client.observe(context.DeadlineExceeded, time.Second, 10)
	if got := testutil.ToFloat64(metrics.attempts.WithLabelValues("c", outcomeRetryable)); got != 1 {
		t.Errorf("a timed-out attempt is retryable: got %v", got)
	}
}
