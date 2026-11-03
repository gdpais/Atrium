// Package probedefs manages probe definitions (TODO 4.5): what a probe
// assignment runs against a database target.
//
// A definition is versioned. Each version is a row of probe_definitions that
// never changes once written, except whether its definition is enabled, so an
// assignment that pins a version keeps what was approved. Built-in
// definitions are the db-collector's catalog, seeded by a migration with no
// query of their own; they are read-only. A custom definition replaces the
// query of the built-in probe it overrides, whose metric descriptors decode
// its rows, and its query is checked with the collector's own validator
// ([config.ValidateReadOnlySelect]), so the API never stores a query the
// collector would refuse to load. Changing it adds a version; disabling it
// disables every version, and nothing is deleted.
//
// Every change and its audit record are written in one transaction.
package probedefs

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"heartbeat/apps/api/internal/audit"
	"heartbeat/apps/api/internal/store"
	"heartbeat/internal/config"
)

// Limits on what a custom definition holds.
const (
	// DefaultIntervalSeconds is the interval a definition suggests when it
	// sets none: the collector's default scrape interval.
	DefaultIntervalSeconds = 30
	// MinIntervalSeconds and MaxIntervalSeconds bound a definition's
	// default interval.
	MinIntervalSeconds = 5
	MaxIntervalSeconds = 3600
	// MaxQueryBytes bounds a query_template. The longest built-in query is
	// about 2 KiB.
	MaxQueryBytes = 16 << 10
	// MaxDescriptionRunes bounds a description.
	MaxDescriptionRunes = 1000
)

// MaxTimeoutMS is the longest timeout_ms a definition may set: the
// collector refuses a longer one ([config.MaxProbeTimeout]).
var MaxTimeoutMS = int(config.MaxProbeTimeout.Milliseconds())

// Engines a custom definition may be for: the collector kinds the API
// renders assignments to (ADR 0014).
var Engines = []string{config.KindSQLServer}

// probeName matches a definition's name and the built-in probe it
// overrides: lower-case snake_case, as the collector's catalog names them.
var probeName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// ErrNotFound reports an ID that is no version of any probe definition.
var ErrNotFound = errors.New("no such probe definition")

// Conflicts: changes the definition's state does not allow.
var (
	ErrNameTaken = &ConflictError{"a probe definition with this engine and name exists; add a version to it instead"}
	ErrBuiltin   = &ConflictError{"built-in probe definitions are read-only: they change with the db-collector"}
	ErrDisabled  = &ConflictError{"the probe definition is disabled; enable it before adding a version"}
)

// ConflictError is a change the definition's current state does not allow,
// such as a version added to one that is not the latest.
type ConflictError struct{ reason string }

func (e *ConflictError) Error() string { return e.reason }

// InvalidError is a definition that cannot be stored, such as a query that
// is not one read-only SELECT. It says why, without quoting the query.
type InvalidError struct{ reason string }

func (e *InvalidError) Error() string { return e.reason }

func invalid(format string, args ...any) *InvalidError {
	return &InvalidError{reason: fmt.Sprintf(format, args...)}
}

// Definition is one version of a probe definition. Its JSON form is what
// audit records hold.
type Definition struct {
	// ID identifies this version; an assignment pins it.
	ID      uuid.UUID `json:"id"`
	Engine  string    `json:"engine"`
	Name    string    `json:"name"`
	Version int       `json:"version"`
	Builtin bool      `json:"builtin"`
	// Overrides is the built-in probe whose query a custom definition
	// replaces; empty for a built-in.
	Overrides   string `json:"overrides,omitempty"`
	Category    string `json:"category"`
	Description string `json:"description"`
	// DefaultIntervalSeconds is the interval an assignment gets unless it
	// sets one.
	DefaultIntervalSeconds int `json:"default_interval_seconds"`
	// TimeoutMS is 0 for the collector's default: half the interval, at
	// most 10s.
	TimeoutMS int `json:"timeout_ms,omitempty"`
	// QueryTemplate is empty for a built-in, which runs the collector's own
	// query.
	QueryTemplate string `json:"query_template,omitempty"`
	// Enabled is the definition's, shared by all its versions.
	Enabled bool `json:"enabled"`
	// Latest reports the definition's highest version.
	Latest    bool      `json:"-"`
	CreatedAt time.Time `json:"-"`
	UpdatedAt time.Time `json:"-"`
}

// Content is what a version of a custom definition holds, as a client sets
// it.
type Content struct {
	// Overrides names the built-in probe whose query this one replaces. Its
	// metric descriptors decode the rows, so the query must return the
	// same columns, in the same units.
	Overrides   string
	Description string
	// DefaultIntervalSeconds is 0 for [DefaultIntervalSeconds].
	DefaultIntervalSeconds int
	// TimeoutMS is 0 for the collector's default.
	TimeoutMS     int
	QueryTemplate string
}

// NewDefinition is a custom definition to create.
type NewDefinition struct {
	Engine string
	Name   string
	Content
}

// DB is a PostgreSQL pool, or a connection.
type DB interface {
	store.DBTX
	audit.Beginner
}

// Service reads and changes probe definitions.
type Service struct {
	db DB
}

// NewService returns a Service on db.
func NewService(db DB) *Service { return &Service{db: db} }

// List returns the latest version of every definition, built-in and custom,
// sorted by engine and name.
func (s *Service) List(ctx context.Context) ([]Definition, error) {
	rows, err := store.New(s.db).ListLatestProbeDefinitions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list probe definitions: %w", err)
	}
	defs := make([]Definition, len(rows))
	for i, row := range rows {
		defs[i] = definition(row.ProbeDefinition, true)
	}
	return defs, nil
}

// Get returns the version id, or [ErrNotFound].
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Definition, error) {
	return get(ctx, store.New(s.db), id)
}

// Versions returns every version of the definition id is a version of, the
// newest first, or [ErrNotFound].
func (s *Service) Versions(ctx context.Context, id uuid.UUID) ([]Definition, error) {
	rows, err := store.New(s.db).ListProbeDefinitionVersions(ctx, pgUUID(id))
	if err != nil {
		return nil, fmt.Errorf("list probe definition versions: %w", err)
	}
	if len(rows) == 0 {
		return nil, ErrNotFound
	}
	defs := make([]Definition, len(rows))
	for i, row := range rows {
		defs[i] = definition(row.ProbeDefinition, row.Latest)
	}
	return defs, nil
}

// Create stores version 1 of a custom definition, and its audit record, in
// one transaction. It fails with an [*InvalidError] for a definition that
// cannot be stored, and with [ErrNameTaken] when the engine and name belong
// to a definition already, built-in or custom.
func (s *Service) Create(ctx context.Context, def NewDefinition, actor audit.Actor, requestID string) (Definition, error) {
	if !slices.Contains(Engines, def.Engine) {
		return Definition{}, invalid("engine %q: want one of %s", def.Engine, strings.Join(Engines, ", "))
	}
	if !probeName.MatchString(def.Name) {
		return Definition{}, invalid("name %q: want lower-case letters, digits and underscores, starting with a letter, at most 63", def.Name)
	}
	content, err := normalize(def.Content)
	if err != nil {
		return Definition{}, err
	}
	var created Definition
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		category, err := builtinCategory(ctx, q, def.Engine, content.Overrides)
		if err != nil {
			return err
		}
		_, err = q.LockLatestProbeDefinitionVersion(ctx, store.LockLatestProbeDefinitionVersionParams{Engine: def.Engine, Name: def.Name})
		switch {
		case err == nil:
			return ErrNameTaken
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("look up the probe definition: %w", err)
		}
		created, err = insert(ctx, q, def.Engine, def.Name, 1, category, content)
		if isUniqueViolation(err) {
			return ErrNameTaken
		}
		if err != nil {
			return err
		}
		_, err = audit.Write(ctx, q, audit.Change{
			RequestID: requestID, Actor: actor, Action: audit.ActionCreate,
			EntityType: audit.EntityProbeDefinition, EntityID: created.ID.String(),
			After: created,
		})
		return err
	})
	if err != nil {
		return Definition{}, err
	}
	return created, nil
}

// AddVersion stores the next version of the custom definition whose latest
// version is from, holding content, and its audit record, in one
// transaction. It returns the new version and true, or from and false when
// content is what from holds already, which writes nothing.
//
// It fails with [ErrNotFound], an [*InvalidError], [ErrBuiltin], [ErrDisabled],
// or a [*ConflictError] when from is not the latest version: someone added
// one since it was read.
func (s *Service) AddVersion(ctx context.Context, from uuid.UUID, content Content, actor audit.Actor, requestID string) (Definition, bool, error) {
	content, err := normalize(content)
	if err != nil {
		return Definition{}, false, err
	}
	var result Definition
	var added bool
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		previous, latest, err := lockLatest(ctx, q, from)
		if err != nil {
			return err
		}
		if latest.ID != pgUUID(from) {
			return &ConflictError{fmt.Sprintf("version %d is not the latest of probe definition %s: version %d (%s) is; add the version to that one",
				previous.Version, previous.Name, latest.Version, latest.ID.String())}
		}
		if !latest.Enabled {
			return ErrDisabled
		}
		if contentOf(previous) == content {
			result = previous
			return nil
		}
		category, err := builtinCategory(ctx, q, previous.Engine, content.Overrides)
		if err != nil {
			return err
		}
		next, err := insert(ctx, q, previous.Engine, previous.Name, previous.Version+1, category, content)
		if err != nil {
			return err
		}
		if _, err := audit.Write(ctx, q, audit.Change{
			RequestID: requestID, Actor: actor, Action: audit.ActionUpdate,
			EntityType: audit.EntityProbeDefinition, EntityID: next.ID.String(),
			Before: previous, After: next,
		}); err != nil {
			return err
		}
		result, added = next, true
		return nil
	})
	if err != nil {
		return Definition{}, false, err
	}
	return result, added, nil
}

// SetEnabled enables or disables the custom definition id is a version of:
// every version of it. An assignment cannot pin a disabled definition's
// versions. It returns version id as it is afterwards; when the definition
// is in that state already, nothing is written. The audit record's entity is
// version id. It fails with [ErrNotFound] or [ErrBuiltin].
func (s *Service) SetEnabled(ctx context.Context, id uuid.UUID, enabled bool, actor audit.Actor, requestID string) (Definition, error) {
	var result Definition
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		before, _, err := lockLatest(ctx, q, id)
		if err != nil {
			return err
		}
		changed, err := q.SetProbeDefinitionEnabled(ctx, store.SetProbeDefinitionEnabledParams{
			Engine: before.Engine, Name: before.Name, Enabled: enabled,
		})
		if err != nil {
			return fmt.Errorf("set the probe definition's enabled: %w", err)
		}
		if changed == 0 {
			result = before
			return nil
		}
		if result, err = get(ctx, q, id); err != nil {
			return err
		}
		action := audit.ActionDisable
		if enabled {
			action = audit.ActionEnable
		}
		_, err = audit.Write(ctx, q, audit.Change{
			RequestID: requestID, Actor: actor, Action: action,
			EntityType: audit.EntityProbeDefinition, EntityID: result.ID.String(),
			Before: before, After: result,
		})
		return err
	})
	if err != nil {
		return Definition{}, err
	}
	return result, nil
}

// lockLatest returns version id of a custom definition and its latest
// version, locked until the transaction ends.
func lockLatest(ctx context.Context, q *store.Queries, id uuid.UUID) (Definition, store.LockLatestProbeDefinitionVersionRow, error) {
	def, err := get(ctx, q, id)
	if err != nil {
		return Definition{}, store.LockLatestProbeDefinitionVersionRow{}, err
	}
	if def.Builtin {
		return Definition{}, store.LockLatestProbeDefinitionVersionRow{}, ErrBuiltin
	}
	latest, err := q.LockLatestProbeDefinitionVersion(ctx, store.LockLatestProbeDefinitionVersionParams{Engine: def.Engine, Name: def.Name})
	if err != nil {
		return Definition{}, store.LockLatestProbeDefinitionVersionRow{}, fmt.Errorf("lock the probe definition: %w", err)
	}
	return def, latest, nil
}

func get(ctx context.Context, q *store.Queries, id uuid.UUID) (Definition, error) {
	row, err := q.GetProbeDefinition(ctx, pgUUID(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Definition{}, ErrNotFound
	}
	if err != nil {
		return Definition{}, fmt.Errorf("get the probe definition: %w", err)
	}
	return definition(row.ProbeDefinition, row.Latest), nil
}

// builtinCategory returns the category of the enabled built-in probe named
// overrides, which a custom definition takes, or an [*InvalidError].
func builtinCategory(ctx context.Context, q *store.Queries, engine, overrides string) (string, error) {
	category, err := q.GetBuiltinProbeCategory(ctx, store.GetBuiltinProbeCategoryParams{Engine: engine, Name: overrides})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", invalid("overrides %q: no enabled built-in %s probe has this name; GET /api/v1/probe-definitions lists them", overrides, engine)
	}
	if err != nil {
		return "", fmt.Errorf("look up the built-in probe: %w", err)
	}
	return category, nil
}

func insert(ctx context.Context, q *store.Queries, engine, name string, version int, category string, c Content) (Definition, error) {
	row, err := q.InsertProbeDefinition(ctx, store.InsertProbeDefinitionParams{
		Engine: engine, Name: name, Version: int32(version), Category: category,
		Overrides:              pgtype.Text{String: c.Overrides, Valid: true},
		Description:            c.Description,
		DefaultIntervalSeconds: int32(c.DefaultIntervalSeconds),
		TimeoutMs:              pgtype.Int4{Int32: int32(c.TimeoutMS), Valid: c.TimeoutMS != 0},
		QueryTemplate:          pgtype.Text{String: c.QueryTemplate, Valid: true},
	})
	if err != nil {
		return Definition{}, fmt.Errorf("insert the probe definition: %w", err)
	}
	return definition(row, true), nil
}

// normalize returns c with its defaults applied, or an [*InvalidError] for
// a field that cannot be stored. Whether the built-in it overrides exists
// is checked against the database.
func normalize(c Content) (Content, error) {
	if !probeName.MatchString(c.Overrides) {
		return Content{}, invalid("overrides %q: want the name of a built-in probe, such as waits", c.Overrides)
	}
	if !utf8.ValidString(c.Description) || utf8.RuneCountInString(c.Description) > MaxDescriptionRunes ||
		strings.ContainsFunc(c.Description, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) {
		return Content{}, invalid("description: want at most %d characters, without control characters other than newlines and tabs", MaxDescriptionRunes)
	}
	if c.DefaultIntervalSeconds == 0 {
		c.DefaultIntervalSeconds = DefaultIntervalSeconds
	}
	if c.DefaultIntervalSeconds < MinIntervalSeconds || c.DefaultIntervalSeconds > MaxIntervalSeconds {
		return Content{}, invalid("default_interval_seconds %d: want %d to %d", c.DefaultIntervalSeconds, MinIntervalSeconds, MaxIntervalSeconds)
	}
	if c.TimeoutMS < 0 || c.TimeoutMS > MaxTimeoutMS {
		return Content{}, invalid("timeout_ms %d: want 1 to %d, or none for the collector's default", c.TimeoutMS, MaxTimeoutMS)
	}
	if len(c.QueryTemplate) > MaxQueryBytes || !utf8.ValidString(c.QueryTemplate) {
		return Content{}, invalid("query_template: want at most %d bytes of UTF-8", MaxQueryBytes)
	}
	// The collector's own check, so the API stores no query the collector
	// would refuse to load. Defense in depth: the collector's read-only
	// login is the control.
	if err := config.ValidateReadOnlySelect(c.QueryTemplate); err != nil {
		return Content{}, invalid("query_template must be one read-only SELECT: it %v", err)
	}
	return c, nil
}

// contentOf returns what a custom definition's version holds.
func contentOf(d Definition) Content {
	return Content{
		Overrides: d.Overrides, Description: d.Description, DefaultIntervalSeconds: d.DefaultIntervalSeconds,
		TimeoutMS: d.TimeoutMS, QueryTemplate: d.QueryTemplate,
	}
}

func definition(row store.ProbeDefinition, latest bool) Definition {
	return Definition{
		ID: uuid.UUID(row.ID.Bytes), Engine: row.Engine, Name: row.Name, Version: int(row.Version), Builtin: row.Builtin,
		Overrides: row.Overrides.String, Category: row.Category, Description: row.Description,
		DefaultIntervalSeconds: int(row.DefaultIntervalSeconds), TimeoutMS: int(row.TimeoutMs.Int32),
		QueryTemplate: row.QueryTemplate.String, Enabled: row.Enabled, Latest: latest,
		CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time,
	}
}

func pgUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" // unique_violation
}
