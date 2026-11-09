package redis

import (
	"context"
	"net"

	goredis "github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"atrium/internal/telemetry"
)

var tracer = telemetry.Tracer("atrium/api/redis")

// tracingHook gives each command a span, inside a span that records: a
// readiness check, which has none, makes no trace of its own. The attributes
// name the command and never its arguments, which hold session IDs.
type tracingHook struct{}

// DialHook implements [goredis.Hook].
func (tracingHook) DialHook(next goredis.DialHook) goredis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) { return next(ctx, network, addr) }
}

// ProcessHook implements [goredis.Hook].
func (tracingHook) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		if !trace.SpanFromContext(ctx).IsRecording() {
			return next(ctx, cmd)
		}
		ctx, span := startSpan(ctx, cmd.Name())
		defer span.End()
		err := next(ctx, cmd)
		failed(span, err)
		return err
	}
}

// ProcessPipelineHook implements [goredis.Hook].
func (tracingHook) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		if !trace.SpanFromContext(ctx).IsRecording() {
			return next(ctx, cmds)
		}
		ctx, span := startSpan(ctx, "pipeline")
		defer span.End()
		span.SetAttributes(attribute.Int("db.operation.batch.size", len(cmds)))
		err := next(ctx, cmds)
		failed(span, err)
		return err
	}
}

func startSpan(ctx context.Context, operation string) (context.Context, trace.Span) {
	return tracer.Start(ctx, "redis."+operation, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(
		attribute.String("db.system.name", "redis"), attribute.String("db.operation.name", operation)))
}

// failed marks a span, but for redis.Nil: a missing key is an answer.
func failed(span trace.Span, err error) {
	if err != nil && err != goredis.Nil {
		span.SetStatus(codes.Error, "command failed")
	}
}
