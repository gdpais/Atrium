package telemetry

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/slogtest"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"
)

// receiver is an OTLP/HTTP logs endpoint that keeps what it is sent.
type receiver struct {
	*httptest.Server
	mu   sync.Mutex
	logs []*logspb.ResourceLogs
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/logs" {
			t.Errorf("path = %s, want /v1/logs", req.URL.Path)
		}
		zr, err := gzip.NewReader(req.Body)
		if err != nil {
			t.Error(err)
			return
		}
		body, err := io.ReadAll(zr)
		if err != nil {
			t.Error(err)
			return
		}
		var data logspb.LogsData
		if err := proto.Unmarshal(body, &data); err != nil {
			t.Error(err)
			return
		}
		r.mu.Lock()
		r.logs = append(r.logs, data.ResourceLogs...)
		r.mu.Unlock()
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *receiver) records() []*logspb.LogRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*logspb.LogRecord
	for _, rl := range r.logs {
		for _, sl := range rl.ScopeLogs {
			out = append(out, sl.LogRecords...)
		}
	}
	return out
}

// start returns a Telemetry for the test, sending to endpoint.
func start(t *testing.T, endpoint string) (*Telemetry, *bytes.Buffer) {
	t.Helper()
	t.Setenv(EnvLogsEndpoint, endpoint)
	t.Setenv(EnvTracesEndpoint, "")
	t.Setenv(EnvSDKDisabled, "")
	t.Setenv(EnvResourceAttributes, "deployment.environment.name=test,other=x")
	t.Setenv(EnvPprofAddr, "")
	stderr := &bytes.Buffer{}
	return Start("svc", stderr), stderr
}

func shutdown(t *testing.T, tel *Telemetry) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tel.Shutdown(ctx)
}

func recordAttr(attrs []*commonpb.KeyValue, key string) *commonpb.AnyValue {
	for _, a := range attrs {
		if a.Key == key {
			return a.Value
		}
	}
	return nil
}

func TestLogsReachStderrAndTheCollector(t *testing.T) {
	r := newReceiver(t)
	tel, stderr := start(t, r.URL+"/v1/logs")
	tel.Logger.With("a", 1).WithGroup("g").Warn("hello", "b", "two", "d", 3*time.Second)
	shutdown(t, tel)

	if !strings.Contains(stderr.String(), `"msg":"hello"`) {
		t.Errorf("stderr = %q, want the record", stderr)
	}
	records := r.records()
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	record := records[0]
	if record.Body.GetStringValue() != "hello" || record.SeverityNumber != logspb.SeverityNumber_SEVERITY_NUMBER_WARN || record.TimeUnixNano == 0 {
		t.Errorf("record = %v", record)
	}
	if got := recordAttr(record.Attributes, "a").GetIntValue(); got != 1 {
		t.Errorf("a = %d, want 1", got)
	}
	if got := recordAttr(record.Attributes, "g.b").GetStringValue(); got != "two" {
		t.Errorf("g.b = %q, want two", got)
	}
	if got := recordAttr(record.Attributes, "g.d").GetStringValue(); got != "3s" {
		t.Errorf("g.d = %q, want 3s", got)
	}
	res := r.logs[0].Resource.Attributes
	for key, want := range map[string]string{
		"service.namespace":           Namespace,
		"service.name":                "svc",
		"service.version":             "dev",
		"deployment.environment.name": "test",
	} {
		if got := recordAttr(res, key).GetStringValue(); got != want {
			t.Errorf("resource %s = %q, want %q", key, got, want)
		}
	}
	if got := testutil.ToFloat64(tel.records.WithLabelValues(outcomeExported)); got != 1 {
		t.Errorf("exported = %v, want 1", got)
	}
}

func TestLogsCarryTheTraceContext(t *testing.T) {
	r := newReceiver(t)
	tel, stderr := start(t, r.URL)
	traceID := trace.TraceID{1, 2, 3}
	spanID := trace.SpanID{4, 5, 6}
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
	}))
	tel.Logger.InfoContext(ctx, "in a span")
	tel.Logger.Info("outside a span")
	shutdown(t, tel)

	records := r.records()
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2", len(records))
	}
	if !bytes.Equal(records[0].TraceId, traceID[:]) || !bytes.Equal(records[0].SpanId, spanID[:]) {
		t.Errorf("trace = %x span = %x", records[0].TraceId, records[0].SpanId)
	}
	if records[1].TraceId != nil {
		t.Errorf("record outside a span has trace ID %x", records[1].TraceId)
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if !strings.Contains(lines[0], `"trace_id":"`+traceID.String()+`"`) || strings.Contains(lines[1], "trace_id") {
		t.Errorf("stderr = %q", lines)
	}
}

func TestStringsAreCutAndValidUTF8(t *testing.T) {
	r := newReceiver(t)
	tel, stderr := start(t, r.URL)
	tel.Logger.Info("bad \xff text", "long", strings.Repeat("x", 2*maxValueBytes), "bad\xffkey", 1)
	shutdown(t, tel)

	records := r.records()
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1: invalid UTF-8 must not fail the export", len(records))
	}
	if body := records[0].Body.GetStringValue(); !utf8.ValidString(body) {
		t.Errorf("body %q is not valid UTF-8", body)
	}
	if got := len(recordAttr(records[0].Attributes, "long").GetStringValue()); got != maxValueBytes {
		t.Errorf("long is %d bytes, want %d", got, maxValueBytes)
	}
	if got := recordAttr(records[0].Attributes, "bad�key").GetIntValue(); got != 1 {
		t.Errorf("bad�key = %d, want 1", got)
	}
	if !strings.Contains(stderr.String(), strings.Repeat("x", 2*maxValueBytes)) {
		t.Error("stderr must keep the full text")
	}
}

func TestNoEndpointSendsNothing(t *testing.T) {
	tel, stderr := start(t, "")
	if tel.logs != nil {
		t.Fatal("no endpoint must start no export")
	}
	tel.Logger.Info("hello")
	shutdown(t, tel)
	if !strings.Contains(stderr.String(), "hello") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestDisabledSendsNothing(t *testing.T) {
	t.Setenv(EnvLogsEndpoint, "http://localhost:1")
	t.Setenv(EnvSDKDisabled, "true")
	if got := logsEndpoint(); got != "" {
		t.Fatalf("logsEndpoint() = %q, want none", got)
	}
}

// A Collector that never answers must not hold up the caller, and must not
// keep more than the queue's bound.
func TestAStuckCollectorDropsInsteadOfBlocking(t *testing.T) {
	release := make(chan struct{})
	stuck := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer stuck.Close()
	defer close(release)
	tel, _ := start(t, stuck.URL)

	count := 4 * maxQueuedRecords
	began := time.Now()
	for i := range count {
		tel.Logger.Info("flood", "i", i)
	}
	if elapsed := time.Since(began); elapsed > 2*time.Second {
		t.Errorf("%d log calls took %s against a stuck Collector", count, elapsed)
	}
	if got := testutil.ToFloat64(tel.records.WithLabelValues(outcomeDroppedQueueFull)); got == 0 {
		t.Error("nothing counted as dropped")
	}
	if got := tel.logs.queuedBytes.Load(); got > maxQueuedBytes {
		t.Errorf("queued %d bytes, above the bound", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	began = time.Now()
	tel.Shutdown(ctx)
	if elapsed := time.Since(began); elapsed > 5*time.Second {
		t.Errorf("Shutdown took %s past its deadline", elapsed)
	}
}

func TestAQueueOverItsBytesDrops(t *testing.T) {
	counter := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "c"}, []string{"outcome"})
	q := newLogQueue("http://localhost:1", Identity{}, counter, nil, slog.New(slog.DiscardHandler))
	// Three values at their limit: the bytes run out before the channel's places.
	value := stringValue(strings.Repeat("x", maxValueBytes))
	big := &logspb.LogRecord{Attributes: []*commonpb.KeyValue{{Key: "a", Value: value}, {Key: "b", Value: value}, {Key: "c", Value: value}}}
	for range maxQueuedRecords {
		q.add(big)
	}
	if got := q.queuedBytes.Load(); got > maxQueuedBytes {
		t.Errorf("queued %d bytes, above the bound", got)
	}
	if got := len(q.pending); got >= maxQueuedRecords {
		t.Errorf("%d records queued: the bytes bound never fired", got)
	}
	if got := testutil.ToFloat64(counter.WithLabelValues(outcomeDroppedQueueFull)); got == 0 {
		t.Error("nothing counted as dropped")
	}
}

func TestAFailedExportIsCountedAndNotLoggedToItself(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer failing.Close()
	tel, stderr := start(t, failing.URL)
	tel.Logger.Info("first")
	shutdown(t, tel)

	if got := testutil.ToFloat64(tel.records.WithLabelValues(outcomeDroppedExportFailed)); got != 1 {
		t.Errorf("dropped_export_failed = %v, want 1", got)
	}
	if got := strings.Count(stderr.String(), "log export failed"); got != 1 {
		t.Errorf("stderr has %d warnings, want 1: %q", got, stderr)
	}
	if got := testutil.ToFloat64(tel.records.WithLabelValues(outcomeExported)); got != 0 {
		t.Errorf("exported = %v, want 0", got)
	}
}

// A CPU profile holds the pprof listener for its whole duration; a shutdown
// during one must still send the last logs.
func TestABusyPprofListenerDoesNotCostTheLastLogs(t *testing.T) {
	r := newReceiver(t)
	tel, _ := start(t, r.URL)
	started, release := make(chan struct{}), make(chan struct{})
	busy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	}))
	defer busy.Close()
	defer close(release)
	tel.pprof = busy.Config
	go func() {
		if resp, err := http.Get(busy.URL); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-started

	tel.Logger.Info("last words")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	tel.Shutdown(ctx)
	if got := len(r.records()); got != 1 {
		t.Errorf("got %d records, want 1", got)
	}
}

// Traces may be lost, logs may not: spans the Collector does not take, while
// Tempo is down and its queue full, must not spend the time the last logs need.
func TestStuckSpansDoNotCostTheLastLogs(t *testing.T) {
	r := newReceiver(t)
	release := make(chan struct{})
	stuck := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer stuck.Close()
	defer close(release)
	previous := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(previous) })
	tel, _ := start(t, r.URL)
	t.Setenv(EnvTracesEndpoint, stuck.URL+"/v1/traces")
	tel.stopTraces = startTraces(tel.Identity, tel.traceErrors, slog.New(slog.DiscardHandler))
	_, span := otel.GetTracerProvider().Tracer("t").Start(context.Background(), "work")
	span.End()

	tel.Logger.Info("last words")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	tel.Shutdown(ctx)
	if got := len(r.records()); got != 1 {
		t.Errorf("got %d records, want 1", got)
	}
}

func TestRegisterExposesServiceInfo(t *testing.T) {
	tel, _ := start(t, "")
	registry := prometheus.NewRegistry()
	if err := tel.Register(registry); err != nil {
		t.Fatal(err)
	}
	want := `
# HELP heartbeat_service_info Always 1: the service, version, revision and environment that are running.
# TYPE heartbeat_service_info gauge
heartbeat_service_info{environment="test",revision="` + tel.Identity.Revision + `",service="svc",version="dev"} 1
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(want), MetricServiceInfo); err != nil {
		t.Error(err)
	}
	if got := testutil.CollectAndCount(tel.records); got != len(logOutcomes) {
		t.Errorf("%d outcome series, want %d from the start", got, len(logOutcomes))
	}
}

func TestNilTelemetryIsSafe(t *testing.T) {
	var tel *Telemetry
	if err := tel.Register(prometheus.NewRegistry()); err != nil {
		t.Fatal(err)
	}
	tel.Shutdown(context.Background())
}

func TestResourceAttribute(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"deployment.environment.name=prod", "prod"},
		{"a=b, deployment.environment.name = eu%20west ,c=d", "eu west"},
		{"deployment.environment.name=100%", "100%"},
		{"other=prod", ""},
		{"deployment.environment.name", ""},
	}
	for _, c := range cases {
		if got := resourceAttribute(c.in, environmentAttribute); got != c.want {
			t.Errorf("resourceAttribute(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLogsEndpointAcceptsBaseAndSignalURLs(t *testing.T) {
	t.Setenv(EnvSDKDisabled, "")
	for in, want := range map[string]string{
		"http://c:4319":          "http://c:4319",
		"http://c:4319/":         "http://c:4319",
		"http://c:4319/v1/logs":  "http://c:4319",
		"http://c:4319/v1/logs/": "http://c:4319",
		"":                       "",
	} {
		t.Setenv(EnvLogsEndpoint, in)
		if got := logsEndpoint(); got != want {
			t.Errorf("logsEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSeverityFollowsOTLP(t *testing.T) {
	for level, want := range map[slog.Level]logspb.SeverityNumber{
		slog.LevelDebug:  logspb.SeverityNumber_SEVERITY_NUMBER_DEBUG,
		slog.LevelInfo:   logspb.SeverityNumber_SEVERITY_NUMBER_INFO,
		slog.LevelWarn:   logspb.SeverityNumber_SEVERITY_NUMBER_WARN,
		slog.LevelError:  logspb.SeverityNumber_SEVERITY_NUMBER_ERROR,
		slog.Level(-100): logspb.SeverityNumber_SEVERITY_NUMBER_TRACE,
		slog.Level(100):  logspb.SeverityNumber_SEVERITY_NUMBER_FATAL4,
	} {
		if got := severity(level); got != want {
			t.Errorf("severity(%v) = %v, want %v", level, got, want)
		}
	}
}

// The exported records follow the slog.Handler contract, checked the way the
// standard library checks handlers.
func TestHandlerContract(t *testing.T) {
	counter := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "c"}, []string{"outcome"})
	var queue *logQueue
	var records []*logspb.LogRecord
	new := func() *handler {
		queue = newLogQueue("http://localhost:1", Identity{}, counter, nil, slog.New(slog.DiscardHandler))
		return &handler{base: slog.DiscardHandler, queue: queue}
	}
	check := func(t *testing.T) map[string]any {
		t.Helper()
		close(queue.pending)
		records = records[:0]
		for record := range queue.pending {
			records = append(records, record)
		}
		if len(records) != 1 {
			t.Fatalf("got %d records, want 1", len(records))
		}
		return toMap(records[0])
	}
	slogtest.Run(t, func(*testing.T) slog.Handler {
		return &alwaysEnabled{new()}
	}, check)
}

// alwaysEnabled lets the contract test use a base that discards.
type alwaysEnabled struct{ *handler }

func (a *alwaysEnabled) Enabled(context.Context, slog.Level) bool { return true }
func (a *alwaysEnabled) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &alwaysEnabled{a.handler.WithAttrs(attrs).(*handler)}
}
func (a *alwaysEnabled) WithGroup(name string) slog.Handler {
	return &alwaysEnabled{a.handler.WithGroup(name).(*handler)}
}

// toMap turns a record into the map slogtest expects, with flat group.key
// attributes nested again.
func toMap(record *logspb.LogRecord) map[string]any {
	out := map[string]any{
		slog.LevelKey:   record.SeverityText,
		slog.MessageKey: record.Body.GetStringValue(),
	}
	if record.TimeUnixNano != 0 {
		out[slog.TimeKey] = time.Unix(0, int64(record.TimeUnixNano))
	}
	for _, attr := range record.Attributes {
		parts := strings.Split(attr.Key, ".")
		level := out
		for _, part := range parts[:len(parts)-1] {
			next, ok := level[part].(map[string]any)
			if !ok {
				next = map[string]any{}
				level[part] = next
			}
			level = next
		}
		var value any
		switch v := attr.Value.Value.(type) {
		case *commonpb.AnyValue_IntValue:
			value = v.IntValue
		case *commonpb.AnyValue_StringValue:
			value = v.StringValue
		default:
			encoded, _ := json.Marshal(attr.Value)
			value = string(encoded)
		}
		level[parts[len(parts)-1]] = value
	}
	return out
}

func TestPprofListensOnLoopbackOnly(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	for _, addr := range []string{":0", "0.0.0.0:0", "10.0.0.1:0", "nonsense"} {
		if server := startPprof(addr, logger); server != nil {
			_ = server.Close()
			t.Errorf("startPprof(%q) listened", addr)
		}
	}
	if server := startPprof("", logger); server != nil {
		t.Error("an empty address must run no listener")
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	server := startPprof(addr, logger)
	if server == nil {
		t.Fatal("no listener")
	}
	defer server.Close()
	resp, err := http.Get("http://" + addr + "/debug/pprof/cmdline")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func BenchmarkLogCall(b *testing.B) {
	stuck := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer stuck.Close()
	b.Setenv(EnvLogsEndpoint, stuck.URL)
	b.Setenv(EnvSDKDisabled, "")
	tel := Start("svc", io.Discard)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		tel.Shutdown(ctx)
	}()
	b.ReportAllocs()
	for b.Loop() {
		tel.Logger.Info("probe failed", "collector", "c", "target", "t", "probe", "waits", "error", "timeout")
	}
}

func BenchmarkLogCallWithoutExport(b *testing.B) {
	b.Setenv(EnvLogsEndpoint, "")
	tel := Start("svc", io.Discard)
	b.ReportAllocs()
	for b.Loop() {
		tel.Logger.Info("probe failed", "collector", "c", "target", "t", "probe", "waits", "error", "timeout")
	}
}
