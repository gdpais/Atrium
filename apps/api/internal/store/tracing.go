package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"atrium/internal/telemetry"
)

var tracer = telemetry.Tracer("atrium/api/store")

// querySpanKey marks a context whose query span [queryTracer] started.
type querySpanKey struct{}

// queryTracer gives each query a span, inside a span that records: a readiness
// check, which has none, makes no trace of its own. The attributes hold the
// statement, which sqlc keeps free of values, and never its arguments.
type queryTracer struct{}

// TraceQueryStart implements [pgx.QueryTracer].
func (queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !trace.SpanFromContext(ctx).IsRecording() {
		return ctx
	}
	ctx, _ = tracer.Start(ctx, "db.query", trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(
		attribute.String("db.system.name", "postgresql"), attribute.String("db.query.text", data.SQL)))
	return context.WithValue(ctx, querySpanKey{}, true)
}

// TraceQueryEnd implements [pgx.QueryTracer].
func (queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if ctx.Value(querySpanKey{}) == nil {
		return
	}
	span := trace.SpanFromContext(ctx)
	if data.Err != nil {
		span.SetStatus(codes.Error, "query failed")
	}
	span.End()
}
