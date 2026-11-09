package inventory

import (
	"context"
	"fmt"
	"maps"
	"time"

	"github.com/google/uuid"

	"atrium/apps/api/internal/audit"
	"atrium/apps/api/internal/store"
)

// Environment is an environment as the API shows it, and as its audit
// records hold it.
type Environment struct {
	ID          uuid.UUID         `json:"id"`
	Name        string            `json:"name"`
	Slug        string            `json:"slug"`
	Type        string            `json:"type"`
	Description string            `json:"description"`
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// NewEnvironment is an environment to create. Status defaults to active.
type NewEnvironment struct {
	Name        string
	Slug        string
	Type        string
	Description string
	Status      string
	Labels      map[string]string
}

// EnvironmentChange holds the fields to change; nil keeps a field. The
// slug cannot change.
type EnvironmentChange struct {
	Name        *string
	Type        *string
	Description *string
	Status      *string
	Labels      *map[string]string
}

func environment(row store.Environment) (Environment, error) {
	labels, err := decodeLabels(row.Labels)
	if err != nil {
		return Environment{}, err
	}
	return Environment{
		ID: goUUID(row.ID), Name: row.Name, Slug: row.Slug, Type: row.Type,
		Description: row.Description, Status: row.Status, Labels: labels,
		CreatedAt: goTime(row.CreatedAt), UpdatedAt: goTime(row.UpdatedAt),
	}, nil
}

// ListEnvironments returns every environment, by name, or those with
// status when it is not "".
func (s *Service) ListEnvironments(ctx context.Context, status string) ([]Environment, error) {
	if status != "" {
		if _, err := oneOf("status", status, "", statuses); err != nil {
			return nil, err
		}
	}
	rows, err := store.New(s.db).ListEnvironments(ctx, pgText(status))
	if err != nil {
		return nil, fmt.Errorf("list environments: %w", err)
	}
	out := make([]Environment, len(rows))
	for i, row := range rows {
		if out[i], err = environment(row); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// GetEnvironment returns the environment with id, or ErrNotFound.
func (s *Service) GetEnvironment(ctx context.Context, id uuid.UUID) (Environment, error) {
	row, err := store.New(s.db).GetEnvironment(ctx, pgUUID(id))
	if err != nil {
		return Environment{}, notFound(err)
	}
	return environment(row)
}

// CreateEnvironment creates an environment and audits it. It fails with a
// ConflictError when another environment has the slug.
func (s *Service) CreateEnvironment(ctx context.Context, caller Caller, in NewEnvironment) (Environment, error) {
	params, err := newEnvironmentParams(in)
	if err != nil {
		return Environment{}, err
	}
	var created Environment
	err = s.change(ctx, caller, audit.EntityEnvironment, func(q *store.Queries, record func(string, string, any, any) error) error {
		row, err := q.CreateEnvironment(ctx, params)
		if constraint(err, uniqueViolation) == "environments_slug_uq" {
			return &ConflictError{Detail: fmt.Sprintf("an environment with slug %q exists", params.Slug)}
		}
		if err != nil {
			return fmt.Errorf("create the environment: %w", err)
		}
		if created, err = environment(row); err != nil {
			return err
		}
		return record(audit.ActionCreate, created.ID.String(), nil, created)
	})
	if err != nil {
		return Environment{}, err
	}
	return created, nil
}

func newEnvironmentParams(in NewEnvironment) (store.CreateEnvironmentParams, error) {
	var p store.CreateEnvironmentParams
	var err error
	if p.Name, err = name("name", in.Name); err != nil {
		return p, err
	}
	if p.Slug, err = slug(in.Slug); err != nil {
		return p, err
	}
	if p.Type, err = oneOf("type", in.Type, "", environmentTypes); err != nil {
		return p, err
	}
	if p.Description, err = text("description", in.Description, maxDescriptionRunes, true); err != nil {
		return p, err
	}
	if p.Status, err = oneOf("status", in.Status, StatusActive, statuses); err != nil {
		return p, err
	}
	l, err := labels(in.Labels)
	if err != nil {
		return p, err
	}
	p.Labels, err = encodeLabels(l)
	return p, err
}

// UpdateEnvironment changes the fields change holds and audits it, and
// returns the environment as it is then. A change that changes nothing
// writes and audits nothing. It fails with ErrNotFound for an unknown id.
func (s *Service) UpdateEnvironment(ctx context.Context, caller Caller, id uuid.UUID, change EnvironmentChange) (Environment, error) {
	var after Environment
	err := s.change(ctx, caller, audit.EntityEnvironment, func(q *store.Queries, record func(string, string, any, any) error) error {
		row, err := q.LockEnvironment(ctx, pgUUID(id))
		if err != nil {
			return notFound(err)
		}
		if err := managedByFile(row.ManagedBy, "environment"); err != nil {
			return err
		}
		before, err := environment(row)
		if err != nil {
			return err
		}
		next, err := applyEnvironmentChange(before, change)
		if err != nil {
			return err
		}
		if sameEnvironment(before, next) {
			after = before
			return nil
		}
		labels, err := encodeLabels(next.Labels)
		if err != nil {
			return err
		}
		row, err = q.UpdateEnvironment(ctx, store.UpdateEnvironmentParams{
			ID: row.ID, Name: next.Name, Type: next.Type, Description: next.Description,
			Status: next.Status, Labels: labels,
		})
		if err != nil {
			return fmt.Errorf("update the environment: %w", err)
		}
		if after, err = environment(row); err != nil {
			return err
		}
		return record(audit.ActionUpdate, after.ID.String(), before, after)
	})
	if err != nil {
		return Environment{}, err
	}
	return after, nil
}

func applyEnvironmentChange(e Environment, c EnvironmentChange) (Environment, error) {
	var err error
	if c.Name != nil {
		if e.Name, err = name("name", *c.Name); err != nil {
			return e, err
		}
	}
	if c.Type != nil {
		if e.Type, err = oneOf("type", *c.Type, "", environmentTypes); err != nil {
			return e, err
		}
	}
	if c.Description != nil {
		if e.Description, err = text("description", *c.Description, maxDescriptionRunes, true); err != nil {
			return e, err
		}
	}
	if c.Status != nil {
		if e.Status, err = oneOf("status", *c.Status, "", statuses); err != nil {
			return e, err
		}
	}
	if c.Labels != nil {
		if e.Labels, err = labels(*c.Labels); err != nil {
			return e, err
		}
	}
	return e, nil
}

func sameEnvironment(a, b Environment) bool {
	return a.Name == b.Name && a.Type == b.Type && a.Description == b.Description &&
		a.Status == b.Status && maps.Equal(a.Labels, b.Labels)
}

// DeleteEnvironment deletes the environment with id and audits it. It fails
// with ErrNotFound for an unknown id, and with a ConflictError while
// anything refers to the environment.
func (s *Service) DeleteEnvironment(ctx context.Context, caller Caller, id uuid.UUID) error {
	return s.change(ctx, caller, audit.EntityEnvironment, func(q *store.Queries, record func(string, string, any, any) error) error {
		row, err := q.LockEnvironment(ctx, pgUUID(id))
		if err != nil {
			return notFound(err)
		}
		if err := managedByFile(row.ManagedBy, "environment"); err != nil {
			return err
		}
		before, err := environment(row)
		if err != nil {
			return err
		}
		if _, err := q.DeleteEnvironment(ctx, row.ID); err != nil {
			return inUse(err, "environment", "disable it instead")
		}
		return record(audit.ActionDelete, before.ID.String(), before, nil)
	})
}
