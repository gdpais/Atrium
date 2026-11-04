package inventory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"heartbeat/apps/api/internal/audit"
	"heartbeat/apps/api/internal/collectors"
	"heartbeat/apps/api/internal/store"
	configschema "heartbeat/packages/config-schema"
)

// Ingest modes (telemetry_sources.ingest_mode).
const (
	IngestAgentless = "agentless"
	IngestOTLP      = "otlp"
)

// SourceOutSystems is the OutSystems source type (ADR 0017).
const SourceOutSystems = "outsystems"

// ingestModes are the ingest modes each source type takes. Every type with
// a schema in packages/config-schema has an entry; a test keeps them equal.
var ingestModes = map[string][]string{
	// OutSystems 11 self-managed is read from its logging database by an
	// outsystems_logs collector; OutSystems Cloud and ODC send OTLP.
	SourceOutSystems: {IngestAgentless, IngestOTLP},
}

// Observation statuses.
const (
	// Observed: an enabled collector extracts the source's environment.
	Observed = "observed"
	// NotObserved: the collector answered, and none of its enabled
	// collectors extracts the environment. Registering a source before its
	// extraction is set up is allowed; this says it is not yet.
	NotObserved = "not_observed"
	// ObservationUnknown: the collector could not be asked.
	ObservationUnknown = "unknown"
	// NotApplicable: no Heartbeat collector reads this kind of source; an
	// otlp source sends its telemetry itself.
	NotApplicable = "not_applicable"
)

// Source is a telemetry source as the API shows it, and, without its
// observation, as its audit records hold it.
type Source struct {
	ID uuid.UUID `json:"id"`
	// ApplicationID is the application it was created for, for good.
	ApplicationID uuid.UUID `json:"application_id"`
	// EnvironmentID is its application's environment, read through the
	// application: a source stores none (ADR 0017 §2).
	EnvironmentID uuid.UUID      `json:"environment_id"`
	SourceType    string         `json:"source_type"`
	IngestMode    string         `json:"ingest_mode"`
	Status        string         `json:"status"`
	Config        map[string]any `json:"config"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
	// Observation is whether a collector reads it, found when the source is
	// read; it is not the source's state, and audit records leave it out.
	Observation Observation `json:"-"`

	environmentSlug string
}

// Observation says whether the DB collector observes a source.
type Observation struct {
	Status string
	// Collectors are the IDs of the collectors that extract it.
	Collectors []string
	// CheckedAt is when the collector answered; zero when it did not.
	CheckedAt time.Time
	// Detail says why a source is not observed or why that is unknown.
	Detail string
}

// NewSource is a source to create for ApplicationID. Status defaults to
// active.
type NewSource struct {
	ApplicationID uuid.UUID
	SourceType    string
	IngestMode    string
	Status        string
	Config        map[string]any
}

// SourceChange holds the fields to change; nil keeps a field. The
// application and the source type cannot change; Config replaces the
// config whole.
type SourceChange struct {
	IngestMode *string
	Status     *string
	Config     *map[string]any
}

// SourceFilter narrows a list of sources; zero values match all.
type SourceFilter struct {
	ApplicationID uuid.UUID
	EnvironmentID uuid.UUID
	SourceType    string
	Status        string
}

// SourcePage is one page of sources.
type SourcePage struct {
	Items []Source
	// NextCursor is the next page's cursor; "" on the last page.
	NextCursor string
}

// Observer says which environments the collector's outsystems_logs
// collectors extract: *collectors.Client.
type Observer interface {
	OutSystemsLogs(ctx context.Context) (collectors.Coverage, error)
}

var _ Observer = (*collectors.Client)(nil)

// Option configures a Service.
type Option func(*Service)

// WithObserver makes the Service report whether sources are observed;
// without it, every agentless source's observation is unknown.
func WithObserver(o Observer) Option { return func(s *Service) { s.observer = o } }

// sourceSchemas are the source types' config schemas, compiled once.
var sourceSchemas = func() map[string]*jsonschema.Schema {
	schemas, err := configschema.SourceSchemas()
	if err != nil {
		panic(err) // embedded, and tested
	}
	return schemas
}()

func source(row store.GetSourceRow) (Source, error) {
	config := map[string]any{}
	if err := json.Unmarshal(row.Config, &config); err != nil {
		return Source{}, fmt.Errorf("config: %w", err)
	}
	return Source{
		ID: goUUID(row.ID), ApplicationID: goUUID(row.ApplicationID), EnvironmentID: goUUID(row.EnvironmentID),
		SourceType: row.SourceType, IngestMode: row.IngestMode, Status: row.Status, Config: config,
		CreatedAt: goTime(row.CreatedAt), UpdatedAt: goTime(row.UpdatedAt), environmentSlug: row.EnvironmentSlug,
	}, nil
}

// sourceOf reads a row of telemetry_sources, of an application in the
// environment with environmentID and slug.
func sourceOf(row store.TelemetrySource, environmentID pgtype.UUID, slug string) (Source, error) {
	return source(store.GetSourceRow{
		ID: row.ID, ApplicationID: row.ApplicationID, EnvironmentID: environmentID, EnvironmentSlug: slug,
		SourceType: row.SourceType, IngestMode: row.IngestMode, Status: row.Status, Config: row.Config,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	})
}

// ListSources returns a page of the sources filter matches, by
// application_name, then ID, each with its observation.
func (s *Service) ListSources(ctx context.Context, filter SourceFilter, p Page) (SourcePage, error) {
	limit, after, err := p.resolve()
	if err != nil {
		return SourcePage{}, err
	}
	params := store.ListSourcesParams{RowLimit: int32(limit + 1)}
	if filter.ApplicationID != uuid.Nil {
		params.ApplicationID = pgUUID(filter.ApplicationID)
	}
	if filter.EnvironmentID != uuid.Nil {
		params.EnvironmentID = pgUUID(filter.EnvironmentID)
	}
	if filter.SourceType != "" {
		if _, err := sourceType(filter.SourceType); err != nil {
			return SourcePage{}, err
		}
		params.SourceType = pgText(filter.SourceType)
	}
	if filter.Status != "" {
		if _, err := oneOf("status", filter.Status, "", statuses); err != nil {
			return SourcePage{}, err
		}
		params.Status = pgText(filter.Status)
	}
	if after != nil {
		params.AfterName = pgtype.Text{String: after.Name, Valid: true}
		params.AfterID = pgUUID(after.ID)
	}
	rows, err := store.New(s.db).ListSources(ctx, params)
	if err != nil {
		return SourcePage{}, fmt.Errorf("list telemetry sources: %w", err)
	}
	rows, next := page(rows, limit, func(r store.ListSourcesRow) (string, uuid.UUID) {
		name, _ := applicationName(r.Config)
		return name, goUUID(r.ID)
	})
	out := SourcePage{Items: make([]Source, len(rows)), NextCursor: next}
	for i, row := range rows {
		if out.Items[i], err = source(store.GetSourceRow(row)); err != nil {
			return SourcePage{}, err
		}
	}
	s.observe(ctx, out.Items)
	return out, nil
}

// GetSource returns the source with id, with its observation, or
// ErrNotFound.
func (s *Service) GetSource(ctx context.Context, id uuid.UUID) (Source, error) {
	row, err := store.New(s.db).GetSource(ctx, pgUUID(id))
	if err != nil {
		return Source{}, notFound(err)
	}
	src, err := source(row)
	if err != nil {
		return Source{}, err
	}
	s.observeOne(ctx, &src)
	return src, nil
}

// CreateSource creates a source for its application and audits it, and
// returns it with its observation. It fails with an OwnerError when the
// application does not exist, and with a ConflictError when another
// application of the environment has a source with the same
// application_name, or the application has one of the same type, ingest
// mode and name.
func (s *Service) CreateSource(ctx context.Context, caller Caller, in NewSource) (Source, error) {
	params, name, err := newSourceParams(in)
	if err != nil {
		return Source{}, err
	}
	noApplication := &OwnerError{Detail: fmt.Sprintf("application %s does not exist", in.ApplicationID)}
	var created Source
	err = s.change(ctx, caller, audit.EntityTelemetrySource, func(q *store.Queries, record func(string, string, any, any) error) error {
		// The application's environment never changes, so reading it
		// unlocked is safe; the insert's foreign key check keeps the
		// application until the transaction ends.
		app, err := q.GetApplication(ctx, params.ApplicationID)
		if errors.Is(err, pgx.ErrNoRows) {
			return noApplication
		}
		if err != nil {
			return fmt.Errorf("read the application: %w", err)
		}
		env, err := q.GetEnvironment(ctx, app.EnvironmentID)
		if err != nil {
			return fmt.Errorf("read the environment: %w", err)
		}
		if err := claimName(ctx, q, app.EnvironmentID, pgtype.UUID{}, params.ApplicationID, params.SourceType, params.IngestMode, name); err != nil {
			return err
		}
		row, err := q.CreateSource(ctx, params)
		if constraint(err, foreignKeyViolation) != "" {
			return noApplication
		}
		if err != nil {
			return fmt.Errorf("create the telemetry source: %w", err)
		}
		if created, err = sourceOf(row, app.EnvironmentID, env.Slug); err != nil {
			return err
		}
		return record(audit.ActionCreate, created.ID.String(), nil, created)
	})
	if err != nil {
		return Source{}, err
	}
	s.observeOne(ctx, &created)
	return created, nil
}

func newSourceParams(in NewSource) (store.CreateSourceParams, string, error) {
	p := store.CreateSourceParams{ApplicationID: pgUUID(in.ApplicationID)}
	var err error
	if in.ApplicationID == uuid.Nil {
		return p, "", invalid("application_id", "required")
	}
	if p.SourceType, err = sourceType(in.SourceType); err != nil {
		return p, "", err
	}
	if p.IngestMode, err = ingestMode(p.SourceType, in.IngestMode); err != nil {
		return p, "", err
	}
	if p.Status, err = oneOf("status", in.Status, StatusActive, statuses); err != nil {
		return p, "", err
	}
	var name string
	p.Config, name, err = sourceConfig(p.SourceType, in.Config)
	return p, name, err
}

// UpdateSource changes the fields change holds and audits it, and returns
// the source as it is then, with its observation. A change that changes
// nothing writes and audits nothing. It fails with ErrNotFound for an
// unknown id, and with a ConflictError as CreateSource does.
func (s *Service) UpdateSource(ctx context.Context, caller Caller, id uuid.UUID, change SourceChange) (Source, error) {
	var after Source
	err := s.change(ctx, caller, audit.EntityTelemetrySource, func(q *store.Queries, record func(string, string, any, any) error) error {
		// Lock the environment before the source, in the order a creation
		// takes them. The source's environment cannot change, so reading it
		// first, unlocked, is safe.
		current, err := q.GetSource(ctx, pgUUID(id))
		if err != nil {
			return notFound(err)
		}
		if _, err := q.LockSourcesOfEnvironment(ctx, current.EnvironmentID); err != nil {
			return fmt.Errorf("lock the environment: %w", err)
		}
		locked, err := q.LockSource(ctx, pgUUID(id))
		if err != nil {
			return notFound(err)
		}
		before, err := source(store.GetSourceRow(locked))
		if err != nil {
			return err
		}
		next := before
		if change.IngestMode != nil {
			if next.IngestMode, err = ingestMode(before.SourceType, *change.IngestMode); err != nil {
				return err
			}
		}
		if change.Status != nil {
			if next.Status, err = oneOf("status", *change.Status, "", statuses); err != nil {
				return err
			}
		}
		// The stored config is checked when it was written; a change of
		// status or ingest mode alone does not check it again, so a schema
		// tightened later never stops an operator disabling a source.
		beforeConfig, err := json.Marshal(before.Config)
		if err != nil {
			return fmt.Errorf("config: %w", err)
		}
		config, name := beforeConfig, ""
		if change.Config != nil {
			if config, name, err = sourceConfig(before.SourceType, *change.Config); err != nil {
				return err
			}
		} else if name, _ = applicationName(beforeConfig); name == "" {
			return fmt.Errorf("stored config of source %s has no application_name", before.ID)
		}
		if next.IngestMode == before.IngestMode && next.Status == before.Status && jsonEqual(config, beforeConfig) {
			after = before
			return nil
		}
		if err := claimName(ctx, q, locked.EnvironmentID, locked.ID, locked.ApplicationID, before.SourceType, next.IngestMode, name); err != nil {
			return err
		}
		row, err := q.UpdateSource(ctx, store.UpdateSourceParams{
			ID: locked.ID, IngestMode: next.IngestMode, Status: next.Status, Config: config,
		})
		if err != nil {
			return fmt.Errorf("update the telemetry source: %w", err)
		}
		if after, err = sourceOf(row, locked.EnvironmentID, locked.EnvironmentSlug); err != nil {
			return err
		}
		return record(audit.ActionUpdate, after.ID.String(), before, after)
	})
	if err != nil {
		return Source{}, err
	}
	s.observeOne(ctx, &after)
	return after, nil
}

// DeleteSource deletes the source with id and audits it. It fails with
// ErrNotFound for an unknown id.
func (s *Service) DeleteSource(ctx context.Context, caller Caller, id uuid.UUID) error {
	return s.change(ctx, caller, audit.EntityTelemetrySource, func(q *store.Queries, record func(string, string, any, any) error) error {
		locked, err := q.LockSource(ctx, pgUUID(id))
		if err != nil {
			return notFound(err)
		}
		before, err := source(store.GetSourceRow(locked))
		if err != nil {
			return err
		}
		if _, err := q.DeleteSource(ctx, locked.ID); err != nil {
			return inUse(err, "telemetry source", "delete what refers to it first")
		}
		return record(audit.ActionDelete, before.ID.String(), before, nil)
	})
}

// claimName checks, with the environment's source lock held, that
// application_name is free for the application in its environment: no
// source of another application names it (ADR 0017 §2), and no other
// source of this application has the same type, ingest mode and name. self
// is the source being changed, if any.
func claimName(ctx context.Context, q *store.Queries, environmentID, self, applicationID pgtype.UUID, sourceType, mode, name string) error {
	if _, err := q.LockSourcesOfEnvironment(ctx, environmentID); err != nil {
		return fmt.Errorf("lock the environment: %w", err)
	}
	named, err := q.SourcesNamed(ctx, store.SourcesNamedParams{EnvironmentID: environmentID, ApplicationName: name})
	if err != nil {
		return fmt.Errorf("find the sources named %q: %w", name, err)
	}
	for _, other := range named {
		switch {
		case self.Valid && other.ID == self:
		case other.ApplicationID != applicationID:
			return &ConflictError{Detail: fmt.Sprintf("in this environment, application_name %q is application %s's: a name maps to one application", name, goUUID(other.ApplicationID))}
		case other.SourceType == sourceType && other.IngestMode == mode:
			return &ConflictError{Detail: fmt.Sprintf("the application already has a %s %s source for %q", sourceType, mode, name)}
		}
	}
	return nil
}

func sourceType(v string) (string, error) {
	if _, ok := sourceSchemas[v]; !ok {
		return "", invalid("source_type", "%q: want one of %s", v, strings.Join(configschema.SourceTypes(), ", "))
	}
	return v, nil
}

func ingestMode(sourceType, v string) (string, error) {
	if !slices.Contains(ingestModes[sourceType], v) {
		return "", invalid("ingest_mode", "%q: a %s source takes %s", v, sourceType, strings.Join(ingestModes[sourceType], " or "))
	}
	return v, nil
}

// sourceConfig checks config against its source type's schema and returns
// it as JSON, with its application_name.
func sourceConfig(sourceType string, config map[string]any) ([]byte, string, error) {
	if config == nil {
		config = map[string]any{}
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return nil, "", invalid("config", "not JSON: %v", err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, "", invalid("config", "not JSON: %v", err)
	}
	if err := sourceSchemas[sourceType].Validate(doc); err != nil {
		return nil, "", invalid("config", "not valid for source_type %s (packages/config-schema/src/telemetry-sources/%s.schema.json): %s", sourceType, sourceType, schemaProblems(err))
	}
	name, ok := applicationName(raw)
	if !ok {
		return nil, "", invalid("config", "application_name: required")
	}
	return raw, name, nil
}

// schemaProblems lists what a validation error found, one cause per item,
// without the first line, which names the schema's internal URL.
func schemaProblems(err error) string {
	lines := strings.Split(strings.TrimSpace(err.Error()), "\n")
	if len(lines) > 1 {
		lines = lines[1:]
	}
	for i, line := range lines {
		lines[i] = strings.TrimPrefix(strings.TrimSpace(line), "- ")
	}
	return strings.Join(lines, "; ")
}

// applicationName reads a config's application_name.
func applicationName(raw []byte) (string, bool) {
	var c struct {
		ApplicationName string `json:"application_name"`
	}
	if json.Unmarshal(raw, &c) != nil || c.ApplicationName == "" {
		return "", false
	}
	return c.ApplicationName, true
}

func jsonEqual(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	ax, _ := json.Marshal(x)
	by, _ := json.Marshal(y)
	return bytes.Equal(ax, by)
}

// observe sets each source's observation, asking the collector once at
// most.
func (s *Service) observe(ctx context.Context, sources []Source) {
	var coverage *collectors.Coverage
	var askErr error
	for i := range sources {
		src := &sources[i]
		if src.SourceType != SourceOutSystems || src.IngestMode != IngestAgentless {
			detail := "no Heartbeat collector reads a " + src.SourceType + " " + src.IngestMode + " source"
			if src.IngestMode == IngestOTLP {
				detail = "an otlp source sends its telemetry itself; no collector reads it"
			}
			src.Observation = Observation{Status: NotApplicable, Collectors: []string{}, Detail: detail}
			continue
		}
		if coverage == nil && askErr == nil {
			if s.observer == nil {
				askErr = collectors.ErrNotConfigured
			} else {
				var c collectors.Coverage
				if c, askErr = s.observer.OutSystemsLogs(ctx); askErr == nil {
					coverage = &c
				}
			}
		}
		if askErr != nil {
			src.Observation = Observation{Status: ObservationUnknown, Collectors: []string{}, Detail: askErr.Error()}
			continue
		}
		ids := coverage.ByEnvironment[src.environmentSlug]
		if len(ids) == 0 {
			src.Observation = Observation{Status: NotObserved, Collectors: []string{}, CheckedAt: coverage.CheckedAt,
				Detail: fmt.Sprintf("no enabled outsystems_logs collector extracts environment %q", src.environmentSlug)}
			continue
		}
		src.Observation = Observation{Status: Observed, Collectors: slices.Clone(ids), CheckedAt: coverage.CheckedAt}
	}
}

func (s *Service) observeOne(ctx context.Context, src *Source) {
	one := []Source{*src}
	s.observe(ctx, one)
	src.Observation = one[0].Observation
}
