package inventory

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	api "heartbeat/apps/api"
	"heartbeat/apps/api/internal/collectors"
	configschema "heartbeat/packages/config-schema"
)

// Every source type has a schema and its ingest modes, and the spec lists
// the same types and every mode: a type added in one place only fails here.
func TestSourceTypesAgree(t *testing.T) {
	types := configschema.SourceTypes()
	if got := slices.Sorted(maps.Keys(ingestModes)); !slices.Equal(got, types) {
		t.Fatalf("ingest modes for %v, schemas for %v", got, types)
	}
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Enum []string `yaml:"enum"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(api.Spec, &doc); err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(slices.Values(doc.Components.Schemas["SourceType"].Enum)); !slices.Equal(got, types) {
		t.Errorf("spec SourceType %v, schemas %v", got, types)
	}
	for sourceType, modes := range ingestModes {
		for _, mode := range modes {
			if !slices.Contains(doc.Components.Schemas["IngestMode"].Enum, mode) {
				t.Errorf("%s takes ingest mode %s, which the spec's IngestMode lacks", sourceType, mode)
			}
		}
	}
}

func TestSourceValidation(t *testing.T) {
	ok := NewSource{ApplicationID: [16]byte{1}, SourceType: "outsystems", IngestMode: "agentless", Config: map[string]any{"application_name": "Claims"}}
	p, name, err := newSourceParams(ok)
	if err != nil || name != "Claims" || p.Status != StatusActive || string(p.Config) != `{"application_name":"Claims"}` {
		t.Fatalf("params %+v, %q, %v", p, name, err)
	}
	for field, in := range map[string]NewSource{
		"application_id": {SourceType: "outsystems", IngestMode: "agentless", Config: ok.Config},
		"source_type":    {ApplicationID: ok.ApplicationID, SourceType: "splunk", IngestMode: "agentless", Config: ok.Config},
		"ingest_mode":    {ApplicationID: ok.ApplicationID, SourceType: "outsystems", IngestMode: "file", Config: ok.Config},
		"status":         {ApplicationID: ok.ApplicationID, SourceType: "outsystems", IngestMode: "otlp", Status: "archived", Config: ok.Config},
		"config":         {ApplicationID: ok.ApplicationID, SourceType: "outsystems", IngestMode: "otlp", Config: map[string]any{"application_name": "Claims", "password": "hunter2"}},
	} {
		_, _, err := newSourceParams(in)
		var invalid *InvalidError
		if !errors.As(err, &invalid) || invalid.Field != field {
			t.Errorf("%s: %v, want an InvalidError for it", field, err)
		}
	}
	_, _, err = newSourceParams(NewSource{ApplicationID: ok.ApplicationID, SourceType: "outsystems", IngestMode: "otlp"})
	if err == nil || !strings.Contains(err.Error(), "application_name") || strings.Contains(err.Error(), "embed:///") {
		t.Errorf("no config: %v, want the missing application_name, without the schema's internal URL", err)
	}
}

// fakeObserver counts how often it is asked.
type fakeObserver struct {
	coverage collectors.Coverage
	err      error
	asked    int
}

func (f *fakeObserver) OutSystemsLogs(context.Context) (collectors.Coverage, error) {
	f.asked++
	return f.coverage, f.err
}

func TestObserve(t *testing.T) {
	at := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	sources := func() []Source {
		return []Source{
			{SourceType: SourceOutSystems, IngestMode: IngestAgentless, environmentSlug: "prod"},
			{SourceType: SourceOutSystems, IngestMode: IngestAgentless, environmentSlug: "staging"},
			{SourceType: SourceOutSystems, IngestMode: IngestOTLP, environmentSlug: "prod"},
		}
	}
	observer := &fakeObserver{coverage: collectors.Coverage{CheckedAt: at, ByEnvironment: map[string][]string{"prod": {"oslogs-prod"}}}}
	got := sources()
	New(nil, WithObserver(observer)).observe(context.Background(), got)
	if observer.asked != 1 {
		t.Fatalf("asked %d times for one page, want once", observer.asked)
	}
	if o := got[0].Observation; o.Status != Observed || !slices.Equal(o.Collectors, []string{"oslogs-prod"}) || !o.CheckedAt.Equal(at) {
		t.Errorf("prod: %+v", o)
	}
	if o := got[1].Observation; o.Status != NotObserved || len(o.Collectors) != 0 || !strings.Contains(o.Detail, `"staging"`) {
		t.Errorf("staging: %+v", o)
	}
	if o := got[2].Observation; o.Status != NotApplicable {
		t.Errorf("otlp: %+v", o)
	}

	failing := &fakeObserver{err: errors.New("the collector did not answer")}
	got = sources()
	New(nil, WithObserver(failing)).observe(context.Background(), got)
	if o := got[0].Observation; o.Status != ObservationUnknown || o.Detail != "the collector did not answer" || !o.CheckedAt.IsZero() {
		t.Errorf("collector down: %+v", o)
	}
	if failing.asked != 1 {
		t.Errorf("asked a failing collector %d times for one page, want once", failing.asked)
	}

	got = sources()
	New(nil).observe(context.Background(), got)
	if o := got[0].Observation; o.Status != ObservationUnknown || !strings.Contains(o.Detail, "HEARTBEAT_API_COLLECTOR_URL") {
		t.Errorf("no observer: %+v", o)
	}
}
