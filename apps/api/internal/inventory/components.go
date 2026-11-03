package inventory

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"heartbeat/apps/api/internal/audit"
	"heartbeat/apps/api/internal/store"
)

// Component is an application component as the API shows it, and as its
// audit records hold it.
type Component struct {
	ID uuid.UUID `json:"id"`
	// ApplicationID is the application it was created in, for good.
	ApplicationID uuid.UUID `json:"application_id"`
	// EnvironmentID is its application's environment, read through the
	// application: a component stores none.
	EnvironmentID uuid.UUID         `json:"environment_id"`
	Name          string            `json:"name"`
	ComponentType string            `json:"component_type"`
	RuntimeName   string            `json:"runtime_name"`
	Labels        map[string]string `json:"labels"`
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
}

// NewComponent is a component to create in ApplicationID.
type NewComponent struct {
	ApplicationID uuid.UUID
	Name          string
	ComponentType string
	RuntimeName   string
	Labels        map[string]string
}

// ComponentChange holds the fields to change; nil keeps a field. The
// application cannot change.
type ComponentChange struct {
	Name          *string
	ComponentType *string
	RuntimeName   *string
	Labels        *map[string]string
}

// ComponentFilter narrows a list of components; zero values match all.
type ComponentFilter struct {
	ApplicationID uuid.UUID
}

// ComponentPage is one page of components.
type ComponentPage struct {
	Items []Component
	// NextCursor is the next page's cursor; "" on the last page.
	NextCursor string
}

// component reads a row of the queries that join the application, whose
// generated row types all have these fields.
func component(row store.GetComponentRow) (Component, error) {
	labels, err := decodeLabels(row.Labels)
	if err != nil {
		return Component{}, err
	}
	return Component{
		ID: goUUID(row.ID), ApplicationID: goUUID(row.ApplicationID), EnvironmentID: goUUID(row.EnvironmentID),
		Name: row.Name, ComponentType: row.ComponentType, RuntimeName: row.RuntimeName, Labels: labels,
		CreatedAt: goTime(row.CreatedAt), UpdatedAt: goTime(row.UpdatedAt),
	}, nil
}

// componentOf reads a row of application_components, of an application in
// environmentID.
func componentOf(row store.ApplicationComponent, environmentID pgtype.UUID) (Component, error) {
	return component(store.GetComponentRow{
		ID: row.ID, ApplicationID: row.ApplicationID, EnvironmentID: environmentID,
		Name: row.Name, ComponentType: row.ComponentType, RuntimeName: row.RuntimeName,
		Labels: row.Labels, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	})
}

// ListComponents returns a page of the components filter matches, by name,
// then ID.
func (s *Service) ListComponents(ctx context.Context, filter ComponentFilter, p Page) (ComponentPage, error) {
	limit, after, err := p.resolve()
	if err != nil {
		return ComponentPage{}, err
	}
	params := store.ListComponentsParams{RowLimit: int32(limit + 1)}
	if filter.ApplicationID != uuid.Nil {
		params.ApplicationID = pgUUID(filter.ApplicationID)
	}
	if after != nil {
		params.AfterName = pgtype.Text{String: after.Name, Valid: true}
		params.AfterID = pgUUID(after.ID)
	}
	rows, err := store.New(s.db).ListComponents(ctx, params)
	if err != nil {
		return ComponentPage{}, fmt.Errorf("list components: %w", err)
	}
	rows, next := page(rows, limit, func(r store.ListComponentsRow) (string, uuid.UUID) { return r.Name, goUUID(r.ID) })
	out := ComponentPage{Items: make([]Component, len(rows)), NextCursor: next}
	for i, row := range rows {
		if out.Items[i], err = component(store.GetComponentRow(row)); err != nil {
			return ComponentPage{}, err
		}
	}
	return out, nil
}

// GetComponent returns the component with id, or ErrNotFound.
func (s *Service) GetComponent(ctx context.Context, id uuid.UUID) (Component, error) {
	row, err := store.New(s.db).GetComponent(ctx, pgUUID(id))
	if err != nil {
		return Component{}, notFound(err)
	}
	return component(row)
}

// CreateComponent creates a component of its application and audits it.
// It fails with an OwnerError when the application does not exist, and with
// a ConflictError when the application has a component of that name and
// type.
func (s *Service) CreateComponent(ctx context.Context, caller Caller, in NewComponent) (Component, error) {
	params, err := newComponentParams(in)
	if err != nil {
		return Component{}, err
	}
	noApplication := &OwnerError{Detail: fmt.Sprintf("application %s does not exist", in.ApplicationID)}
	var created Component
	err = s.change(ctx, caller, audit.EntityComponent, func(q *store.Queries, record func(string, string, any, any) error) error {
		// The application's environment never changes, so reading it
		// unlocked is safe; the insert's foreign key check keeps the
		// application from being deleted until the transaction ends.
		app, err := q.GetApplication(ctx, params.ApplicationID)
		if errors.Is(err, pgx.ErrNoRows) {
			return noApplication
		}
		if err != nil {
			return fmt.Errorf("read the application: %w", err)
		}
		row, err := q.CreateComponent(ctx, params)
		switch {
		case constraint(err, foreignKeyViolation) != "":
			return noApplication
		case constraint(err, uniqueViolation) != "":
			return &ConflictError{Detail: fmt.Sprintf("the application already has a %s component named %q", params.ComponentType, params.Name)}
		case err != nil:
			return fmt.Errorf("create the component: %w", err)
		}
		if created, err = componentOf(row, app.EnvironmentID); err != nil {
			return err
		}
		return record(audit.ActionCreate, created.ID.String(), nil, created)
	})
	if err != nil {
		return Component{}, err
	}
	return created, nil
}

func newComponentParams(in NewComponent) (store.CreateComponentParams, error) {
	p := store.CreateComponentParams{ApplicationID: pgUUID(in.ApplicationID)}
	var err error
	if in.ApplicationID == uuid.Nil {
		return p, invalid("application_id", "required")
	}
	if p.Name, err = name("name", in.Name); err != nil {
		return p, err
	}
	if p.ComponentType, err = identifier("component_type", in.ComponentType); err != nil {
		return p, err
	}
	if p.RuntimeName, err = text("runtime_name", in.RuntimeName, maxTextRunes, false); err != nil {
		return p, err
	}
	l, err := labels(in.Labels)
	if err != nil {
		return p, err
	}
	p.Labels, err = encodeLabels(l)
	return p, err
}

// UpdateComponent changes the fields change holds and audits it, and
// returns the component as it is then. A change that changes nothing
// writes and audits nothing. It fails with ErrNotFound for an unknown id,
// and with a ConflictError when another component of its application has
// the new name and type.
func (s *Service) UpdateComponent(ctx context.Context, caller Caller, id uuid.UUID, change ComponentChange) (Component, error) {
	var after Component
	err := s.change(ctx, caller, audit.EntityComponent, func(q *store.Queries, record func(string, string, any, any) error) error {
		locked, err := q.LockComponent(ctx, pgUUID(id))
		if err != nil {
			return notFound(err)
		}
		before, err := component(store.GetComponentRow(locked))
		if err != nil {
			return err
		}
		next, err := applyComponentChange(before, change)
		if err != nil {
			return err
		}
		if sameComponent(before, next) {
			after = before
			return nil
		}
		labels, err := encodeLabels(next.Labels)
		if err != nil {
			return err
		}
		row, err := q.UpdateComponent(ctx, store.UpdateComponentParams{
			ID: locked.ID, Name: next.Name, ComponentType: next.ComponentType,
			RuntimeName: next.RuntimeName, Labels: labels,
		})
		if constraint(err, uniqueViolation) != "" {
			return &ConflictError{Detail: fmt.Sprintf("the application already has a %s component named %q", next.ComponentType, next.Name)}
		}
		if err != nil {
			return fmt.Errorf("update the component: %w", err)
		}
		if after, err = componentOf(row, locked.EnvironmentID); err != nil {
			return err
		}
		return record(audit.ActionUpdate, after.ID.String(), before, after)
	})
	if err != nil {
		return Component{}, err
	}
	return after, nil
}

func applyComponentChange(c Component, ch ComponentChange) (Component, error) {
	var err error
	if ch.Name != nil {
		if c.Name, err = name("name", *ch.Name); err != nil {
			return c, err
		}
	}
	if ch.ComponentType != nil {
		if c.ComponentType, err = identifier("component_type", *ch.ComponentType); err != nil {
			return c, err
		}
	}
	if ch.RuntimeName != nil {
		if c.RuntimeName, err = text("runtime_name", *ch.RuntimeName, maxTextRunes, false); err != nil {
			return c, err
		}
	}
	if ch.Labels != nil {
		if c.Labels, err = labels(*ch.Labels); err != nil {
			return c, err
		}
	}
	return c, nil
}

func sameComponent(a, b Component) bool {
	return a.Name == b.Name && a.ComponentType == b.ComponentType && a.RuntimeName == b.RuntimeName &&
		maps.Equal(a.Labels, b.Labels)
}

// DeleteComponent deletes the component with id and audits it. It fails
// with ErrNotFound for an unknown id.
func (s *Service) DeleteComponent(ctx context.Context, caller Caller, id uuid.UUID) error {
	return s.change(ctx, caller, audit.EntityComponent, func(q *store.Queries, record func(string, string, any, any) error) error {
		locked, err := q.LockComponent(ctx, pgUUID(id))
		if err != nil {
			return notFound(err)
		}
		before, err := component(store.GetComponentRow(locked))
		if err != nil {
			return err
		}
		if _, err := q.DeleteComponent(ctx, locked.ID); err != nil {
			return inUse(err, "component", "delete what refers to it first")
		}
		return record(audit.ActionDelete, before.ID.String(), before, nil)
	})
}
