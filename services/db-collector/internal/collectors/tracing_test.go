package collectors

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	collectorconfig "heartbeat/internal/config"
	collectorexport "heartbeat/services/db-collector/internal/export"
	collectormetadata "heartbeat/services/db-collector/internal/metadata"
)

var (
	spansOnce sync.Once
	spans     *tracetest.InMemoryExporter
)

// recordSpans keeps spans in memory. The package's tracer delegates to the
// first provider set, so all tests share one exporter.
func recordSpans(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	spansOnce.Do(func() {
		spans = tracetest.NewInMemoryExporter()
		otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(spans)))
	})
	spans.Reset()
	return spans
}

// scriptedExecutor fails the probes it is told to.
type scriptedExecutor struct{ fail map[string]error }

func (e scriptedExecutor) RunProbe(_ context.Context, item collectormetadata.ScheduledProbe) ([]collectorexport.Sample, []collectormetadata.Evidence, error) {
	return nil, nil, e.fail[item.Definition.Name]
}

func TestACycleIsOneTrace(t *testing.T) {
	exporter := recordSpans(t)
	runner := NewRunner(scriptedExecutor{fail: map[string]error{"bad": errors.New("login failed for host db.internal")}}, nil, LoggingEvidenceSink{})
	collector := collectorconfig.CollectorRuntimeConfig{
		ID: "c", Enabled: true, ScrapeInterval: time.Minute, TargetNames: []string{"t"},
		Targets: []collectorconfig.TargetRuntimeConfig{{
			Name: "t", EnvironmentSlug: "e",
			Probes: []collectorconfig.ProbeRuntimeConfig{{Name: "good"}, {Name: "bad"}},
		}},
	}
	runner.runCycle(context.Background(), collector, newTargetTracker())

	got := map[string]tracetest.SpanStub{}
	for _, span := range exporter.GetSpans() {
		key := span.Name
		for _, attr := range span.Attributes {
			if attr.Key == "heartbeat.probe" {
				key += "/" + attr.Value.AsString()
			}
		}
		got[key] = span
	}
	cycle, target, good, bad := got["collector.cycle"], got["collector.target"], got["collector.probe/good"], got["collector.probe/bad"]
	if cycle.Name == "" || target.Name == "" || good.Name == "" || bad.Name == "" {
		t.Fatalf("spans = %v", got)
	}
	if target.Parent.SpanID() != cycle.SpanContext.SpanID() || good.Parent.SpanID() != target.SpanContext.SpanID() || bad.Parent.SpanID() != target.SpanContext.SpanID() {
		t.Error("the spans do not nest cycle, target, probe")
	}
	if bad.Status.Code != codes.Error || good.Status.Code == codes.Error {
		t.Errorf("status good = %v, bad = %v", good.Status, bad.Status)
	}
	if bad.Status.Description != "probe failed" {
		t.Errorf("status %q: the error's text can name a host and a login", bad.Status.Description)
	}
}
