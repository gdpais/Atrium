package main

import (
	"context"
	"fmt"
	"io"

	"heartbeat/apps/api/internal/inventory"
	"heartbeat/apps/api/internal/store"
)

// renderAssignments runs `api probe-assignments render`: it writes the
// values file that holds managedCollectors, rendered from the approved
// probe assignments (ADR 0014 §5), to stdout. It reads PostgreSQL only and
// changes nothing; `make kind-assignments` runs it in the API's pod, so
// that kind needs no login and the API no access to the host.
func renderAssignments(ctx context.Context, stdout io.Writer) error {
	cfg, resolver, err := loadMigrateConfig()
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	conn, err := store.Connect(ctx, cfg, resolver)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	want, err := store.LatestVersion()
	if err != nil {
		return err
	}
	if err := (store.SchemaChecker{Queries: store.New(conn), Want: want}).Check(ctx); err != nil {
		return err
	}
	file, err := inventory.New(conn).RenderProbeAssignments(ctx)
	if err != nil {
		return fmt.Errorf("probe-assignments render: %w", err)
	}
	_, err = stdout.Write(file)
	return err
}
