//go:build integration

package inventory_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"heartbeat/apps/api/internal/inventory"
)

// What an application's or a target's telemetry is labelled with, for
// deep links: its environment's slug, and its sources' application_names
// or its name.
func TestTelemetry(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	prod := f.environment(t, "prod-links")
	shop, bare := f.application(t, prod.ID, "Shop"), f.application(t, prod.ID, "Bare")
	if _, err := f.svc.CreateSource(ctx, operator, outsystems(shop.ID, "agentless", "ShopOS")); err != nil {
		t.Fatal(err)
	}
	cloud := outsystems(shop.ID, "otlp", "ShopCloud")
	cloud.Status = inventory.StatusDisabled
	if _, err := f.svc.CreateSource(ctx, operator, cloud); err != nil {
		t.Fatal(err)
	}

	got, err := f.svc.ApplicationTelemetry(ctx, shop.ID)
	if err != nil || got.EnvironmentSlug != "prod-links" || !slices.Equal(got.ApplicationNames, []string{"ShopCloud", "ShopOS"}) || got.TargetName != "" {
		t.Errorf("ApplicationTelemetry(Shop) = %+v, %v; want prod-links, both names (a disabled source still names its telemetry)", got, err)
	}
	got, err = f.svc.ApplicationTelemetry(ctx, bare.ID)
	if err != nil || got.EnvironmentSlug != "prod-links" || len(got.ApplicationNames) != 0 {
		t.Errorf("ApplicationTelemetry(Bare) = %+v, %v; want the environment and no names", got, err)
	}

	target := f.target(t, prod.ID, "finance-links")
	got, err = f.svc.DatabaseTargetTelemetry(ctx, target.ID)
	if err != nil || got.EnvironmentSlug != "prod-links" || got.TargetName != "finance-links" || len(got.ApplicationNames) != 0 {
		t.Errorf("DatabaseTargetTelemetry = %+v, %v", got, err)
	}

	for name, lookup := range map[string]func(context.Context, uuid.UUID) (inventory.Telemetry, error){
		"application": f.svc.ApplicationTelemetry, "target": f.svc.DatabaseTargetTelemetry,
	} {
		if _, err := lookup(ctx, uuid.New()); !errors.Is(err, inventory.ErrNotFound) {
			t.Errorf("an unknown %s: %v, want ErrNotFound", name, err)
		}
	}
}
