package inventory

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"heartbeat/apps/api/internal/audit"
	"heartbeat/apps/api/internal/store"
)

// Application is an application as the API shows it, and as its audit
// records hold it.
type Application struct {
	ID uuid.UUID `json:"id"`
	// EnvironmentID is the environment it was created in, for good.
	EnvironmentID uuid.UUID `json:"environment_id"`
	Name          string    `json:"name"`
	Platform      string    `json:"platform"`
	OwnerTeam     string    `json:"owner_team"`
	Criticality   string    `json:"criticality"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// NewApplication is an application to create in EnvironmentID.
// Criticality defaults to medium and Status to active.
type NewApplication struct {
	EnvironmentID uuid.UUID
	Name          string
	Platform      string
	OwnerTeam     string
	Criticality   string
	Status        string
}

// ApplicationChange holds the fields to change; nil keeps a field. The
// environment cannot change.
type ApplicationChange struct {
	Name        *string
	Platform    *string
	OwnerTeam   *string
	Criticality *string
	Status      *string
}

// ApplicationFilter narrows a list of applications; zero values match all.
type ApplicationFilter struct {
	EnvironmentID uuid.UUID
	Status        string
}

// ApplicationPage is one page of applications.
type ApplicationPage struct {
	Items []Application
	// NextCursor is the next page's cursor; "" on the last page.
	NextCursor string
}

const defaultCriticality = "medium"

func application(row store.Application) Application {
	return Application{
		ID: goUUID(row.ID), EnvironmentID: goUUID(row.EnvironmentID), Name: row.Name,
		Platform: row.Platform, OwnerTeam: row.OwnerTeam, Criticality: row.Criticality,
		Status: row.Status, CreatedAt: goTime(row.CreatedAt), UpdatedAt: goTime(row.UpdatedAt),
	}
}

// ListApplications returns a page of the applications filter matches, by
// name, then ID.
func (s *Service) ListApplications(ctx context.Context, filter ApplicationFilter, p Page) (ApplicationPage, error) {
	limit, after, err := p.resolve()
	if err != nil {
		return ApplicationPage{}, err
	}
	params := store.ListApplicationsParams{RowLimit: int32(limit + 1)}
	if filter.EnvironmentID != uuid.Nil {
		params.EnvironmentID = pgUUID(filter.EnvironmentID)
	}
	if filter.Status != "" {
		if _, err := oneOf("status", filter.Status, "", statuses); err != nil {
			return ApplicationPage{}, err
		}
		params.Status = pgText(filter.Status)
	}
	if after != nil {
		params.AfterName = pgtype.Text{String: after.Name, Valid: true}
		params.AfterID = pgUUID(after.ID)
	}
	rows, err := store.New(s.db).ListApplications(ctx, params)
	if err != nil {
		return ApplicationPage{}, fmt.Errorf("list applications: %w", err)
	}
	rows, next := page(rows, limit, func(r store.Application) (string, uuid.UUID) { return r.Name, goUUID(r.ID) })
	out := ApplicationPage{Items: make([]Application, len(rows)), NextCursor: next}
	for i, row := range rows {
		out.Items[i] = application(row)
	}
	return out, nil
}

// GetApplication returns the application with id, or ErrNotFound.
func (s *Service) GetApplication(ctx context.Context, id uuid.UUID) (Application, error) {
	row, err := store.New(s.db).GetApplication(ctx, pgUUID(id))
	if err != nil {
		return Application{}, notFound(err)
	}
	return application(row), nil
}

// CreateApplication creates an application in its environment and audits
// it. It fails with an OwnerError when the environment does not exist, and
// with a ConflictError when the environment has an application of that
// name.
func (s *Service) CreateApplication(ctx context.Context, caller Caller, in NewApplication) (Application, error) {
	params, err := newApplicationParams(in)
	if err != nil {
		return Application{}, err
	}
	var created Application
	err = s.change(ctx, caller, audit.EntityApplication, func(q *store.Queries, record func(string, string, any, any) error) error {
		row, err := q.CreateApplication(ctx, params)
		switch {
		case constraint(err, foreignKeyViolation) != "":
			return &OwnerError{Detail: fmt.Sprintf("environment %s does not exist", in.EnvironmentID)}
		case constraint(err, uniqueViolation) != "":
			return &ConflictError{Detail: fmt.Sprintf("the environment already has an application named %q", params.Name)}
		case err != nil:
			return fmt.Errorf("create the application: %w", err)
		}
		created = application(row)
		return record(audit.ActionCreate, created.ID.String(), nil, created)
	})
	if err != nil {
		return Application{}, err
	}
	return created, nil
}

func newApplicationParams(in NewApplication) (store.CreateApplicationParams, error) {
	p := store.CreateApplicationParams{EnvironmentID: pgUUID(in.EnvironmentID)}
	var err error
	if in.EnvironmentID == uuid.Nil {
		return p, invalid("environment_id", "required")
	}
	if p.Name, err = name("name", in.Name); err != nil {
		return p, err
	}
	if p.Platform, err = identifier("platform", in.Platform); err != nil {
		return p, err
	}
	if p.OwnerTeam, err = text("owner_team", in.OwnerTeam, maxTextRunes, false); err != nil {
		return p, err
	}
	if p.Criticality, err = oneOf("criticality", in.Criticality, defaultCriticality, criticalities); err != nil {
		return p, err
	}
	p.Status, err = oneOf("status", in.Status, StatusActive, statuses)
	return p, err
}

// UpdateApplication changes the fields change holds and audits it, and
// returns the application as it is then. A change that changes nothing
// writes and audits nothing. It fails with ErrNotFound for an unknown id,
// and with a ConflictError when another application of its environment has
// the new name.
func (s *Service) UpdateApplication(ctx context.Context, caller Caller, id uuid.UUID, change ApplicationChange) (Application, error) {
	var after Application
	err := s.change(ctx, caller, audit.EntityApplication, func(q *store.Queries, record func(string, string, any, any) error) error {
		row, err := q.LockApplication(ctx, pgUUID(id))
		if err != nil {
			return notFound(err)
		}
		before := application(row)
		next, err := applyApplicationChange(before, change)
		if err != nil {
			return err
		}
		if next == before {
			after = before
			return nil
		}
		row, err = q.UpdateApplication(ctx, store.UpdateApplicationParams{
			ID: row.ID, Name: next.Name, Platform: next.Platform, OwnerTeam: next.OwnerTeam,
			Criticality: next.Criticality, Status: next.Status,
		})
		if constraint(err, uniqueViolation) != "" {
			return &ConflictError{Detail: fmt.Sprintf("the environment already has an application named %q", next.Name)}
		}
		if err != nil {
			return fmt.Errorf("update the application: %w", err)
		}
		after = application(row)
		return record(audit.ActionUpdate, after.ID.String(), before, after)
	})
	if err != nil {
		return Application{}, err
	}
	return after, nil
}

func applyApplicationChange(a Application, c ApplicationChange) (Application, error) {
	var err error
	if c.Name != nil {
		if a.Name, err = name("name", *c.Name); err != nil {
			return a, err
		}
	}
	if c.Platform != nil {
		if a.Platform, err = identifier("platform", *c.Platform); err != nil {
			return a, err
		}
	}
	if c.OwnerTeam != nil {
		if a.OwnerTeam, err = text("owner_team", *c.OwnerTeam, maxTextRunes, false); err != nil {
			return a, err
		}
	}
	if c.Criticality != nil {
		if a.Criticality, err = oneOf("criticality", *c.Criticality, "", criticalities); err != nil {
			return a, err
		}
	}
	if c.Status != nil {
		if a.Status, err = oneOf("status", *c.Status, "", statuses); err != nil {
			return a, err
		}
	}
	return a, nil
}

// DeleteApplication deletes the application with id and audits it. It
// fails with ErrNotFound for an unknown id, and with a ConflictError while
// anything refers to the application.
func (s *Service) DeleteApplication(ctx context.Context, caller Caller, id uuid.UUID) error {
	return s.change(ctx, caller, audit.EntityApplication, func(q *store.Queries, record func(string, string, any, any) error) error {
		row, err := q.LockApplication(ctx, pgUUID(id))
		if err != nil {
			return notFound(err)
		}
		before := application(row)
		if _, err := q.DeleteApplication(ctx, row.ID); err != nil {
			return inUse(err, "application", "disable it instead")
		}
		return record(audit.ActionDelete, before.ID.String(), before, nil)
	})
}
