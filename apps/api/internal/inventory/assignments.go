package inventory

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"heartbeat/apps/api/internal/audit"
	"heartbeat/apps/api/internal/store"
	"heartbeat/internal/config"
)

// Probe assignment states (ADR 0014 §4). PostgreSQL holds the request; the
// rendered values file is the desired state and the collector's admin
// config the observed state.
const (
	// StateDraft is created or changed and not yet approved: not rendered.
	StateDraft = "draft"
	// StateApproved is approved by an admin: rendered into managedCollectors.
	StateApproved = "approved"
	// StateProposed is in an open pull request to the config repository.
	// It comes with the pull request output, which waits for the config
	// repository [18].
	StateProposed = "proposed"
	// StateApplied is run by the collector as rendered.
	StateApplied = "applied"
	// StateRejected is refused by an admin, or its pull request was closed
	// unmerged: not rendered.
	StateRejected = "rejected"
	// StateFailed is refused by the config repository's checks or by the
	// collector. It comes with the pull request output.
	StateFailed = "failed"
)

// AssignmentStates lists every state.
var AssignmentStates = []string{StateDraft, StateApproved, StateProposed, StateApplied, StateRejected, StateFailed}

// approvedStates are the states whose enabled assignments a render holds.
var approvedStates = []string{StateApproved, StateProposed, StateApplied}

// Limits of an assignment's probe policy.
const (
	// MinIntervalSeconds and MaxIntervalSeconds bound an interval, as they
	// bound a definition's default.
	MinIntervalSeconds = 5
	MaxIntervalSeconds = 3600
	// maxStateDetailRunes bounds the reason a rejection gives.
	maxStateDetailRunes = 1000
)

// MaxTimeoutMS is the longest timeout an assignment may set: the collector
// refuses a longer one ([config.MaxProbeTimeout]).
var MaxTimeoutMS = int(config.MaxProbeTimeout.Milliseconds())

// ProbeAssignment is a probe definition version run against a database
// target, as the API shows it and as its audit records hold it.
type ProbeAssignment struct {
	ID               uuid.UUID `json:"id"`
	DatabaseTargetID uuid.UUID `json:"database_target_id"`
	// ProbeDefinitionID is the definition version it runs, pinned: a new
	// version of the definition changes nothing here until the assignment
	// is changed to it and approved again.
	ProbeDefinitionID uuid.UUID `json:"probe_definition_id"`
	IntervalSeconds   int       `json:"interval_seconds"`
	// TimeoutMS is 0 to leave the timeout to the definition, and then to
	// the collector: half the interval, at most 10s.
	TimeoutMS int `json:"timeout_ms"`
	// MaxRows is 0 for the collector's limit, 10000.
	MaxRows int    `json:"max_rows"`
	Enabled bool   `json:"enabled"`
	State   string `json:"state"`
	// StateDetail says why it was rejected or failed.
	StateDetail    string `json:"state_detail"`
	PullRequestURL string `json:"pull_request_url,omitempty"`
	// ApprovedBy and ApprovedAt are who approved it and when; nil while it
	// is a draft or rejected.
	ApprovedBy *uuid.UUID `json:"approved_by,omitempty"`
	ApprovedAt *time.Time `json:"approved_at,omitempty"`
	// CreatedBy is the user who created it; nil for none.
	CreatedBy *uuid.UUID `json:"created_by,omitempty"`
	// CollectorID is the collector it renders to: api-<target id>-<interval>s.
	CollectorID string    `json:"collector_id"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// NewProbeAssignment is an assignment to create, as a draft.
type NewProbeAssignment struct {
	DatabaseTargetID  uuid.UUID
	ProbeDefinitionID uuid.UUID
	// IntervalSeconds is 0 for the definition's default interval.
	IntervalSeconds int
	TimeoutMS       int
	MaxRows         int
	// Enabled is nil for true.
	Enabled *bool
}

// ProbeAssignmentChange holds the fields to change; nil keeps a field. A
// TimeoutMS or MaxRows of 0 clears it. ProbeDefinitionID may name another
// version of the same definition only. The target cannot change.
type ProbeAssignmentChange struct {
	ProbeDefinitionID *uuid.UUID
	IntervalSeconds   *int
	TimeoutMS         *int
	MaxRows           *int
	Enabled           *bool
}

// ProbeAssignmentFilter narrows a list of assignments; zero values match all.
type ProbeAssignmentFilter struct {
	DatabaseTargetID uuid.UUID
	State            string
}

// ProbeAssignmentPage is one page of assignments.
type ProbeAssignmentPage struct {
	Items      []ProbeAssignment
	NextCursor string
}

// CollectorID is the ID of the collector the assignments of target at
// interval render to (ADR 0014 §2).
func CollectorID(target uuid.UUID, intervalSeconds int) string {
	return fmt.Sprintf("api-%s-%ds", target, intervalSeconds)
}

func probeAssignment(row store.ProbeAssignment) ProbeAssignment {
	a := ProbeAssignment{
		ID: goUUID(row.ID), DatabaseTargetID: goUUID(row.DatabaseTargetID), ProbeDefinitionID: goUUID(row.ProbeDefinitionID),
		IntervalSeconds: int(row.IntervalSeconds), TimeoutMS: int(row.TimeoutMs.Int32), MaxRows: int(row.MaxRows.Int32),
		Enabled: row.Enabled, State: row.State, StateDetail: row.StateDetail, PullRequestURL: row.PullRequestUrl.String,
		CreatedAt: goTime(row.CreatedAt), UpdatedAt: goTime(row.UpdatedAt),
	}
	if row.ApprovedBy.Valid {
		by := goUUID(row.ApprovedBy)
		a.ApprovedBy = &by
	}
	if row.ApprovedAt.Valid {
		at := goTime(row.ApprovedAt)
		a.ApprovedAt = &at
	}
	if row.CreatedBy.Valid {
		by := goUUID(row.CreatedBy)
		a.CreatedBy = &by
	}
	a.CollectorID = CollectorID(a.DatabaseTargetID, a.IntervalSeconds)
	return a
}

// ListProbeAssignments returns a page of the assignments filter matches, in
// the order they were created.
func (s *Service) ListProbeAssignments(ctx context.Context, filter ProbeAssignmentFilter, p Page) (ProbeAssignmentPage, error) {
	limit, after, err := p.resolve()
	if err != nil {
		return ProbeAssignmentPage{}, err
	}
	params := store.ListProbeAssignmentsParams{RowLimit: int32(limit + 1)}
	if filter.DatabaseTargetID != uuid.Nil {
		params.DatabaseTargetID = pgUUID(filter.DatabaseTargetID)
	}
	if filter.State != "" {
		if _, err := oneOf("state", filter.State, "", AssignmentStates); err != nil {
			return ProbeAssignmentPage{}, err
		}
		params.State = pgText(filter.State)
	}
	if after != nil {
		// The cursor's name is the creation time of the page's last item.
		at, err := time.Parse(time.RFC3339Nano, after.Name)
		if err != nil {
			return ProbeAssignmentPage{}, invalid("cursor", "not one this API returned; start again without it")
		}
		params.AfterCreatedAt = pgtype.Timestamptz{Time: at, Valid: true}
		params.AfterID = pgUUID(after.ID)
	}
	rows, err := store.New(s.db).ListProbeAssignments(ctx, params)
	if err != nil {
		return ProbeAssignmentPage{}, fmt.Errorf("list probe assignments: %w", err)
	}
	rows, next := page(rows, limit, func(r store.ProbeAssignment) (string, uuid.UUID) {
		return goTime(r.CreatedAt).Format(time.RFC3339Nano), goUUID(r.ID)
	})
	out := ProbeAssignmentPage{Items: make([]ProbeAssignment, len(rows)), NextCursor: next}
	for i, row := range rows {
		out.Items[i] = probeAssignment(row)
	}
	return out, nil
}

// GetProbeAssignment returns the assignment with id, or ErrNotFound.
func (s *Service) GetProbeAssignment(ctx context.Context, id uuid.UUID) (ProbeAssignment, error) {
	row, err := store.New(s.db).GetProbeAssignment(ctx, pgUUID(id))
	if err != nil {
		return ProbeAssignment{}, notFound(err)
	}
	return probeAssignment(row), nil
}

// CreateProbeAssignment creates a draft assignment and audits it. It fails
// with an OwnerError when the target or the definition version does not
// exist, and with a ConflictError when the definition is disabled or for
// another engine, or the target has an assignment of the definition, or of
// another that runs the same probe, already.
func (s *Service) CreateProbeAssignment(ctx context.Context, caller Caller, in NewProbeAssignment) (ProbeAssignment, error) {
	if in.DatabaseTargetID == uuid.Nil {
		return ProbeAssignment{}, invalid("database_target_id", "required")
	}
	if in.ProbeDefinitionID == uuid.Nil {
		return ProbeAssignment{}, invalid("probe_definition_id", "required")
	}
	var created ProbeAssignment
	err := s.change(ctx, caller, audit.EntityProbeAssignment, func(q *store.Queries, record func(string, string, any, any) error) error {
		// The target's lock makes changes to its assignments take turns, so
		// the checks below see every other one; it is the lock a target's
		// delete takes too.
		target, err := q.LockDatabaseTarget(ctx, pgUUID(in.DatabaseTargetID))
		if errors.Is(err, pgx.ErrNoRows) {
			return &OwnerError{Detail: fmt.Sprintf("database target %s does not exist", in.DatabaseTargetID)}
		}
		if err != nil {
			return fmt.Errorf("lock the database target: %w", err)
		}
		def, err := assignableDefinition(ctx, q, in.ProbeDefinitionID, target.Engine)
		if err != nil {
			return err
		}
		enabled := true
		if in.Enabled != nil {
			enabled = *in.Enabled
		}
		a := ProbeAssignment{
			DatabaseTargetID: in.DatabaseTargetID, ProbeDefinitionID: in.ProbeDefinitionID,
			IntervalSeconds: in.IntervalSeconds, TimeoutMS: in.TimeoutMS, MaxRows: in.MaxRows, Enabled: enabled,
		}
		if a.IntervalSeconds == 0 {
			a.IntervalSeconds = int(def.DefaultIntervalSeconds)
		}
		if err := checkPolicy(a, def); err != nil {
			return err
		}
		if err := checkProbeTaken(ctx, q, a.DatabaseTargetID, uuid.Nil, def); err != nil {
			return err
		}
		row, err := q.CreateProbeAssignment(ctx, store.CreateProbeAssignmentParams{
			DatabaseTargetID: pgUUID(a.DatabaseTargetID), ProbeDefinitionID: pgUUID(a.ProbeDefinitionID),
			IntervalSeconds: int32(a.IntervalSeconds), TimeoutMs: pgInt(a.TimeoutMS), MaxRows: pgInt(a.MaxRows),
			Enabled: a.Enabled, CreatedBy: userOf(caller),
		})
		if constraint(err, uniqueViolation) != "" {
			return &ConflictError{Detail: "the database target has an assignment of this probe definition version already: change it instead"}
		}
		if err != nil {
			return fmt.Errorf("store the probe assignment: %w", err)
		}
		created = probeAssignment(row)
		return record(audit.ActionCreate, created.ID.String(), nil, created)
	})
	if err != nil {
		return ProbeAssignment{}, err
	}
	return created, nil
}

// UpdateProbeAssignment changes the fields change holds and audits it, and
// returns the assignment as it is then. A change puts it back to draft,
// unapproved: it is left out of renders until it is approved again. A
// change that changes nothing writes and audits nothing. It fails with
// ErrNotFound for an unknown id, with an OwnerError for a definition version
// that does not exist, and with a ConflictError for a version of another
// definition, or a disabled one.
func (s *Service) UpdateProbeAssignment(ctx context.Context, caller Caller, id uuid.UUID, change ProbeAssignmentChange) (ProbeAssignment, error) {
	var after ProbeAssignment
	err := s.changeAssignment(ctx, caller, id, func(q *store.Queries, target store.DatabaseTarget, row store.ProbeAssignment, record func(string, string, any, any) error) error {
		before := probeAssignment(row)
		next := before
		if change.ProbeDefinitionID != nil {
			next.ProbeDefinitionID = *change.ProbeDefinitionID
		}
		if change.IntervalSeconds != nil {
			next.IntervalSeconds = *change.IntervalSeconds
		}
		if change.TimeoutMS != nil {
			next.TimeoutMS = *change.TimeoutMS
		}
		if change.MaxRows != nil {
			next.MaxRows = *change.MaxRows
		}
		if change.Enabled != nil {
			next.Enabled = *change.Enabled
		}
		if next.ProbeDefinitionID == before.ProbeDefinitionID && next.IntervalSeconds == before.IntervalSeconds &&
			next.TimeoutMS == before.TimeoutMS && next.MaxRows == before.MaxRows && next.Enabled == before.Enabled {
			after = before
			return nil
		}
		def, err := assignableDefinition(ctx, q, next.ProbeDefinitionID, target.Engine)
		if err != nil {
			return err
		}
		if next.ProbeDefinitionID != before.ProbeDefinitionID {
			current, err := q.GetProbeDefinition(ctx, row.ProbeDefinitionID)
			if err != nil {
				return fmt.Errorf("read the assignment's probe definition: %w", err)
			}
			if current.ProbeDefinition.Engine != def.Engine || current.ProbeDefinition.Name != def.Name {
				return &ConflictError{Detail: fmt.Sprintf("probe definition version %s is not a version of %s, the definition this assignment runs: create another assignment instead", next.ProbeDefinitionID, current.ProbeDefinition.Name)}
			}
		}
		if err := checkPolicy(next, def); err != nil {
			return err
		}
		updated, err := q.UpdateProbeAssignment(ctx, store.UpdateProbeAssignmentParams{
			ID: row.ID, ProbeDefinitionID: pgUUID(next.ProbeDefinitionID), IntervalSeconds: int32(next.IntervalSeconds),
			TimeoutMs: pgInt(next.TimeoutMS), MaxRows: pgInt(next.MaxRows), Enabled: next.Enabled,
		})
		if constraint(err, uniqueViolation) != "" {
			return &ConflictError{Detail: "the database target has an assignment of this probe definition version already"}
		}
		if err != nil {
			return fmt.Errorf("store the probe assignment: %w", err)
		}
		after = probeAssignment(updated)
		return record(audit.ActionUpdate, after.ID.String(), before, after)
	})
	if err != nil {
		return ProbeAssignment{}, err
	}
	return after, nil
}

// ApproveProbeAssignment approves a draft or rejected assignment and audits
// it: the next render holds it (ADR 0014 §3). The render that holds it is
// made and checked in the same transaction, so an approval that would make
// a file the collector refuses is refused instead. It fails with
// ErrNotFound for an unknown id, and with a ConflictError when the
// assignment is approved already, its definition was disabled since it was
// created, or the target's other approved assignments run at another
// interval: a target runs at one, its collector's scrape_interval.
func (s *Service) ApproveProbeAssignment(ctx context.Context, caller Caller, id uuid.UUID) (ProbeAssignment, error) {
	approver := userOf(caller)
	if !approver.Valid {
		return ProbeAssignment{}, errors.New("approve a probe assignment: the caller is not a user")
	}
	var after ProbeAssignment
	err := s.changeAssignment(ctx, caller, id, func(q *store.Queries, target store.DatabaseTarget, row store.ProbeAssignment, record func(string, string, any, any) error) error {
		before := probeAssignment(row)
		if before.State != StateDraft && before.State != StateRejected {
			return &ConflictError{Detail: fmt.Sprintf("the probe assignment is %s, not a draft or rejected: change it to approve it again", before.State)}
		}
		// A definition disabled since the draft was made is not approved:
		// the render would leave it out, and enabling the definition again
		// would run it on an approval given while it was off.
		if _, err := assignableDefinition(ctx, q, before.ProbeDefinitionID, target.Engine); err != nil {
			return err
		}
		siblings, err := q.ListTargetAssignmentProbes(ctx, store.ListTargetAssignmentProbesParams{DatabaseTargetID: row.DatabaseTargetID, ExceptID: row.ID})
		if err != nil {
			return fmt.Errorf("list the target's assignments: %w", err)
		}
		if before.Enabled {
			for _, other := range siblings {
				if other.Enabled && slices.Contains(approvedStates, other.State) && int(other.IntervalSeconds) != before.IntervalSeconds {
					return &ConflictError{Detail: fmt.Sprintf("the target's approved assignments run every %ds, and a target runs at one interval, its collector's scrape_interval: change this assignment's interval to %d, or change theirs", other.IntervalSeconds, other.IntervalSeconds)}
				}
			}
		}
		updated, err := q.SetProbeAssignmentState(ctx, store.SetProbeAssignmentStateParams{
			ID: row.ID, State: StateApproved, ApprovedBy: approver,
			ApprovedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		})
		if err != nil {
			return fmt.Errorf("approve the probe assignment: %w", err)
		}
		// The render now holds the approval; check it as the collector will.
		var refused *RenderError
		if _, err := render(ctx, q); errors.As(err, &refused) {
			return &ConflictError{Detail: "the approval would render a file the collector refuses: " + refused.Reason}
		} else if err != nil {
			return err
		}
		after = probeAssignment(updated)
		return record(audit.ActionApprove, after.ID.String(), before, after)
	})
	if err != nil {
		return ProbeAssignment{}, err
	}
	return after, nil
}

// RejectProbeAssignment rejects a draft and audits it, with the reason
// given. A rejected assignment is not rendered; it is changed, or approved
// after all, to try again. It fails with ErrNotFound for an unknown id, and
// with a ConflictError when the assignment is not a draft.
func (s *Service) RejectProbeAssignment(ctx context.Context, caller Caller, id uuid.UUID, reason string) (ProbeAssignment, error) {
	reason = strings.TrimSpace(reason)
	if utf8.RuneCountInString(reason) > maxStateDetailRunes || !utf8.ValidString(reason) || strings.ContainsFunc(reason, func(r rune) bool { return unicode.IsControl(r) && r != '\n' }) {
		return ProbeAssignment{}, invalid("reason", "want text of at most %d characters, without control characters", maxStateDetailRunes)
	}
	var after ProbeAssignment
	err := s.changeAssignment(ctx, caller, id, func(q *store.Queries, _ store.DatabaseTarget, row store.ProbeAssignment, record func(string, string, any, any) error) error {
		before := probeAssignment(row)
		if before.State != StateDraft {
			return &ConflictError{Detail: fmt.Sprintf("the probe assignment is %s, not a draft: disable or delete it to take it out of the render", before.State)}
		}
		updated, err := q.SetProbeAssignmentState(ctx, store.SetProbeAssignmentStateParams{ID: row.ID, State: StateRejected, StateDetail: reason})
		if err != nil {
			return fmt.Errorf("reject the probe assignment: %w", err)
		}
		after = probeAssignment(updated)
		return record(audit.ActionReject, after.ID.String(), before, after)
	})
	if err != nil {
		return ProbeAssignment{}, err
	}
	return after, nil
}

// DeleteProbeAssignment deletes the assignment with id and audits it; the
// next render leaves it out. It fails with ErrNotFound for an unknown id.
func (s *Service) DeleteProbeAssignment(ctx context.Context, caller Caller, id uuid.UUID) error {
	return s.changeAssignment(ctx, caller, id, func(q *store.Queries, _ store.DatabaseTarget, row store.ProbeAssignment, record func(string, string, any, any) error) error {
		before := probeAssignment(row)
		if _, err := q.DeleteProbeAssignment(ctx, row.ID); err != nil {
			return fmt.Errorf("delete the probe assignment: %w", err)
		}
		return record(audit.ActionDelete, before.ID.String(), before, nil)
	})
}

// changeAssignment runs fn in a change's transaction with the assignment
// with id and its target, both locked: the target first, as a create and a
// target's delete lock it, so that changes to one target's assignments take
// turns.
func (s *Service) changeAssignment(ctx context.Context, caller Caller, id uuid.UUID, fn func(q *store.Queries, target store.DatabaseTarget, row store.ProbeAssignment, record func(string, string, any, any) error) error) error {
	return s.change(ctx, caller, audit.EntityProbeAssignment, func(q *store.Queries, record func(string, string, any, any) error) error {
		current, err := q.GetProbeAssignment(ctx, pgUUID(id))
		if err != nil {
			return notFound(err)
		}
		target, err := q.LockDatabaseTarget(ctx, current.DatabaseTargetID)
		if err != nil {
			return notFound(err)
		}
		// An assignment never moves to another target, so the one locked is
		// still its target; it may have been deleted meanwhile.
		row, err := q.LockProbeAssignment(ctx, pgUUID(id))
		if err != nil {
			return notFound(err)
		}
		return fn(q, target, row, record)
	})
}

// assignableDefinition returns the definition version id, which must exist
// (an OwnerError), be enabled and be for engine (a ConflictError).
func assignableDefinition(ctx context.Context, q *store.Queries, id uuid.UUID, engine string) (store.ProbeDefinition, error) {
	row, err := q.GetProbeDefinition(ctx, pgUUID(id))
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ProbeDefinition{}, &OwnerError{Detail: fmt.Sprintf("probe definition version %s does not exist", id)}
	}
	if err != nil {
		return store.ProbeDefinition{}, fmt.Errorf("read the probe definition: %w", err)
	}
	def := row.ProbeDefinition
	if !def.Enabled {
		return def, &ConflictError{Detail: fmt.Sprintf("probe definition %s is disabled", def.Name)}
	}
	if def.Engine != engine {
		return def, &ConflictError{Detail: fmt.Sprintf("probe definition %s is for %s, and the target is %s", def.Name, def.Engine, engine)}
	}
	return def, nil
}

// checkPolicy checks an assignment's probe policy as the collector will:
// the interval within bounds, the timeout (its own, or else its
// definition's) within the interval and [MaxTimeoutMS], and the row limit
// within [config.MaxProbeMaxRows].
func checkPolicy(a ProbeAssignment, def store.ProbeDefinition) error {
	if a.IntervalSeconds < MinIntervalSeconds || a.IntervalSeconds > MaxIntervalSeconds {
		return invalid("interval_seconds", "%d: want %d to %d", a.IntervalSeconds, MinIntervalSeconds, MaxIntervalSeconds)
	}
	if a.TimeoutMS < 0 || a.TimeoutMS > MaxTimeoutMS {
		return invalid("timeout_ms", "%d: want 1 to %d, or 0 for the definition's or the collector's", a.TimeoutMS, MaxTimeoutMS)
	}
	timeout, field := a.TimeoutMS, "timeout_ms"
	if timeout == 0 && def.TimeoutMs.Valid {
		timeout, field = int(def.TimeoutMs.Int32), "the definition's timeout_ms"
	}
	// The collector caps a timeout at the interval: one past it is a
	// mistake, not a policy.
	if timeout > a.IntervalSeconds*1000 {
		return invalid("timeout_ms", "%s, %d, is longer than the interval, %ds: shorten it or lengthen the interval", field, timeout, a.IntervalSeconds)
	}
	if a.MaxRows < 0 || a.MaxRows > config.MaxProbeMaxRows {
		return invalid("max_rows", "%d: want 1 to %d, or 0 for the collector's %d", a.MaxRows, config.MaxProbeMaxRows, config.DefaultProbeMaxRows)
	}
	return nil
}

// checkProbeTaken refuses a second assignment, on one target, of a
// definition, or of another that runs the same probe: a custom definition
// runs the built-in probe it overrides, under its name, and a target runs
// each probe once.
func checkProbeTaken(ctx context.Context, q *store.Queries, target, except uuid.UUID, def store.ProbeDefinition) error {
	siblings, err := q.ListTargetAssignmentProbes(ctx, store.ListTargetAssignmentProbesParams{DatabaseTargetID: pgUUID(target), ExceptID: pgUUID(except)})
	if err != nil {
		return fmt.Errorf("list the target's assignments: %w", err)
	}
	probe := runsProbe(def)
	for _, other := range siblings {
		switch {
		case other.Engine == def.Engine && other.Name == def.Name:
			return &ConflictError{Detail: fmt.Sprintf("the database target has an assignment of %s already, %s: change it instead", def.Name, goUUID(other.ID))}
		case other.ProbeName == probe:
			return &ConflictError{Detail: fmt.Sprintf("the database target runs probe %s already, through assignment %s of %s: a target runs each probe once", probe, goUUID(other.ID), other.Name)}
		}
	}
	return nil
}

// runsProbe is the name of the probe a definition runs: its own for a
// built-in, the one it overrides for a custom definition.
func runsProbe(def store.ProbeDefinition) string {
	if def.Overrides.Valid && def.Overrides.String != "" {
		return def.Overrides.String
	}
	return def.Name
}

// userOf is the user a change is made by, or null for another actor.
func userOf(caller Caller) pgtype.UUID {
	if caller.Actor.Type != audit.ActorUser {
		return pgtype.UUID{}
	}
	id, err := uuid.Parse(caller.Actor.UserID)
	if err != nil {
		return pgtype.UUID{}
	}
	return pgUUID(id)
}

func pgInt(v int) pgtype.Int4 { return pgtype.Int4{Int32: int32(v), Valid: v != 0} }
