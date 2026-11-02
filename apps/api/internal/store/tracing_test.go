package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestQueryTracer(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter)))
	data := pgx.TraceQueryStartData{SQL: "SELECT 1 WHERE $1", Args: []any{"secret-value"}}

	// A readiness check has no span, and makes no trace of its own.
	q := queryTracer{}
	ctx := q.TraceQueryStart(context.Background(), nil, data)
	q.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})
	if got := exporter.GetSpans(); len(got) != 0 {
		t.Fatalf("a query outside a span started %d spans", len(got))
	}

	parentCtx, parent := tracer.Start(context.Background(), "request")
	ctx = q.TraceQueryStart(parentCtx, nil, data)
	q.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: errors.New("boom")})
	got := exporter.GetSpans()
	if len(got) != 1 || got[0].Name != "db.query" || got[0].Status.Code != codes.Error {
		t.Fatalf("spans = %v", got)
	}
	for _, attr := range got[0].Attributes {
		if attr.Value.AsString() == "secret-value" {
			t.Errorf("attribute %s holds a query argument", attr.Key)
		}
	}
	// The query's end must not end the request's span.
	if !parent.IsRecording() {
		t.Error("the parent span ended")
	}
	parent.End()
}
