package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"

	"atrium/internal/telemetry"
)

// What a delivered record carries (ADR 0016 §3).
const (
	// ServiceName is the service.name resource attribute of every record:
	// in Loki, {service_name="atrium-api"}.
	ServiceName = "atrium-api"
	// EventName is the event.name attribute of every record.
	EventName = "atrium.audit"
	// AttributeRecordID holds the record's id: OpenTelemetry's attribute for
	// an ID that marks records with the same value as duplicates.
	AttributeRecordID = "log.record.uid"
	// OTLPClient is the client label of the delivery's OTLP metrics.
	OTLPClient = "audit"
	scopeName  = "atrium/api/audit"
)

// Drainer defaults.
const (
	// DefaultBatchSize is the most records one round claims and sends.
	DefaultBatchSize = 100
	// DefaultPollInterval is the wait after a round that emptied the outbox.
	DefaultPollInterval = 2 * time.Second
	// DefaultExportTimeout bounds one round's export, its retries included,
	// and so how long the round's rows stay locked. The OTel Collector waits
	// for Loki before it answers, so this is several of the OTLP client's
	// 10-second attempts.
	DefaultExportTimeout = 30 * time.Second
	// DefaultMinBackoff and DefaultMaxBackoff shape the wait after a failed
	// round: it doubles from the first to the second.
	DefaultMinBackoff = time.Second
	DefaultMaxBackoff = 30 * time.Second
	// warnInterval limits the warnings a failing delivery logs.
	warnInterval = time.Minute
)

// Exporter sends OTLP log records: an [otlp.Client].
type Exporter interface {
	Export(ctx context.Context, endpoint string, resourceLogs []*logspb.ResourceLogs) error
}

// Drainer sends the outbox's records to the OTel Collector, the oldest
// first, and deletes them once the Collector accepts them. Each API replica
// runs one; [Outbox.Drain] keeps two from sending the same rows at once.
type Drainer struct {
	// Outbox is where the records wait: an [Outbox].
	Outbox interface {
		Drain(ctx context.Context, limit int, deliver func(context.Context, []Row) error) (int, error)
	}
	// Exporter sends them to Endpoint, an OTLP/HTTP base URL such as
	// http://otel-collector:4318.
	Exporter Exporter
	Endpoint string
	// Resource is the records' resource; see [Resource].
	Resource *resourcepb.Resource
	// Delivered counts the records the Collector accepted. Nil counts
	// nothing.
	Delivered prometheus.Counter
	// Logger receives delivery failures and recoveries.
	Logger *slog.Logger

	BatchSize                                           int
	PollInterval, ExportTimeout, MinBackoff, MaxBackoff time.Duration
}

// NewDrainer returns a Drainer with the defaults.
func NewDrainer(outbox Outbox, exporter Exporter, endpoint string, identity telemetry.Identity, logger *slog.Logger) *Drainer {
	return &Drainer{
		Outbox:        outbox,
		Exporter:      exporter,
		Endpoint:      endpoint,
		Resource:      Resource(identity),
		Logger:        logger,
		BatchSize:     DefaultBatchSize,
		PollInterval:  DefaultPollInterval,
		ExportTimeout: DefaultExportTimeout,
		MinBackoff:    DefaultMinBackoff,
		MaxBackoff:    DefaultMaxBackoff,
	}
}

// Run drains the outbox until ctx ends: round after round while full
// batches come back, every PollInterval once it is empty, and with a
// doubling backoff while delivery fails. Rows of a failed round stay in the
// outbox, so a Collector outage delays records and loses none.
//
// When ctx ends Run stops at once: an export in flight is abandoned and its
// rows are sent again later, by this replica's successor or another one. Rows
// the Collector has already accepted are still deleted, within 5 seconds.
func (d *Drainer) Run(ctx context.Context) {
	var backoff time.Duration
	var failingSince, lastWarn time.Time
	for {
		n, err := d.Outbox.Drain(ctx, d.BatchSize, d.deliver)
		// Counted before stopping: a round can finish its delete after a
		// shutdown began.
		if err == nil && d.Delivered != nil {
			d.Delivered.Add(float64(n))
		}
		if ctx.Err() != nil {
			return
		}
		var wait time.Duration
		switch {
		case err != nil:
			backoff = min(max(2*backoff, d.MinBackoff), d.MaxBackoff)
			wait = backoff
			now := time.Now()
			if failingSince.IsZero() {
				failingSince, lastWarn = now, now
				d.Logger.Warn("audit: delivery failed; the records stay in the outbox and are sent again", "error", err, "retry_in", wait.String())
			} else if now.Sub(lastWarn) >= warnInterval {
				lastWarn = now
				d.Logger.Warn("audit: delivery still failing", "error", err, "failing_for", now.Sub(failingSince).Round(time.Second).String())
			}
		case n >= d.BatchSize:
			// More may be waiting: go on at once.
		default:
			wait = d.PollInterval
		}
		if err == nil {
			backoff = 0
			if !failingSince.IsZero() {
				d.Logger.Info("audit: delivery resumed", "failed_for", time.Since(failingSince).Round(time.Second).String())
				failingSince = time.Time{}
			}
		}
		if !sleep(ctx, wait) {
			return
		}
	}
}

// deliver sends one round's rows and returns nil once the Collector has
// accepted them all.
func (d *Drainer) deliver(ctx context.Context, rows []Row) error {
	ctx, cancel := context.WithTimeout(ctx, d.ExportTimeout)
	defer cancel()
	ctx, span := tracer.Start(ctx, "audit.export", trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.Int("audit.records", len(rows))))
	defer span.End()
	observed := uint64(time.Now().UnixNano())
	records := make([]*logspb.LogRecord, len(rows))
	for i, row := range rows {
		records[i] = logRecord(row, observed)
	}
	err := d.Exporter.Export(ctx, d.Endpoint, []*logspb.ResourceLogs{{
		Resource: d.Resource,
		ScopeLogs: []*logspb.ScopeLogs{{
			Scope:      &commonpb.InstrumentationScope{Name: scopeName},
			LogRecords: records,
		}},
	}})
	if err != nil {
		span.SetStatus(codes.Error, "export failed")
	}
	return err
}

// logRecord is row as an OTLP log record: the record as its body, at the
// record's time.
func logRecord(row Row, observed uint64) *logspb.LogRecord {
	body := row.Record
	var compact bytes.Buffer
	if json.Compact(&compact, row.Record) == nil {
		body = compact.Bytes()
	}
	record := &logspb.LogRecord{
		ObservedTimeUnixNano: observed,
		SeverityNumber:       logspb.SeverityNumber_SEVERITY_NUMBER_INFO,
		SeverityText:         "INFO",
		Body:                 &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: string(body)}},
		Attributes: []*commonpb.KeyValue{
			stringAttribute("event.name", EventName),
			stringAttribute(AttributeRecordID, row.ID),
		},
	}
	var head struct {
		Time time.Time `json:"time"`
	}
	if json.Unmarshal(row.Record, &head) == nil && !head.Time.IsZero() {
		record.TimeUnixNano = uint64(head.Time.UnixNano())
	}
	return record
}

// Resource is the records' resource: service.name atrium-api, and the
// replica's version, instance and environment from identity.
func Resource(identity telemetry.Identity) *resourcepb.Resource {
	resource := &resourcepb.Resource{}
	for _, attr := range []struct{ key, value string }{
		{"service.name", ServiceName},
		{"service.version", identity.Version},
		{"service.instance.id", identity.InstanceID},
		{"deployment.environment.name", identity.Environment},
	} {
		if attr.value != "" {
			resource.Attributes = append(resource.Attributes, stringAttribute(attr.key, attr.value))
		}
	}
	return resource
}

func stringAttribute(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}}
}

// sleep waits for d or until ctx ends, reporting whether d elapsed.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
