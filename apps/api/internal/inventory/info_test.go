package inventory

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"atrium/apps/api/internal/store"
)

// snapshotCollector emits a fixed snapshot, as Collect would.
type snapshotCollector struct{ snap infoSnapshot }

func (c snapshotCollector) Describe(ch chan<- *prometheus.Desc) { infoCollector{}.Describe(ch) }
func (c snapshotCollector) Collect(ch chan<- prometheus.Metric) { c.snap.emit(ch) }

// Two sources with the same labels would fail the whole scrape; the second
// is dropped and the scrape succeeds.
func TestInfoSeriesWithTheSameLabelsAreSentOnce(t *testing.T) {
	row := store.SourceInfoRow{EnvironmentSlug: "prod", Application: "Claims", SourceType: "outsystems",
		IngestMode: "agentless", ApplicationName: "Claims", Status: "disabled", ManagedBy: "api"}
	registry := prometheus.NewRegistry()
	registry.MustRegister(snapshotCollector{infoSnapshot{
		sources:      []store.SourceInfoRow{row, row},
		observations: []string{Observed, Observed},
		truncated:    map[string]bool{},
	}})
	want := `
# HELP atrium_api_inventory_source_info A telemetry source of the inventory, and whether the collector observes it; always 1.
# TYPE atrium_api_inventory_source_info gauge
atrium_api_inventory_source_info{application="Claims",application_name="Claims",environment="prod",ingest_mode="agentless",managed_by="api",observation="observed",source_type="outsystems",status="disabled"} 1
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(want), MetricSourceInfo); err != nil {
		t.Fatal(err)
	}
}

// A label value the client refuses leaves its row out, and the scrape
// works.
func TestInfoSeriesWithAnInvalidLabelAreLeftOut(t *testing.T) {
	registry := prometheus.NewRegistry()
	registry.MustRegister(snapshotCollector{infoSnapshot{
		environments: []store.EnvironmentInfoRow{
			{Slug: "prod", Type: "production", Status: "active", ManagedBy: "config"},
			{Slug: "bad\xff", Type: "test", Status: "active", ManagedBy: "api"},
		},
		truncated: map[string]bool{},
	}})
	want := `
# HELP atrium_api_inventory_environment_info An environment of the inventory; always 1.
# TYPE atrium_api_inventory_environment_info gauge
atrium_api_inventory_environment_info{environment="prod",managed_by="config",status="active",type="production"} 1
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(want), MetricEnvironmentInfo); err != nil {
		t.Fatal(err)
	}
}

func TestCutKeepsMaxInfoSeries(t *testing.T) {
	for _, tc := range []struct {
		rows, want int
		truncated  bool
	}{
		{0, 0, false},
		{MaxInfoSeries, MaxInfoSeries, false},
		{MaxInfoSeries + 1, MaxInfoSeries, true},
	} {
		truncated := map[string]bool{}
		got := cut(make([]int, tc.rows), kindSource, truncated)
		if len(got) != tc.want || truncated[kindSource] != tc.truncated {
			t.Errorf("cut(%d rows) = %d rows, truncated %v; want %d, %v", tc.rows, len(got), truncated[kindSource], tc.want, tc.truncated)
		}
	}
}
