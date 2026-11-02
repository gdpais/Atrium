package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/trace"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"heartbeat/internal/otlp"
)

// MetricLogRecords counts the log records a service tried to send over OTLP.
// Labels: outcome (see [logOutcomes]).
const MetricLogRecords = "heartbeat_telemetry_log_records_total"

// Values of the outcome label of [MetricLogRecords].
const (
	// outcomeExported is a record the Collector accepted.
	outcomeExported = "exported"
	// outcomeDroppedQueueFull is a record the queue had no room for.
	outcomeDroppedQueueFull = "dropped_queue_full"
	// outcomeDroppedExportFailed is a record whose export failed or did not
	// finish before the service stopped.
	outcomeDroppedExportFailed = "dropped_export_failed"
)

// logOutcomes lists every outcome label value, in a stable order.
var logOutcomes = []string{outcomeExported, outcomeDroppedQueueFull, outcomeDroppedExportFailed}

const (
	// maxQueuedRecords and maxQueuedBytes bound what waits in memory for the
	// Collector, so that an outage costs records instead of memory.
	maxQueuedRecords = 1024
	maxQueuedBytes   = 8 << 20
	// maxBatchRecords is the most records one export carries.
	maxBatchRecords = 256
	// flushInterval is the longest a record waits for its batch to fill.
	flushInterval = 2 * time.Second
	// exportTimeout bounds one export, and one attempt within it. Being late
	// is no reason to wait: the record is already on stderr.
	exportTimeout = 5 * time.Second
	// exportAttempts is how many times one batch is tried.
	exportAttempts = 2
	// maxValueBytes cuts a long attribute or message. The full text is on
	// stderr.
	maxValueBytes = 4 << 10
	// warnInterval limits the warnings a failing export writes to stderr.
	warnInterval = time.Minute
	// selfLogsClient is the client label of the export's OTLP metrics.
	selfLogsClient = "self-logs"
	// scopeName is the instrumentation scope of the records.
	scopeName = "heartbeat/internal/telemetry"
)

// handler writes each record to base and queues it for export. It satisfies
// [slog.Handler]; a nil queue only writes.
type handler struct {
	base   slog.Handler
	queue  *logQueue
	prefix string
	attrs  []*commonpb.KeyValue
}

// Enabled follows the stderr handler, so both destinations get the same
// records.
func (h *handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.base.Enabled(ctx, level)
}

// Handle writes r to stderr, adding the trace context when ctx has one, and
// queues it. The record is converted here rather than in the exporter's
// goroutine because a [slog.LogValuer] may not be safe to call from another
// goroutine after this call returns.
func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	span := trace.SpanContextFromContext(ctx)
	written := r
	if span.IsValid() {
		written = r.Clone()
		written.AddAttrs(slog.String("trace_id", span.TraceID().String()), slog.String("span_id", span.SpanID().String()))
	}
	err := h.base.Handle(ctx, written)
	if h.queue != nil {
		h.queue.add(h.record(r, span))
	}
	return err
}

// WithAttrs returns a handler that adds attrs to every record.
func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	next.base = h.base.WithAttrs(attrs)
	next.attrs = appendAttrs(h.attrs[:len(h.attrs):len(h.attrs)], h.prefix, attrs)
	return &next
}

// WithGroup returns a handler that qualifies later attributes with name. OTLP
// attributes here are flat, named group.key.
func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := *h
	next.base = h.base.WithGroup(name)
	next.prefix = h.prefix + name + "."
	return &next
}

// record converts r to an OTLP log record.
func (h *handler) record(r slog.Record, span trace.SpanContext) *logspb.LogRecord {
	attrs := h.attrs[:len(h.attrs):len(h.attrs)]
	r.Attrs(func(a slog.Attr) bool {
		attrs = appendAttr(attrs, h.prefix, a)
		return true
	})
	record := &logspb.LogRecord{
		ObservedTimeUnixNano: uint64(time.Now().UnixNano()),
		SeverityNumber:       severity(r.Level),
		SeverityText:         r.Level.String(),
		Body:                 stringValue(r.Message),
		Attributes:           attrs,
	}
	if !r.Time.IsZero() {
		record.TimeUnixNano = uint64(r.Time.UnixNano())
	}
	if span.IsValid() {
		traceID, spanID := span.TraceID(), span.SpanID()
		record.TraceId, record.SpanId = traceID[:], spanID[:]
		record.Flags = uint32(span.TraceFlags())
	}
	return record
}

func appendAttrs(out []*commonpb.KeyValue, prefix string, attrs []slog.Attr) []*commonpb.KeyValue {
	for _, a := range attrs {
		out = appendAttr(out, prefix, a)
	}
	return out
}

// appendAttr follows [slog.Handler]'s rules: an empty attribute and a group
// with no attributes are left out, and a group with no key is inlined. The key
// must be valid UTF-8 like a value: one bad key fails its whole batch.
func appendAttr(out []*commonpb.KeyValue, prefix string, a slog.Attr) []*commonpb.KeyValue {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return out
	}
	if a.Value.Kind() == slog.KindGroup {
		if a.Key != "" {
			prefix += a.Key + "."
		}
		return appendAttrs(out, prefix, a.Value.Group())
	}
	return append(out, &commonpb.KeyValue{Key: strings.ToValidUTF8(prefix+a.Key, "�"), Value: anyValue(a.Value)})
}

// anyValue converts a resolved, non-group value.
func anyValue(v slog.Value) *commonpb.AnyValue {
	switch v.Kind() {
	case slog.KindBool:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: v.Bool()}}
	case slog.KindInt64:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v.Int64()}}
	case slog.KindFloat64:
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: v.Float64()}}
	case slog.KindTime:
		return stringValue(v.Time().Format(time.RFC3339Nano))
	case slog.KindAny:
		return stringValue(fmt.Sprint(v.Any()))
	default:
		return stringValue(v.String())
	}
}

// stringValue returns text as a value protobuf can encode: valid UTF-8, since
// a driver error can quote anything, and cut at maxValueBytes.
func stringValue(text string) *commonpb.AnyValue {
	if len(text) > maxValueBytes {
		text = text[:maxValueBytes]
	}
	return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: strings.ToValidUTF8(text, "�")}}
}

// severity maps a slog level to OTLP's numbers, which slog's levels were
// designed to fit: INFO is 9, and every 4 levels are one named severity.
func severity(level slog.Level) logspb.SeverityNumber {
	number := int32(level) + int32(logspb.SeverityNumber_SEVERITY_NUMBER_INFO)
	number = max(number, int32(logspb.SeverityNumber_SEVERITY_NUMBER_TRACE))
	number = min(number, int32(logspb.SeverityNumber_SEVERITY_NUMBER_FATAL4))
	return logspb.SeverityNumber(number)
}

// logQueue holds records for the goroutine that exports them. Adding never
// blocks: a record without room is dropped and counted.
type logQueue struct {
	client   *otlp.Client
	endpoint string
	resource *resourcepb.Resource
	scope    *commonpb.InstrumentationScope
	records  *prometheus.CounterVec
	// diagnostics writes to stderr only. A failing export logged through the
	// service's own logger would queue more records to export.
	diagnostics *slog.Logger

	pending chan *logspb.LogRecord
	// queuedBytes is the protobuf size of the records in pending and in the
	// batch being built.
	queuedBytes atomic.Int64

	stop chan struct{}
	done chan struct{}
	// ctx ends when a shutdown runs out of time; it cancels the export in
	// flight.
	ctx      context.Context
	cancel   context.CancelFunc
	lastWarn time.Time
}

func newLogQueue(endpoint string, identity Identity, records *prometheus.CounterVec, metrics *otlp.ClientMetrics, diagnostics *slog.Logger) *logQueue {
	client := otlp.NewClient()
	client.HTTP = &http.Client{Timeout: exportTimeout}
	client.MaxAttempts = exportAttempts
	client.Logger = diagnostics
	client.Metrics = metrics
	ctx, cancel := context.WithCancel(context.Background())
	return &logQueue{
		client:      client,
		endpoint:    endpoint,
		resource:    resource(identity),
		scope:       &commonpb.InstrumentationScope{Name: scopeName},
		records:     records,
		diagnostics: diagnostics,
		pending:     make(chan *logspb.LogRecord, maxQueuedRecords),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
		ctx:         ctx,
		cancel:      cancel,
	}
}

// resource returns the OTLP resource of identity. A value that is unknown is
// left out rather than sent empty.
func resource(identity Identity) *resourcepb.Resource {
	var attrs []*commonpb.KeyValue
	for _, attr := range []struct{ key, value string }{
		{"service.namespace", Namespace},
		{"service.name", identity.Service},
		{"service.version", identity.Version},
		{"service.instance.id", identity.InstanceID},
		{environmentAttribute, identity.Environment},
	} {
		if attr.value != "" {
			attrs = append(attrs, &commonpb.KeyValue{Key: attr.key, Value: stringValue(attr.value)})
		}
	}
	return &resourcepb.Resource{Attributes: attrs}
}

// add queues record, or counts it dropped. It never blocks.
func (q *logQueue) add(record *logspb.LogRecord) {
	size := int64(proto.Size(record))
	if q.queuedBytes.Add(size) > maxQueuedBytes {
		q.queuedBytes.Add(-size)
		q.records.WithLabelValues(outcomeDroppedQueueFull).Inc()
		return
	}
	select {
	case q.pending <- record:
	default:
		q.queuedBytes.Add(-size)
		q.records.WithLabelValues(outcomeDroppedQueueFull).Inc()
	}
}

// run exports batches until stop is closed, then sends what is left.
func (q *logQueue) run() {
	defer close(q.done)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	batch := make([]*logspb.LogRecord, 0, maxBatchRecords)
	flush := func() {
		q.export(batch)
		batch = batch[:0]
	}
	for {
		select {
		case record := <-q.pending:
			batch = append(batch, record)
			if len(batch) == maxBatchRecords {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-q.stop:
			for {
				select {
				case record := <-q.pending:
					batch = append(batch, record)
					if len(batch) == maxBatchRecords {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

// export sends one batch and counts how it went.
func (q *logQueue) export(batch []*logspb.LogRecord) {
	if len(batch) == 0 {
		return
	}
	var size int64
	for _, record := range batch {
		size += int64(proto.Size(record))
	}
	defer q.queuedBytes.Add(-size)
	ctx, cancel := context.WithTimeout(q.ctx, exportTimeout)
	defer cancel()
	err := q.client.Export(ctx, q.endpoint, []*logspb.ResourceLogs{{
		Resource:  q.resource,
		ScopeLogs: []*logspb.ScopeLogs{{Scope: q.scope, LogRecords: batch}},
	}})
	if err == nil {
		q.records.WithLabelValues(outcomeExported).Add(float64(len(batch)))
		return
	}
	q.records.WithLabelValues(outcomeDroppedExportFailed).Add(float64(len(batch)))
	if time.Since(q.lastWarn) >= warnInterval {
		q.lastWarn = time.Now()
		q.diagnostics.Warn("telemetry: log export failed; records stay on stderr only", "records", len(batch), "error", err)
	}
}

// shutdown lets run send what is queued, until ctx ends.
func (q *logQueue) shutdown(ctx context.Context) {
	close(q.stop)
	select {
	case <-q.done:
	case <-ctx.Done():
		q.cancel()
		<-q.done
	}
	q.cancel()
}
