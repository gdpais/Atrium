// Package inventory manages environments, their applications and the
// applications' components: the inventory everything else Heartbeat knows
// belongs to (ADR 0001, ADR 0013 §2).
//
// There is one ownership path. An application belongs to the environment
// it was created in, applications.environment_id, and a component to the
// application it was created in; neither ever moves, and a component's
// environment is read through its application, never stored. Deletes are
// refused while anything refers to the entity, as the foreign keys
// (on delete restrict) say; disabling is the way to retire one.
//
// Each change runs in one transaction with its audit record (ADR 0016):
// the record's before and after are the entity as the API shows it. A
// change that changes nothing writes nothing and records nothing.
//
// Package inventory knows nothing of HTTP: package httpapi maps its errors
// to problems.
package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"heartbeat/apps/api/internal/audit"
	"heartbeat/apps/api/internal/store"
)

// Statuses of environments and applications.
const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
)

// Who manages a row (ADR 0021 §4): the API's endpoints, or the
// environment's inventory file, applied by the API's reconciler. Each row
// has one writer.
const (
	ManagedByAPI    = "api"
	ManagedByConfig = "config"
)

// ErrNotFound is an ID that names no entity of its type.
var ErrNotFound = errors.New("inventory: not found")

// InvalidError is a field whose value is not acceptable.
type InvalidError struct {
	Field  string
	Reason string
}

func (e *InvalidError) Error() string { return e.Field + ": " + e.Reason }

// ConflictError is a change the existing entities refuse: a name or slug
// another entity has, or a delete while others still refer to the entity.
type ConflictError struct{ Detail string }

func (e *ConflictError) Error() string { return e.Detail }

// OwnerError is an environment or application, named as an entity's
// owner, that does not exist.
type OwnerError struct{ Detail string }

func (e *OwnerError) Error() string { return e.Detail }

// DB is PostgreSQL: a pool, which runs queries and begins transactions.
type DB interface {
	store.DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Caller is who asks for a change, for its audit record.
type Caller struct {
	Actor audit.Actor
	// RequestID identifies the request that makes the change.
	RequestID string
}

// Service reads and changes the inventory. It is safe for concurrent use.
type Service struct {
	db       DB
	observer Observer
	runner   Runner
}

// New returns a Service on db.
func New(db DB, opts ...Option) *Service {
	s := &Service{db: db}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// change runs fn in a transaction, with the queries to make the change and
// a function that audits it through the same transaction.
func (s *Service) change(ctx context.Context, caller Caller, entityType string, fn func(q *store.Queries, record func(action, id string, before, after any) error) error) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		return fn(q, func(action, id string, before, after any) error {
			_, err := audit.Write(ctx, q, audit.Change{
				RequestID:  caller.RequestID,
				Actor:      caller.Actor,
				Action:     action,
				EntityType: entityType,
				EntityID:   id,
				Before:     before,
				After:      after,
			})
			return err
		})
	})
}

// referrers names, for the error a refused delete returns, the tables that
// can refer to an environment or an application.
var referrers = map[string]string{
	"applications":           "applications",
	"application_components": "components",
	"telemetry_sources":      "telemetry sources",
	"database_targets":       "database targets",
	"investigations":         "investigations",
	"alert_policies":         "alert policies",
	"notification_routes":    "notification routes",
	"alert_events":           "alert events",
	"report_schedules":       "report schedules",
	"report_runs":            "report runs",
}

// inUse turns the foreign key violation of a refused delete into a
// ConflictError that says what still refers to the entity. PostgreSQL 18
// reports an `on delete restrict` key as restrict_violation; earlier
// releases, and `no action` keys, as foreign_key_violation.
func inUse(err error, entity, advice string) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || (pgErr.Code != restrictViolation && pgErr.Code != foreignKeyViolation) {
		return fmt.Errorf("delete the %s: %w", entity, err)
	}
	// For a delete, the error's table is the one whose rows still refer
	// to the entity.
	by, ok := referrers[pgErr.TableName]
	if !ok {
		by = strings.ReplaceAll(pgErr.TableName, "_", " ")
	}
	return &ConflictError{Detail: fmt.Sprintf("the %s still has %s, so it cannot be deleted: %s", entity, by, advice)}
}

// constraint returns the name of the constraint err violates, with code,
// or "" when err is not such a violation.
func constraint(err error, code string) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == code {
		return pgErr.ConstraintName
	}
	return ""
}

// SQLSTATE codes.
const (
	uniqueViolation     = "23505"
	foreignKeyViolation = "23503"
	restrictViolation   = "23001"
)

// notFound maps pgx.ErrNoRows to ErrNotFound.
func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func pgUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

func pgText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

func goUUID(id pgtype.UUID) uuid.UUID { return uuid.UUID(id.Bytes) }

func goTime(t pgtype.Timestamptz) time.Time { return t.Time.UTC() }

// encodeLabels returns labels as the jsonb the tables hold: an object,
// never null.
func encodeLabels(labels map[string]string) ([]byte, error) {
	if labels == nil {
		labels = map[string]string{}
	}
	return json.Marshal(labels)
}

// decodeLabels reads the labels a row holds.
func decodeLabels(raw []byte) (map[string]string, error) {
	labels := map[string]string{}
	if len(raw) == 0 {
		return labels, nil
	}
	if err := json.Unmarshal(raw, &labels); err != nil {
		return nil, fmt.Errorf("labels: %w", err)
	}
	return labels, nil
}

// managedByFile refuses a change, through the API's endpoints, to a row the
// inventory file manages: the file is its only writer, and a change made
// here would be undone by the next apply or fight it.
func managedByFile(managedBy, entity string) error {
	if managedBy != ManagedByConfig {
		return nil
	}
	return &ConflictError{Detail: fmt.Sprintf("the %s is managed by its environment's inventory file (ADR 0021): change it there, by pull request", entity)}
}
