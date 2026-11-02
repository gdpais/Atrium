package redis

import (
	"context"
	"errors"
	"testing"

	goredis "github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestTracingHook(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter)))
	var answer error
	process := tracingHook{}.ProcessHook(func(context.Context, goredis.Cmder) error { return answer })
	cmd := goredis.NewStringCmd(context.Background(), "get", "session:secret-id")

	if err := process(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	if got := exporter.GetSpans(); len(got) != 0 {
		t.Fatalf("a command outside a span started %d spans", len(got))
	}

	ctx, parent := tracer.Start(context.Background(), "request")
	defer parent.End()
	answer = errors.New("down")
	if err := process(ctx, cmd); !errors.Is(err, answer) {
		t.Fatalf("err = %v", err)
	}
	answer = goredis.Nil
	_ = process(ctx, cmd)
	got := exporter.GetSpans()
	if len(got) != 2 || got[0].Name != "redis.get" {
		t.Fatalf("spans = %v", got)
	}
	if got[0].Status.Code != codes.Error {
		t.Errorf("a failed command has status %v", got[0].Status)
	}
	if got[1].Status.Code == codes.Error {
		t.Error("a missing key is an answer, not an error")
	}
	for _, attr := range got[0].Attributes {
		if attr.Value.AsString() == "session:secret-id" {
			t.Errorf("attribute %s holds a command argument", attr.Key)
		}
	}
}
