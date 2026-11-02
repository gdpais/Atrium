package otlp

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// receiver is a fake OTLP/HTTP logs endpoint.  It decodes every request
// and answers with the statuses in script, then 200.
type receiver struct {
	t      *testing.T
	mu     sync.Mutex
	script []int
	// header is set on scripted answers.
	header   http.Header
	requests []*logspb.LogsData
	attempts int
	// body is written on success.
	body []byte
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempts++
	if req.Method != http.MethodPost || req.URL.Path != "/v1/logs" {
		r.t.Errorf("got %s %s, want POST /v1/logs", req.Method, req.URL.Path)
	}
	if got := req.Header.Get("Content-Type"); got != "application/x-protobuf" {
		r.t.Errorf("Content-Type = %q", got)
	}
	if len(r.script) > 0 {
		status := r.script[0]
		r.script = r.script[1:]
		for key, values := range r.header {
			w.Header()[key] = values
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(status)
		fmt.Fprintf(w, "scripted %d", status)
		return
	}
	data, err := decodeRequest(req)
	if err != nil {
		r.t.Errorf("decode request: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	r.requests = append(r.requests, data)
	w.Header().Set("Content-Type", "application/x-protobuf")
	_, _ = w.Write(r.body)
}

func decodeRequest(req *http.Request) (*logspb.LogsData, error) {
	var body io.Reader = req.Body
	if req.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(req.Body)
		if err != nil {
			return nil, err
		}
		body = zr
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	var data logspb.LogsData
	return &data, proto.Unmarshal(raw, &data)
}

func (r *receiver) records() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var bodies []string
	for _, request := range r.requests {
		for _, rl := range request.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				for _, record := range sl.LogRecords {
					bodies = append(bodies, record.Body.GetStringValue())
				}
			}
		}
	}
	return bodies
}

// testClient returns a Client that does not really sleep and records waits.
func testClient(waits *[]time.Duration) *Client {
	c := NewClient()
	c.sleep = func(ctx context.Context, d time.Duration) bool {
		*waits = append(*waits, d)
		return ctx.Err() == nil
	}
	return c
}

func logs(service string, bodies ...string) *logspb.ResourceLogs {
	var records []*logspb.LogRecord
	for _, body := range bodies {
		records = append(records, &logspb.LogRecord{Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: body}}})
	}
	return &logspb.ResourceLogs{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{
			Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: service}},
		}}},
		ScopeLogs: []*logspb.ScopeLogs{{Scope: &commonpb.InstrumentationScope{Name: "test"}, LogRecords: records}},
	}
}

func TestExportDeliversRecords(t *testing.T) {
	rcv := &receiver{t: t}
	server := httptest.NewServer(rcv)
	defer server.Close()
	var waits []time.Duration
	if err := testClient(&waits).Export(context.Background(), server.URL+"/", []*logspb.ResourceLogs{logs("a", "1", "2"), logs("b", "3")}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if got := strings.Join(rcv.records(), ","); got != "1,2,3" {
		t.Fatalf("received %s", got)
	}
	if rcv.attempts != 1 || len(waits) != 0 {
		t.Fatalf("attempts %d, waits %v; want one request", rcv.attempts, waits)
	}
}

func TestExportRetriesTransientFailures(t *testing.T) {
	rcv := &receiver{t: t, script: []int{http.StatusServiceUnavailable, http.StatusTooManyRequests, http.StatusBadGateway}, header: http.Header{"Retry-After": {"3"}}}
	server := httptest.NewServer(rcv)
	defer server.Close()
	var waits []time.Duration
	if err := testClient(&waits).Export(context.Background(), server.URL, []*logspb.ResourceLogs{logs("a", "1")}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if got := strings.Join(rcv.records(), ","); got != "1" {
		t.Fatalf("received %q", got)
	}
	// Backoff 250ms, 500ms, 1s, each raised to Retry-After (3s).
	if want := []time.Duration{3 * time.Second, 3 * time.Second, 3 * time.Second}; fmt.Sprint(waits) != fmt.Sprint(want) {
		t.Fatalf("waits = %v, want %v", waits, want)
	}
}

// By default a request is retried until it is accepted: a busy Collector is
// waited for.  The backoff doubles up to MaxBackoff.
func TestExportRetriesUntilAccepted(t *testing.T) {
	rcv := &receiver{t: t, script: []int{503, 503, 503, 503, 503, 503, 503, 503}}
	server := httptest.NewServer(rcv)
	defer server.Close()
	var waits []time.Duration
	var observed []string
	ctx := WithRetryObserver(context.Background(), func(attempt int, err error) {
		observed = append(observed, fmt.Sprintf("%d:%v", attempt, err))
	})
	if err := testClient(&waits).Export(ctx, server.URL, []*logspb.ResourceLogs{logs("a", "1")}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(observed) != 8 || !strings.Contains(observed[0], "1:receiver answered 503") {
		t.Fatalf("observed %v, want each of the 8 retried attempts", observed)
	}
	if got := strings.Join(rcv.records(), ","); got != "1" || rcv.attempts != 9 {
		t.Fatalf("received %q in %d attempts", got, rcv.attempts)
	}
	want := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second, 5 * time.Second}
	if fmt.Sprint(waits) != fmt.Sprint(want) {
		t.Fatalf("waits = %v, want %v", waits, want)
	}
}

// An attempt that timed out waiting for a busy Collector is retried at once,
// on the same request.
func TestExportRetriesATimedOutAttemptAtOnce(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	attempts, delivered := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		attempts++
		first := attempts == 1
		mu.Unlock()
		if first {
			// The first attempt waits until the client gives up.
			<-release
			return
		}
		if _, err := decodeRequest(req); err != nil {
			t.Errorf("decode: %v", err)
		}
		mu.Lock()
		delivered++
		mu.Unlock()
	}))
	defer server.Close()
	defer close(release)
	var waits []time.Duration
	client := testClient(&waits)
	client.HTTP.Timeout = 50 * time.Millisecond
	if err := client.Export(context.Background(), server.URL, []*logspb.ResourceLogs{logs("a", "1")}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 2 || delivered != 1 || len(waits) != 0 {
		t.Fatalf("attempts %d, delivered %d, waits %v; want a second attempt at once", attempts, delivered, waits)
	}
}

func TestExportGivesUpAfterMaxAttempts(t *testing.T) {
	rcv := &receiver{t: t, script: []int{500, 500, 500, 500, 500}}
	server := httptest.NewServer(rcv)
	defer server.Close()
	var waits []time.Duration
	client := testClient(&waits)
	client.MaxAttempts = 4
	err := client.Export(context.Background(), server.URL, []*logspb.ResourceLogs{logs("a", "1")})
	if err == nil || !strings.Contains(err.Error(), "attempt 4 of 4") || !strings.Contains(err.Error(), "500 Internal Server Error: scripted 500") {
		t.Fatalf("Export error = %v", err)
	}
	if want := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second}; fmt.Sprint(waits) != fmt.Sprint(want) {
		t.Fatalf("waits = %v, want %v", waits, want)
	}
}

func TestExportDoesNotRetryRejectedRequests(t *testing.T) {
	rcv := &receiver{t: t, script: []int{http.StatusBadRequest}}
	server := httptest.NewServer(rcv)
	defer server.Close()
	var waits []time.Duration
	err := testClient(&waits).Export(context.Background(), server.URL, []*logspb.ResourceLogs{logs("a", "1")})
	if err == nil || !strings.Contains(err.Error(), "rejected the request: 400 Bad Request") {
		t.Fatalf("Export error = %v", err)
	}
	if rcv.attempts != 1 {
		t.Fatalf("attempts = %d, want 1", rcv.attempts)
	}
}

func TestExportRetriesNetworkErrors(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	url := server.URL
	server.Close()
	var waits []time.Duration
	client := testClient(&waits)
	client.MaxAttempts = 4
	err := client.Export(context.Background(), url, []*logspb.ResourceLogs{logs("a", "1")})
	if err == nil || len(waits) != 3 {
		t.Fatalf("Export error = %v after waits %v; want 4 failed attempts", err, waits)
	}
}

func TestExportStopsWhenTheContextEnds(t *testing.T) {
	rcv := &receiver{t: t, script: []int{503, 503}}
	server := httptest.NewServer(rcv)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	client := NewClient()
	client.sleep = func(context.Context, time.Duration) bool { cancel(); return false }
	err := client.Export(ctx, server.URL, []*logspb.ResourceLogs{logs("a", "1")})
	if err == nil || !strings.Contains(err.Error(), "context canceled") || rcv.attempts != 1 {
		t.Fatalf("Export error = %v after %d attempts", err, rcv.attempts)
	}
}

// A partial success is delivered (nothing to retry) and logged.
func TestExportLogsPartialSuccess(t *testing.T) {
	var partial []byte
	partial = protowire.AppendTag(partial, 1, protowire.VarintType)
	partial = protowire.AppendVarint(partial, 2)
	partial = protowire.AppendTag(partial, 2, protowire.BytesType)
	partial = protowire.AppendString(partial, "too old")
	var body []byte
	body = protowire.AppendTag(body, 1, protowire.BytesType)
	body = protowire.AppendBytes(body, partial)

	rcv := &receiver{t: t, body: body}
	server := httptest.NewServer(rcv)
	defer server.Close()
	var logged bytes.Buffer
	var waits []time.Duration
	client := testClient(&waits)
	client.Logger = slog.New(slog.NewTextHandler(&logged, nil))
	if err := client.Export(context.Background(), server.URL, []*logspb.ResourceLogs{logs("a", "1", "2")}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if !strings.Contains(logged.String(), "rejected=2") || !strings.Contains(logged.String(), `message="too old"`) {
		t.Fatalf("log = %q", logged.String())
	}
}

func TestSplitBoundsRequestSize(t *testing.T) {
	long := strings.Repeat("x", 1000)
	in := []*logspb.ResourceLogs{logs("a", "a1-"+long, "a2-"+long, "a3-"+long), logs("b", "b1-"+long, "b2-"+long)}
	limit := 2500
	requests := Split(in, limit)
	if len(requests) != 3 {
		t.Fatalf("got %d requests, want 3", len(requests))
	}
	var order []string
	for i, request := range requests {
		if size := proto.Size(&logspb.LogsData{ResourceLogs: request}); size > limit {
			t.Errorf("request %d is %d bytes, above %d", i, size, limit)
		}
		for _, rl := range request {
			service := rl.Resource.Attributes[0].Value.GetStringValue()
			for _, sl := range rl.ScopeLogs {
				if sl.Scope.GetName() != "test" {
					t.Errorf("request %d lost the scope", i)
				}
				for _, record := range sl.LogRecords {
					body := record.Body.GetStringValue()
					if body[:1] != service {
						t.Errorf("record %s moved to resource %s", body[:2], service)
					}
					order = append(order, body[:2])
				}
			}
		}
	}
	if got := strings.Join(order, ","); got != "a1,a2,a3,b1,b2" {
		t.Fatalf("order = %s", got)
	}
	// A record above the limit is sent alone rather than dropped.
	if requests := Split([]*logspb.ResourceLogs{logs("a", long, long)}, 100); len(requests) != 2 {
		t.Fatalf("oversized records: %d requests, want 2", len(requests))
	}
}
