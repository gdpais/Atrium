package inventory

import (
	"context"
	"fmt"
	"maps"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"heartbeat/apps/api/internal/audit"
	"heartbeat/apps/api/internal/store"
	"heartbeat/internal/config"
)

// DatabaseTarget is a monitored database as the API shows it, and as its
// audit records hold it. It holds a credential reference, never a
// credential: the collector resolves the reference from its own mounted
// Secret (ADR 0014 §6), and the API never connects to the database (ADR
// 0013 §1).
type DatabaseTarget struct {
	ID uuid.UUID `json:"id"`
	// EnvironmentID is the environment it was created in, for good.
	EnvironmentID uuid.UUID `json:"environment_id"`
	Engine        string    `json:"engine"`
	// Name is the target label of its metrics, unique in its environment;
	// it cannot change.
	Name string `json:"name"`
	Host string `json:"host"`
	Port int    `json:"port"`
	// DatabaseName is "" for the login's default database.
	DatabaseName  string            `json:"database_name"`
	CredentialRef string            `json:"credential_ref"`
	Status        string            `json:"status"`
	Labels        map[string]string `json:"labels"`
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
}

// NewDatabaseTarget is a target to create in EnvironmentID. Engine
// defaults to sqlserver, Port to its default port, Status to active.
type NewDatabaseTarget struct {
	EnvironmentID uuid.UUID
	Engine        string
	Name          string
	Host          string
	// Port is 0 for the engine's default port.
	Port          int
	DatabaseName  string
	CredentialRef string
	Status        string
	Labels        map[string]string
}

// DatabaseTargetChange holds the fields to change; nil keeps a field. The
// environment, engine and name cannot change; an empty DatabaseName clears
// it.
type DatabaseTargetChange struct {
	Host          *string
	Port          *int
	DatabaseName  *string
	CredentialRef *string
	Status        *string
	Labels        *map[string]string
}

// DatabaseTargetFilter narrows a list of targets; zero values match all.
type DatabaseTargetFilter struct {
	EnvironmentID uuid.UUID
	Status        string
}

// DatabaseTargetPage is one page of targets.
type DatabaseTargetPage struct {
	Items []DatabaseTarget
	// NextCursor is the next page's cursor; "" on the last page.
	NextCursor string
}

// Engines are the engines a target may have: the collector kinds the API
// renders assignments to (ADR 0014).
var Engines = []string{config.KindSQLServer}

// defaultPorts are the engines' default ports.
var defaultPorts = map[string]int{config.KindSQLServer: 1433}

// CredentialRefPattern is the form of a target's credential reference: kv/,
// secret/ or env/, then a name of lower-case letters, digits and '-' that
// starts with a letter or a digit, at most 200. It is stricter than the
// collector's own check (config.ValidSecretRef), which the API runs too.
// The collector reads the key credentials.Key(ref), the reference
// upper-cased with '/', '-' and '.' as '_' (env/finance-prod is
// HEARTBEAT_CREDENTIAL_ENV_FINANCE_PROD), so this form gives every key one
// spelling: env/Finance.Prod cannot reach env/finance-prod's credential
// while looking like another reference. The key is a valid Kubernetes
// Secret key and environment variable name, and the form leaves no room
// for a username:password or a URL pasted in by mistake.
const CredentialRefPattern = `^(kv|secret|env)/[a-z0-9][a-z0-9-]{0,199}$`

var credentialRefRE = regexp.MustCompile(CredentialRefPattern)

// Limits of a target's fields.
const (
	// maxHostBytes is a DNS name's limit.
	maxHostBytes = 253
	// maxDatabaseNameRunes is SQL Server's limit (sysname).
	maxDatabaseNameRunes = 128
)

func databaseTarget(row store.DatabaseTarget) (DatabaseTarget, error) {
	labels, err := decodeLabels(row.Labels)
	if err != nil {
		return DatabaseTarget{}, err
	}
	return DatabaseTarget{
		ID: goUUID(row.ID), EnvironmentID: goUUID(row.EnvironmentID), Engine: row.Engine, Name: row.Name,
		Host: row.Host, Port: int(row.Port), DatabaseName: row.DatabaseName.String, CredentialRef: row.CredentialRef,
		Status: row.Status, Labels: labels, CreatedAt: goTime(row.CreatedAt), UpdatedAt: goTime(row.UpdatedAt),
	}, nil
}

// ListDatabaseTargets returns a page of the targets filter matches, by
// name, then ID.
func (s *Service) ListDatabaseTargets(ctx context.Context, filter DatabaseTargetFilter, p Page) (DatabaseTargetPage, error) {
	limit, after, err := p.resolve()
	if err != nil {
		return DatabaseTargetPage{}, err
	}
	params := store.ListDatabaseTargetsParams{RowLimit: int32(limit + 1)}
	if filter.EnvironmentID != uuid.Nil {
		params.EnvironmentID = pgUUID(filter.EnvironmentID)
	}
	if filter.Status != "" {
		if _, err := oneOf("status", filter.Status, "", statuses); err != nil {
			return DatabaseTargetPage{}, err
		}
		params.Status = pgText(filter.Status)
	}
	if after != nil {
		params.AfterName = pgtype.Text{String: after.Name, Valid: true}
		params.AfterID = pgUUID(after.ID)
	}
	rows, err := store.New(s.db).ListDatabaseTargets(ctx, params)
	if err != nil {
		return DatabaseTargetPage{}, fmt.Errorf("list database targets: %w", err)
	}
	rows, next := page(rows, limit, func(r store.DatabaseTarget) (string, uuid.UUID) { return r.Name, goUUID(r.ID) })
	out := DatabaseTargetPage{Items: make([]DatabaseTarget, len(rows)), NextCursor: next}
	for i, row := range rows {
		if out.Items[i], err = databaseTarget(row); err != nil {
			return DatabaseTargetPage{}, err
		}
	}
	return out, nil
}

// GetDatabaseTarget returns the target with id, or ErrNotFound.
func (s *Service) GetDatabaseTarget(ctx context.Context, id uuid.UUID) (DatabaseTarget, error) {
	row, err := store.New(s.db).GetDatabaseTarget(ctx, pgUUID(id))
	if err != nil {
		return DatabaseTarget{}, notFound(err)
	}
	return databaseTarget(row)
}

// CreateDatabaseTarget creates a target in its environment and audits it.
// It fails with an OwnerError when the environment does not exist, and with
// a ConflictError when the environment has a target of that name, or one
// for the same database.
func (s *Service) CreateDatabaseTarget(ctx context.Context, caller Caller, in NewDatabaseTarget) (DatabaseTarget, error) {
	t, err := newDatabaseTarget(in)
	if err != nil {
		return DatabaseTarget{}, err
	}
	encoded, err := encodeLabels(t.Labels)
	if err != nil {
		return DatabaseTarget{}, err
	}
	var created DatabaseTarget
	err = s.change(ctx, caller, audit.EntityDatabaseTarget, func(q *store.Queries, record func(string, string, any, any) error) error {
		row, err := q.CreateDatabaseTarget(ctx, store.CreateDatabaseTargetParams{
			EnvironmentID: pgUUID(t.EnvironmentID), Engine: t.Engine, Name: t.Name, Host: t.Host, Port: int32(t.Port),
			DatabaseName: pgText(t.DatabaseName), Status: t.Status, CredentialRef: t.CredentialRef, Labels: encoded,
		})
		if c := constraint(err, foreignKeyViolation); c != "" {
			return &OwnerError{Detail: fmt.Sprintf("environment %s does not exist", t.EnvironmentID)}
		}
		if err := targetTaken(err, t); err != nil {
			return err
		}
		if created, err = databaseTarget(row); err != nil {
			return err
		}
		return record(audit.ActionCreate, created.ID.String(), nil, created)
	})
	if err != nil {
		return DatabaseTarget{}, err
	}
	return created, nil
}

// UpdateDatabaseTarget changes the fields change holds and audits it, and
// returns the target as it is then. A change to its host, port, database or
// credential reference sends its approved probe assignments back to draft,
// each audited, so that the collector never runs against a server, or with
// a credential, that nobody approved. A change that changes nothing writes
// and audits nothing. It fails with ErrNotFound for an unknown id, and with
// a ConflictError when another target of the environment is for the same
// database.
func (s *Service) UpdateDatabaseTarget(ctx context.Context, caller Caller, id uuid.UUID, change DatabaseTargetChange) (DatabaseTarget, error) {
	var after DatabaseTarget
	err := s.change(ctx, caller, audit.EntityDatabaseTarget, func(q *store.Queries, record func(string, string, any, any) error) error {
		row, err := q.LockDatabaseTarget(ctx, pgUUID(id))
		if err != nil {
			return notFound(err)
		}
		if err := managedByFile(row.ManagedBy, "database target"); err != nil {
			return err
		}
		before, err := databaseTarget(row)
		if err != nil {
			return err
		}
		next, err := applyDatabaseTargetChange(before, change)
		if err != nil {
			return err
		}
		if sameDatabaseTarget(next, before) {
			after = before
			return nil
		}
		encoded, err := encodeLabels(next.Labels)
		if err != nil {
			return err
		}
		row, err = q.UpdateDatabaseTarget(ctx, store.UpdateDatabaseTargetParams{
			ID: row.ID, Host: next.Host, Port: int32(next.Port), DatabaseName: pgText(next.DatabaseName),
			Status: next.Status, CredentialRef: next.CredentialRef, Labels: encoded,
		})
		if err := targetTaken(err, next); err != nil {
			return err
		}
		if after, err = databaseTarget(row); err != nil {
			return err
		}
		if err := record(audit.ActionUpdate, after.ID.String(), before, after); err != nil {
			return err
		}
		if !sameConnection(before, after) {
			return resetApprovals(ctx, q, caller, after.ID)
		}
		return nil
	})
	if err != nil {
		return DatabaseTarget{}, err
	}
	return after, nil
}

// DeleteDatabaseTarget deletes the target with id and audits it. Its probe
// assignments would go with it (on delete cascade), so it is refused, with
// a ConflictError, while it has any: disable it instead. It fails with
// ErrNotFound for an unknown id.
func (s *Service) DeleteDatabaseTarget(ctx context.Context, caller Caller, id uuid.UUID) error {
	return s.change(ctx, caller, audit.EntityDatabaseTarget, func(q *store.Queries, record func(string, string, any, any) error) error {
		// The lock holds back a new assignment, whose foreign key check
		// waits for it, until this one commits.
		row, err := q.LockDatabaseTarget(ctx, pgUUID(id))
		if err != nil {
			return notFound(err)
		}
		if err := managedByFile(row.ManagedBy, "database target"); err != nil {
			return err
		}
		before, err := databaseTarget(row)
		if err != nil {
			return err
		}
		n, err := q.CountDatabaseTargetAssignments(ctx, row.ID)
		if err != nil {
			return fmt.Errorf("count the database target's assignments: %w", err)
		}
		if n > 0 {
			return &ConflictError{Detail: fmt.Sprintf("the database target has %d probe assignments, so it cannot be deleted: disable it instead", n)}
		}
		if _, err := q.DeleteDatabaseTarget(ctx, row.ID); err != nil {
			return inUse(err, "database target", "disable it instead")
		}
		return record(audit.ActionDelete, before.ID.String(), before, nil)
	})
}

// targetTaken turns a unique violation of a create or an update into a
// ConflictError that says which; it returns any other error as is.
func targetTaken(err error, t DatabaseTarget) error {
	switch constraint(err, uniqueViolation) {
	case "":
		if err != nil {
			return fmt.Errorf("store the database target: %w", err)
		}
		return nil
	case "database_targets_environment_name_uq":
		return &ConflictError{Detail: fmt.Sprintf("the environment already has a database target named %q", t.Name)}
	default:
		return &ConflictError{Detail: fmt.Sprintf("the environment already has a %s target for %s, port %d, database %q", t.Engine, t.Host, t.Port, t.DatabaseName)}
	}
}

func newDatabaseTarget(in NewDatabaseTarget) (DatabaseTarget, error) {
	t := DatabaseTarget{EnvironmentID: in.EnvironmentID}
	var err error
	if in.EnvironmentID == uuid.Nil {
		return t, invalid("environment_id", "required")
	}
	if t.Engine, err = oneOf("engine", in.Engine, config.KindSQLServer, Engines); err != nil {
		return t, err
	}
	if t.Name, err = targetName(in.Name); err != nil {
		return t, err
	}
	if t.Host, err = targetHost(in.Host); err != nil {
		return t, err
	}
	port := in.Port
	if port == 0 {
		port = defaultPorts[t.Engine]
	}
	if t.Port, err = targetPort(port); err != nil {
		return t, err
	}
	if t.DatabaseName, err = text("database_name", in.DatabaseName, maxDatabaseNameRunes, false); err != nil {
		return t, err
	}
	if t.CredentialRef, err = credentialRef(in.CredentialRef); err != nil {
		return t, err
	}
	if t.Status, err = oneOf("status", in.Status, StatusActive, statuses); err != nil {
		return t, err
	}
	t.Labels, err = labels(in.Labels)
	return t, err
}

func applyDatabaseTargetChange(t DatabaseTarget, c DatabaseTargetChange) (DatabaseTarget, error) {
	var err error
	if c.Host != nil {
		if t.Host, err = targetHost(*c.Host); err != nil {
			return t, err
		}
	}
	if c.Port != nil {
		if t.Port, err = targetPort(*c.Port); err != nil {
			return t, err
		}
	}
	if c.DatabaseName != nil {
		if t.DatabaseName, err = text("database_name", *c.DatabaseName, maxDatabaseNameRunes, false); err != nil {
			return t, err
		}
	}
	if c.CredentialRef != nil {
		if t.CredentialRef, err = credentialRef(*c.CredentialRef); err != nil {
			return t, err
		}
	}
	if c.Status != nil {
		if t.Status, err = oneOf("status", *c.Status, "", statuses); err != nil {
			return t, err
		}
	}
	if c.Labels != nil {
		if t.Labels, err = labels(*c.Labels); err != nil {
			return t, err
		}
	}
	return t, nil
}

// sameConnection reports whether a change leaves where a target is, and
// how the collector logs in to it, as they are.
func sameConnection(a, b DatabaseTarget) bool {
	return a.Host == b.Host && a.Port == b.Port && a.DatabaseName == b.DatabaseName && a.CredentialRef == b.CredentialRef
}

// resetApprovals sends a target's approved assignments back to draft, and
// audits each, after its host, port, database or credential reference
// changed: an approval covers the target as it was, and the collector must
// not connect, with a credential, to a server nobody approved (ADR 0014 §4).
func resetApprovals(ctx context.Context, q *store.Queries, caller Caller, target uuid.UUID) error {
	rows, err := q.ListApprovedTargetAssignments(ctx, pgUUID(target))
	if err != nil {
		return fmt.Errorf("list the target's approved assignments: %w", err)
	}
	for _, row := range rows {
		reset, err := q.ResetProbeAssignment(ctx, row.ID)
		if err != nil {
			return fmt.Errorf("reset probe assignment %s: %w", goUUID(row.ID), err)
		}
		before, after := probeAssignment(row), probeAssignment(reset)
		if _, err := audit.Write(ctx, q, audit.Change{
			RequestID: caller.RequestID, Actor: caller.Actor, Action: audit.ActionUpdate,
			EntityType: audit.EntityProbeAssignment, EntityID: after.ID.String(), Before: before, After: after,
		}); err != nil {
			return err
		}
	}
	return nil
}

// sameDatabaseTarget reports whether a change leaves the fields it can
// change as they are.
func sameDatabaseTarget(a, b DatabaseTarget) bool {
	return a.Host == b.Host && a.Port == b.Port && a.DatabaseName == b.DatabaseName &&
		a.CredentialRef == b.CredentialRef && a.Status == b.Status && maps.Equal(a.Labels, b.Labels)
}

// targetName checks a target's name: the target label of its metrics, so
// a slug like an environment's (SlugPattern).
func targetName(v string) (string, error) {
	if !slugRE.MatchString(v) {
		return "", invalid("name", "%q: want lower-case letters, digits and hyphens, 1 to 63, starting and ending with a letter or a digit, such as finance-prod", v)
	}
	return v, nil
}

// targetHost checks a host as the collector does (config.ValidHost), so it
// joins with the port into a connection URL the driver parses: a DNS name,
// an IPv4 address or a bare IPv6 address, with no port, brackets,
// credentials or path. It is stored in lower case, as DNS compares names,
// and an IPv6 address in its canonical form, so that one server has one
// spelling for the identity index.
func targetHost(v string) (string, error) {
	if v == "" || len(v) > maxHostBytes || !config.ValidHost(v) {
		return "", invalid("host", "want a host name, an IPv4 address or a bare IPv6 address, at most %d characters, with no port, brackets, credentials or path; set the port in port", maxHostBytes)
	}
	if strings.Contains(v, ":") {
		// ValidHost let it through only as an IPv6 address.
		return net.ParseIP(v).String(), nil
	}
	return strings.ToLower(v), nil
}

func targetPort(v int) (int, error) {
	if v < 1 || v > 65535 {
		return 0, invalid("port", "%d: want 1 to 65535", v)
	}
	return v, nil
}

// credentialRef checks a credential reference. Its error does not quote
// the value: a credential pasted in by mistake stays out of the answer and
// the logs.
func credentialRef(v string) (string, error) {
	if !credentialRefRE.MatchString(v) || !config.ValidSecretRef(v) {
		return "", invalid("credential_ref", "want kv/, secret/ or env/ and a name of lower-case letters, digits and '-', at most 200, such as env/finance-prod: a reference to the collector's credentials Secret, never a username or password")
	}
	return v, nil
}
