//go:build integration

package inventory_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"heartbeat/apps/api/internal/collectors"
	"heartbeat/apps/api/internal/inventory"
)

// unreachable is a collector that cannot be asked.
type unreachable struct{}

func (unreachable) OutSystemsLogs(context.Context) (collectors.Coverage, error) {
	return collectors.Coverage{}, errors.New("collector: connection refused")
}

var infoMetrics = []string{
	inventory.MetricEnvironmentInfo, inventory.MetricApplicationInfo, inventory.MetricSourceInfo,
	inventory.MetricDatabaseTargetInfo, inventory.MetricInfoTruncated,
}

// The info metrics list every row with its state, whoever wrote it, as
// PostgreSQL holds it at the scrape: a change shows at the next scrape.
func TestInfoMetricsListTheInventory(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	prod, staging := f.environment(t, "prod"), f.environment(t, "staging")
	claims, billing := f.application(t, prod.ID, "Claims"), f.application(t, prod.ID, "Billing")
	stagingClaims := f.application(t, staging.ID, "Claims")
	for _, s := range []inventory.NewSource{
		outsystems(claims.ID, "agentless", "Claims"),
		outsystems(billing.ID, "otlp", "BillingODC"),
		outsystems(stagingClaims.ID, "agentless", "Claims"),
	} {
		if _, err := f.svc.CreateSource(ctx, operator, s); err != nil {
			t.Fatal(err)
		}
	}
	f.target(t, prod.ID, "claims-db")
	f.manage(t, "applications", billing.ID)
	f.manage(t, "environments", prod.ID)
	if _, err := f.svc.UpdateApplication(ctx, operator, stagingClaims.ID, inventory.ApplicationChange{Status: ptr(inventory.StatusDisabled)}); err != nil {
		t.Fatal(err)
	}

	registry := prometheus.NewRegistry()
	registry.MustRegister(inventory.New(f.pool, inventory.WithObserver(watching{"prod": {"oslogs-prod"}})).InfoCollector())
	want := `
# HELP heartbeat_api_inventory_application_info An application of the inventory; always 1.
# TYPE heartbeat_api_inventory_application_info gauge
heartbeat_api_inventory_application_info{application="Billing",criticality="medium",environment="prod",managed_by="config",platform="outsystems_traditional",status="active"} 1
heartbeat_api_inventory_application_info{application="Claims",criticality="medium",environment="prod",managed_by="api",platform="outsystems_traditional",status="active"} 1
heartbeat_api_inventory_application_info{application="Claims",criticality="medium",environment="staging",managed_by="api",platform="outsystems_traditional",status="disabled"} 1
# HELP heartbeat_api_inventory_database_target_info A database target of the inventory; always 1.
# TYPE heartbeat_api_inventory_database_target_info gauge
heartbeat_api_inventory_database_target_info{engine="sqlserver",environment="prod",managed_by="api",status="active",target="claims-db"} 1
# HELP heartbeat_api_inventory_environment_info An environment of the inventory; always 1.
# TYPE heartbeat_api_inventory_environment_info gauge
heartbeat_api_inventory_environment_info{environment="prod",managed_by="config",status="active",type="test"} 1
heartbeat_api_inventory_environment_info{environment="staging",managed_by="api",status="active",type="test"} 1
# HELP heartbeat_api_inventory_info_truncated 1 when the kind has more rows than its info metric lists.
# TYPE heartbeat_api_inventory_info_truncated gauge
heartbeat_api_inventory_info_truncated{kind="application"} 0
heartbeat_api_inventory_info_truncated{kind="database_target"} 0
heartbeat_api_inventory_info_truncated{kind="environment"} 0
heartbeat_api_inventory_info_truncated{kind="source"} 0
# HELP heartbeat_api_inventory_source_info A telemetry source of the inventory, and whether the collector observes it; always 1.
# TYPE heartbeat_api_inventory_source_info gauge
heartbeat_api_inventory_source_info{application="Billing",application_name="BillingODC",environment="prod",ingest_mode="otlp",managed_by="api",observation="not_applicable",source_type="outsystems",status="active"} 1
heartbeat_api_inventory_source_info{application="Claims",application_name="Claims",environment="prod",ingest_mode="agentless",managed_by="api",observation="observed",source_type="outsystems",status="active"} 1
heartbeat_api_inventory_source_info{application="Claims",application_name="Claims",environment="staging",ingest_mode="agentless",managed_by="api",observation="not_observed",source_type="outsystems",status="active"} 1
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(want), infoMetrics...); err != nil {
		t.Fatal(err)
	}

	// A rename shows at the next scrape: nothing is cached.
	if _, err := f.svc.UpdateApplication(ctx, operator, claims.ID, inventory.ApplicationChange{Name: ptr("Claims Portal")}); err != nil {
		t.Fatal(err)
	}
	renamed := `
# HELP heartbeat_api_inventory_application_info An application of the inventory; always 1.
# TYPE heartbeat_api_inventory_application_info gauge
heartbeat_api_inventory_application_info{application="Billing",criticality="medium",environment="prod",managed_by="config",platform="outsystems_traditional",status="active"} 1
heartbeat_api_inventory_application_info{application="Claims Portal",criticality="medium",environment="prod",managed_by="api",platform="outsystems_traditional",status="active"} 1
heartbeat_api_inventory_application_info{application="Claims",criticality="medium",environment="staging",managed_by="api",platform="outsystems_traditional",status="disabled"} 1
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(renamed), inventory.MetricApplicationInfo); err != nil {
		t.Fatal(err)
	}
}

// A collector that cannot be asked makes agentless sources unknown, and the
// scrape still lists them.
func TestInfoMetricsWithoutTheCollector(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	prod := f.environment(t, "prod")
	claims := f.application(t, prod.ID, "Claims")
	if _, err := f.svc.CreateSource(context.Background(), operator, outsystems(claims.ID, "agentless", "Claims")); err != nil {
		t.Fatal(err)
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(inventory.New(f.pool, inventory.WithObserver(unreachable{})).InfoCollector())
	want := `
# HELP heartbeat_api_inventory_source_info A telemetry source of the inventory, and whether the collector observes it; always 1.
# TYPE heartbeat_api_inventory_source_info gauge
heartbeat_api_inventory_source_info{application="Claims",application_name="Claims",environment="prod",ingest_mode="agentless",managed_by="api",observation="unknown",source_type="outsystems",status="active"} 1
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(want), inventory.MetricSourceInfo); err != nil {
		t.Fatal(err)
	}
}

// Past MaxInfoSeries rows a kind lists the first ones and says it was cut.
func TestInfoMetricsAreBounded(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if _, err := f.pool.Exec(context.Background(), `
insert into environments (name, slug, type)
select 'Env ' || i, 'env-' || lpad(i::text, 4, '0'), 'test'
from generate_series(1, $1) as i`, inventory.MaxInfoSeries+1); err != nil {
		t.Fatal(err)
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(inventory.New(f.pool).InfoCollector())
	if n, err := testutil.GatherAndCount(registry, inventory.MetricEnvironmentInfo); err != nil || n != inventory.MaxInfoSeries {
		t.Fatalf("%d environment series (%v), want %d", n, err, inventory.MaxInfoSeries)
	}
	want := `
# HELP heartbeat_api_inventory_info_truncated 1 when the kind has more rows than its info metric lists.
# TYPE heartbeat_api_inventory_info_truncated gauge
heartbeat_api_inventory_info_truncated{kind="application"} 0
heartbeat_api_inventory_info_truncated{kind="database_target"} 0
heartbeat_api_inventory_info_truncated{kind="environment"} 1
heartbeat_api_inventory_info_truncated{kind="source"} 0
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(want), inventory.MetricInfoTruncated); err != nil {
		t.Fatal(err)
	}
}
