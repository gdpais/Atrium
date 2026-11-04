package inventory

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"heartbeat/apps/api/internal/store"
)

// Telemetry is what an entity's telemetry carries to say it is the
// entity's, for deep links [4.9] and later investigations: the environment's
// slug (deployment.environment.name, the environment label), and an
// application's application_names (service.name, ADR 0017) or a database
// target's name (the target label).
type Telemetry struct {
	EnvironmentSlug string
	// ApplicationNames are the distinct application_names of an
	// application's sources, active or disabled, sorted.
	ApplicationNames []string
	// TargetName is a database target's name.
	TargetName string
}

// sourcesPage is how many sources ApplicationTelemetry reads at a time.
const sourcesPage = 200

// ApplicationTelemetry returns the environment slug of the application
// with id and the application_names of its sources, or ErrNotFound. It
// does not ask the collector whether they are observed.
func (s *Service) ApplicationTelemetry(ctx context.Context, id uuid.UUID) (Telemetry, error) {
	q := store.New(s.db)
	app, err := q.GetApplication(ctx, pgUUID(id))
	if err != nil {
		return Telemetry{}, notFound(err)
	}
	env, err := q.GetEnvironment(ctx, app.EnvironmentID)
	if err != nil {
		return Telemetry{}, fmt.Errorf("the application's environment: %w", err)
	}
	out := Telemetry{EnvironmentSlug: env.Slug}
	params := store.ListSourcesParams{ApplicationID: pgUUID(id), RowLimit: sourcesPage}
	for {
		rows, err := q.ListSources(ctx, params)
		if err != nil {
			return Telemetry{}, fmt.Errorf("list the application's sources: %w", err)
		}
		for _, row := range rows {
			if name, ok := applicationName(row.Config); ok {
				out.ApplicationNames = append(out.ApplicationNames, name)
			}
		}
		if len(rows) < sourcesPage {
			break
		}
		last := rows[len(rows)-1]
		name, _ := applicationName(last.Config)
		params.AfterName = pgtype.Text{String: name, Valid: true}
		params.AfterID = last.ID
	}
	slices.Sort(out.ApplicationNames)
	out.ApplicationNames = slices.Compact(out.ApplicationNames)
	return out, nil
}

// DatabaseTargetTelemetry returns the environment slug and the name of the
// database target with id, or ErrNotFound.
func (s *Service) DatabaseTargetTelemetry(ctx context.Context, id uuid.UUID) (Telemetry, error) {
	q := store.New(s.db)
	target, err := q.GetDatabaseTarget(ctx, pgUUID(id))
	if err != nil {
		return Telemetry{}, notFound(err)
	}
	env, err := q.GetEnvironment(ctx, target.EnvironmentID)
	if err != nil {
		return Telemetry{}, fmt.Errorf("the target's environment: %w", err)
	}
	return Telemetry{EnvironmentSlug: env.Slug, TargetName: target.Name}, nil
}
