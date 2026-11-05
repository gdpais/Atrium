package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"heartbeat/apps/api/internal/audit"
	"heartbeat/apps/api/internal/store"
)

// ApplyResult is what applying an inventory file changed.
type ApplyResult struct {
	// Changes counts the changes made, one audit record each; 0 when
	// PostgreSQL already matched the file.
	Changes int
}

// ApplyFile reconciles PostgreSQL to an environment's inventory file, as
// ParseFile returned it, in one transaction under an advisory lock, so one
// replica applies a file at a time (ADR 0021 §3). Every change is audited
// with caller's actor and revision, the file's, as the config version.
//
// Rows are matched by the names they are unique by: the environment by its
// slug, an application by its name in the environment, a component by its
// name and type in its application, a telemetry source by its type, ingest
// mode and application_name in its application, a target by its name in
// the environment, a probe assignment by its probe on its target. A row
// that matches is updated to match the file; one that does not exist is
// created, managed by the file. A row the file manages and no longer lists
// is disabled, never deleted: an application, a source or a target gets
// status disabled, a probe assignment enabled false. A component has no
// status, so one the file drops stays as it is. The file's probe
// assignments are approved by the merge of its pull request: approved,
// with no user named.
//
// Rows the API manages are never changed. A file that names one, or that
// the existing rows refuse otherwise (an application_name another
// application's source claims, a target for a database another target is
// for, a render the collector would refuse), changes nothing and returns a
// *FileError naming the place in the file. Rows of other environments are
// not read.
func (s *Service) ApplyFile(ctx context.Context, caller Caller, revision string, file File) (ApplyResult, error) {
	var result ApplyResult
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		q := store.New(tx)
		if err := q.LockInventoryApply(ctx); err != nil {
			return fmt.Errorf("lock the inventory: %w", err)
		}
		a := &applier{ctx: ctx, q: q, caller: caller, revision: revision, now: time.Now()}
		if err := a.apply(&file); err != nil {
			return err
		}
		if a.changes > 0 {
			// The file's assignments are approved: check the render as the
			// collector will, as an approval through the API does.
			var refused *RenderError
			if _, err := render(ctx, q); errors.As(err, &refused) {
				return &FileError{Problems: []string{"database_targets: the file would render a managedCollectors file the collector refuses: " + refused.Reason}}
			} else if err != nil {
				return err
			}
		}
		result.Changes = a.changes
		return nil
	})
	if err != nil {
		return ApplyResult{}, err
	}
	return result, nil
}

// applier applies one file in one transaction.
type applier struct {
	ctx      context.Context
	q        *store.Queries
	caller   Caller
	revision string
	now      time.Time
	changes  int

	env store.Environment
}

// record audits one change through the apply's transaction.
func (a *applier) record(entityType, action, id string, before, after any) error {
	if _, err := audit.Write(a.ctx, a.q, audit.Change{
		RequestID: a.caller.RequestID, Actor: a.caller.Actor, Action: action,
		EntityType: entityType, EntityID: id, Before: before, After: after,
		ConfigVersion: a.revision,
	}); err != nil {
		return err
	}
	a.changes++
	return nil
}

// refuse is a file that the existing rows refuse, at the place at.
func refuse(at, format string, args ...any) error {
	return &FileError{Problems: []string{at + ": " + fmt.Sprintf(format, args...)}}
}

// apiManaged refuses a row the API manages, which the file never changes.
func apiManaged(at, entity, managedBy string) error {
	if managedBy == ManagedByConfig {
		return nil
	}
	return refuse(at, "the %s exists and is managed through the API: the inventory file changes only the rows it manages (ADR 0021 §4)", entity)
}

var configManaged = pgtype.Text{String: ManagedByConfig, Valid: true}

func (a *applier) apply(f *File) error {
	if err := a.environment(&f.Environment); err != nil {
		return err
	}
	apps, err := a.applications(f.Applications)
	if err != nil {
		return err
	}
	if err := a.components(f.Applications, apps); err != nil {
		return err
	}
	if err := a.sources(f.Applications, apps); err != nil {
		return err
	}
	targets, err := a.targets(f.DatabaseTargets)
	if err != nil {
		return err
	}
	return a.assignments(f.DatabaseTargets, targets)
}

func (a *applier) environment(e *FileEnvironment) error {
	labels, err := encodeLabels(e.Labels)
	if err != nil {
		return err
	}
	row, err := a.q.LockEnvironmentBySlug(a.ctx, e.Slug)
	if errors.Is(err, pgx.ErrNoRows) {
		row, err = a.q.CreateEnvironment(a.ctx, store.CreateEnvironmentParams{
			Name: e.Name, Slug: e.Slug, Type: e.Type, Description: e.Description, Status: e.Status,
			Labels: labels, ManagedBy: configManaged,
		})
		if err != nil {
			return fmt.Errorf("create the environment: %w", err)
		}
		a.env = row
		after, err := environment(row)
		if err != nil {
			return err
		}
		return a.record(audit.EntityEnvironment, audit.ActionCreate, after.ID.String(), nil, after)
	}
	if err != nil {
		return fmt.Errorf("read the environment: %w", err)
	}
	if err := apiManaged("environment.slug", fmt.Sprintf("environment %q", e.Slug), row.ManagedBy); err != nil {
		return err
	}
	a.env = row
	before, err := environment(row)
	if err != nil {
		return err
	}
	next := before
	next.Name, next.Type, next.Description, next.Status, next.Labels = e.Name, e.Type, e.Description, e.Status, labelsOrEmpty(e.Labels)
	if sameEnvironment(before, next) {
		return nil
	}
	row, err = a.q.UpdateEnvironment(a.ctx, store.UpdateEnvironmentParams{
		ID: row.ID, Name: next.Name, Type: next.Type, Description: next.Description, Status: next.Status, Labels: labels,
	})
	if err != nil {
		return fmt.Errorf("update the environment: %w", err)
	}
	a.env = row
	after, err := environment(row)
	if err != nil {
		return err
	}
	return a.record(audit.EntityEnvironment, audit.ActionUpdate, after.ID.String(), before, after)
}

// applications applies the file's applications and disables the ones it
// manages and no longer lists. It returns the rows of the file's
// applications, in the file's order.
func (a *applier) applications(files []FileApplication) ([]store.Application, error) {
	rows, err := a.q.LockEnvironmentApplications(a.ctx, a.env.ID)
	if err != nil {
		return nil, fmt.Errorf("read the environment's applications: %w", err)
	}
	byName := map[string]store.Application{}
	for _, row := range rows {
		byName[row.Name] = row
	}
	out := make([]store.Application, len(files))
	listed := map[string]bool{}
	for i, f := range files {
		at := fmt.Sprintf("applications[%d]", i)
		listed[f.Name] = true
		row, ok := byName[f.Name]
		if !ok {
			created, err := a.q.CreateApplication(a.ctx, store.CreateApplicationParams{
				EnvironmentID: a.env.ID, Name: f.Name, Platform: f.Platform, OwnerTeam: f.OwnerTeam,
				Criticality: f.Criticality, Status: f.Status, ManagedBy: configManaged,
			})
			if err != nil {
				return nil, fmt.Errorf("create application %q: %w", f.Name, err)
			}
			after := application(created)
			if err := a.record(audit.EntityApplication, audit.ActionCreate, after.ID.String(), nil, after); err != nil {
				return nil, err
			}
			out[i] = created
			continue
		}
		if err := apiManaged(at+".name", fmt.Sprintf("application %q", f.Name), row.ManagedBy); err != nil {
			return nil, err
		}
		before := application(row)
		next := before
		next.Platform, next.OwnerTeam, next.Criticality, next.Status = f.Platform, f.OwnerTeam, f.Criticality, f.Status
		if row, err = a.updateApplication(row, before, next, audit.ActionUpdate); err != nil {
			return nil, err
		}
		out[i] = row
	}
	for _, row := range rows {
		if listed[row.Name] || row.ManagedBy != ManagedByConfig || row.Status == StatusDisabled {
			continue
		}
		before := application(row)
		next := before
		next.Status = StatusDisabled
		if _, err := a.updateApplication(row, before, next, audit.ActionDisable); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (a *applier) updateApplication(row store.Application, before, next Application, action string) (store.Application, error) {
	if next == before {
		return row, nil
	}
	updated, err := a.q.UpdateApplication(a.ctx, store.UpdateApplicationParams{
		ID: row.ID, Name: next.Name, Platform: next.Platform, OwnerTeam: next.OwnerTeam,
		Criticality: next.Criticality, Status: next.Status,
	})
	if err != nil {
		return row, fmt.Errorf("update application %q: %w", before.Name, err)
	}
	after := application(updated)
	return updated, a.record(audit.EntityApplication, action, after.ID.String(), before, after)
}

// components applies the components of the file's applications. A
// component the file drops stays: it has no status to disable.
func (a *applier) components(files []FileApplication, apps []store.Application) error {
	rows, err := a.q.LockEnvironmentComponents(a.ctx, a.env.ID)
	if err != nil {
		return fmt.Errorf("read the environment's components: %w", err)
	}
	type key struct {
		app        pgtype.UUID
		name, kind string
	}
	existing := map[key]store.ApplicationComponent{}
	for _, row := range rows {
		existing[key{row.ApplicationID, row.Name, row.ComponentType}] = row
	}
	for i, f := range files {
		app := apps[i]
		for j, c := range f.Components {
			at := fmt.Sprintf("applications[%d].components[%d]", i, j)
			labels, err := encodeLabels(c.Labels)
			if err != nil {
				return err
			}
			row, ok := existing[key{app.ID, c.Name, c.Type}]
			if !ok {
				created, err := a.q.CreateComponent(a.ctx, store.CreateComponentParams{
					ApplicationID: app.ID, Name: c.Name, ComponentType: c.Type, RuntimeName: c.RuntimeName,
					Labels: labels, ManagedBy: configManaged,
				})
				if err != nil {
					return fmt.Errorf("create component %q of application %q: %w", c.Name, f.Name, err)
				}
				after, err := componentOf(created, a.env.ID)
				if err != nil {
					return err
				}
				if err := a.record(audit.EntityComponent, audit.ActionCreate, after.ID.String(), nil, after); err != nil {
					return err
				}
				continue
			}
			if err := apiManaged(at, fmt.Sprintf("%s component %q", c.Type, c.Name), row.ManagedBy); err != nil {
				return err
			}
			before, err := componentOf(row, a.env.ID)
			if err != nil {
				return err
			}
			next := before
			next.RuntimeName, next.Labels = c.RuntimeName, labelsOrEmpty(c.Labels)
			if sameComponent(before, next) {
				continue
			}
			updated, err := a.q.UpdateComponent(a.ctx, store.UpdateComponentParams{
				ID: row.ID, Name: next.Name, ComponentType: next.ComponentType, RuntimeName: next.RuntimeName, Labels: labels,
			})
			if err != nil {
				return fmt.Errorf("update component %q of application %q: %w", c.Name, f.Name, err)
			}
			after, err := componentOf(updated, a.env.ID)
			if err != nil {
				return err
			}
			if err := a.record(audit.EntityComponent, audit.ActionUpdate, after.ID.String(), before, after); err != nil {
				return err
			}
		}
	}
	return nil
}

// sources applies the telemetry sources of the file's applications and
// disables the ones it manages and no longer lists, first, so that a name
// they held is free for the file's.
func (a *applier) sources(files []FileApplication, apps []store.Application) error {
	rows, err := a.q.LockEnvironmentSources(a.ctx, a.env.ID)
	if err != nil {
		return fmt.Errorf("read the environment's telemetry sources: %w", err)
	}
	type key struct {
		app                  pgtype.UUID
		kind, mode, applName string
	}
	keyOf := func(row store.TelemetrySource) key {
		name, _ := applicationName(row.Config)
		return key{row.ApplicationID, row.SourceType, row.IngestMode, name}
	}
	listed := map[key]bool{}
	for i, f := range files {
		for _, s := range f.TelemetrySources {
			name, _ := applicationName(mustJSON(s.Config))
			listed[key{apps[i].ID, s.Type, s.IngestMode, name}] = true
		}
	}
	existing := map[key]int{}
	for i, row := range rows {
		k := keyOf(row)
		if listed[k] {
			existing[k] = i
			continue
		}
		if row.ManagedBy != ManagedByConfig || row.Status == StatusDisabled {
			continue
		}
		updated, err := a.updateSource(row, row.IngestMode, StatusDisabled, row.Config, audit.ActionDisable)
		if err != nil {
			return err
		}
		rows[i] = updated
	}
	for i, f := range files {
		app := apps[i]
		for j, s := range f.TelemetrySources {
			at := fmt.Sprintf("applications[%d].telemetry_sources[%d]", i, j)
			config := mustJSON(s.Config)
			name, _ := applicationName(config)
			if s.Status == StatusActive {
				// ADR 0017 §2: in an environment, an application_name is
				// one application's. A source the file disabled, or is
				// about to, no longer claims it; one the API manages does.
				for _, other := range rows {
					otherName, _ := applicationName(other.Config)
					if otherName == name && other.ApplicationID != app.ID && (other.ManagedBy != ManagedByConfig || other.Status == StatusActive) {
						return refuse(at+".config.application_name", "%q is claimed by telemetry source %s of another application: within an environment, an application_name belongs to one application", name, goUUID(other.ID))
					}
				}
			}
			idx, ok := existing[key{app.ID, s.Type, s.IngestMode, name}]
			if !ok {
				created, err := a.q.CreateSource(a.ctx, store.CreateSourceParams{
					ApplicationID: app.ID, SourceType: s.Type, IngestMode: s.IngestMode, Status: s.Status,
					Config: config, ManagedBy: configManaged,
				})
				if err != nil {
					return fmt.Errorf("create the %s source %q of application %q: %w", s.Type, name, f.Name, err)
				}
				rows = append(rows, created)
				after, err := sourceOf(created, a.env.ID, a.env.Slug)
				if err != nil {
					return err
				}
				if err := a.record(audit.EntityTelemetrySource, audit.ActionCreate, after.ID.String(), nil, after); err != nil {
					return err
				}
				continue
			}
			row := rows[idx]
			if err := apiManaged(at, fmt.Sprintf("%s %s source for %q", s.Type, s.IngestMode, name), row.ManagedBy); err != nil {
				return err
			}
			if row.Status == s.Status && jsonEqual(row.Config, config) {
				continue
			}
			if rows[idx], err = a.updateSource(row, s.IngestMode, s.Status, config, audit.ActionUpdate); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *applier) updateSource(row store.TelemetrySource, mode, status string, config []byte, action string) (store.TelemetrySource, error) {
	before, err := sourceOf(row, a.env.ID, a.env.Slug)
	if err != nil {
		return row, err
	}
	updated, err := a.q.UpdateSource(a.ctx, store.UpdateSourceParams{ID: row.ID, IngestMode: mode, Status: status, Config: config})
	if err != nil {
		return row, fmt.Errorf("update telemetry source %s: %w", before.ID, err)
	}
	after, err := sourceOf(updated, a.env.ID, a.env.Slug)
	if err != nil {
		return row, err
	}
	return updated, a.record(audit.EntityTelemetrySource, action, after.ID.String(), before, after)
}

// targets applies the file's database targets and disables the ones it
// manages and no longer lists. A disabled target still holds its name and
// its server, port and database (database_targets_identity_uq is not
// partial), so a target the file renames is refused, naming the old one. It
// returns the rows of the file's targets, in the file's order.
func (a *applier) targets(files []FileDatabaseTarget) ([]store.DatabaseTarget, error) {
	rows, err := a.q.LockEnvironmentDatabaseTargets(a.ctx, a.env.ID)
	if err != nil {
		return nil, fmt.Errorf("read the environment's database targets: %w", err)
	}
	listed := map[string]bool{}
	for _, f := range files {
		listed[f.Name] = true
	}
	byName := map[string]store.DatabaseTarget{}
	for _, row := range rows {
		byName[row.Name] = row
		if listed[row.Name] || row.ManagedBy != ManagedByConfig || row.Status == StatusDisabled {
			continue
		}
		before, err := databaseTarget(row)
		if err != nil {
			return nil, err
		}
		next := before
		next.Status = StatusDisabled
		if _, err := a.updateTarget("", row, before, next, audit.ActionDisable); err != nil {
			return nil, err
		}
	}
	out := make([]store.DatabaseTarget, len(files))
	for i, f := range files {
		at := fmt.Sprintf("database_targets[%d]", i)
		labels, err := encodeLabels(f.Labels)
		if err != nil {
			return nil, err
		}
		row, ok := byName[f.Name]
		if !ok {
			created, err := a.q.CreateDatabaseTarget(a.ctx, store.CreateDatabaseTargetParams{
				EnvironmentID: a.env.ID, Engine: f.Engine, Name: f.Name, Host: f.Host, Port: int32(f.Port),
				DatabaseName: pgText(f.DatabaseName), Status: f.Status, CredentialRef: f.CredentialRef,
				Labels: labels, ManagedBy: configManaged,
			})
			if err := targetTaken(err, DatabaseTarget{Engine: f.Engine, Name: f.Name, Host: f.Host, Port: f.Port, DatabaseName: f.DatabaseName}); err != nil {
				if held, ok := dropped(rows, listed, f); ok {
					return nil, refuse(at, "target %q, which the file no longer lists, is disabled and still holds this server, port and database: a target is not renamed; list it under its name %q", held.Name, held.Name)
				}
				return nil, fileConflict(at, err)
			}
			after, err := databaseTarget(created)
			if err != nil {
				return nil, err
			}
			if err := a.record(audit.EntityDatabaseTarget, audit.ActionCreate, after.ID.String(), nil, after); err != nil {
				return nil, err
			}
			out[i] = created
			continue
		}
		if err := apiManaged(at+".name", fmt.Sprintf("database target %q", f.Name), row.ManagedBy); err != nil {
			return nil, err
		}
		if row.Engine != f.Engine {
			return nil, refuse(at+".engine", "%s, and the target is %s: a target keeps its engine; give the new one another name", f.Engine, row.Engine)
		}
		before, err := databaseTarget(row)
		if err != nil {
			return nil, err
		}
		next := before
		next.Host, next.Port, next.DatabaseName, next.CredentialRef = f.Host, f.Port, f.DatabaseName, f.CredentialRef
		next.Status, next.Labels = f.Status, labelsOrEmpty(f.Labels)
		if out[i], err = a.updateTarget(at, row, before, next, audit.ActionUpdate); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (a *applier) updateTarget(at string, row store.DatabaseTarget, before, next DatabaseTarget, action string) (store.DatabaseTarget, error) {
	if sameDatabaseTarget(before, next) {
		return row, nil
	}
	labels, err := encodeLabels(next.Labels)
	if err != nil {
		return row, err
	}
	updated, err := a.q.UpdateDatabaseTarget(a.ctx, store.UpdateDatabaseTargetParams{
		ID: row.ID, Host: next.Host, Port: int32(next.Port), DatabaseName: pgText(next.DatabaseName),
		Status: next.Status, CredentialRef: next.CredentialRef, Labels: labels,
	})
	if err := targetTaken(err, next); err != nil {
		return row, fileConflict(at, err)
	}
	after, err := databaseTarget(updated)
	if err != nil {
		return row, err
	}
	if err := a.record(audit.EntityDatabaseTarget, action, after.ID.String(), before, after); err != nil {
		return row, err
	}
	if !sameConnection(before, after) {
		// The merge that changed where the target is approved its probes
		// there too: send the applied ones back to approved, so that the
		// applied check confirms the collector runs them against the new
		// connection (ADR 0014 §4).
		if err := a.reapprove(updated.ID); err != nil {
			return row, err
		}
	}
	return updated, nil
}

// reapprove sends a target's applied assignments back to approved.
func (a *applier) reapprove(target pgtype.UUID) error {
	rows, err := a.q.ListApprovedTargetAssignments(a.ctx, target)
	if err != nil {
		return fmt.Errorf("list the target's approved assignments: %w", err)
	}
	for _, row := range rows {
		if row.State != StateApplied {
			continue
		}
		if _, err := a.approve(row, row, audit.ActionUpdate); err != nil {
			return err
		}
	}
	return nil
}

// assignments applies the probes of the file's targets: each an assignment
// of the built-in definition, approved; one the file no longer lists, or
// whose target it no longer lists, is disabled.
func (a *applier) assignments(files []FileDatabaseTarget, targets []store.DatabaseTarget) error {
	rows, err := a.q.LockEnvironmentProbeAssignments(a.ctx, a.env.ID)
	if err != nil {
		return fmt.Errorf("read the environment's probe assignments: %w", err)
	}
	type key struct {
		target pgtype.UUID
		probe  string
	}
	listed := map[key]FileProbe{}
	for i, f := range files {
		for _, p := range f.Probes {
			listed[key{targets[i].ID, p.Name}] = p
		}
	}
	existing := map[key]store.ProbeAssignment{}
	for _, row := range rows {
		k := key{row.ProbeAssignment.DatabaseTargetID, row.DefinitionName}
		if _, ok := listed[k]; ok {
			existing[k] = row.ProbeAssignment
			continue
		}
		if row.ProbeAssignment.ManagedBy != ManagedByConfig || !row.ProbeAssignment.Enabled {
			continue
		}
		next := row.ProbeAssignment
		next.Enabled = false
		if err := a.updateAssignment(row.ProbeAssignment, next, audit.ActionDisable); err != nil {
			return err
		}
	}
	for i, f := range files {
		target := targets[i]
		for j, p := range f.Probes {
			at := fmt.Sprintf("database_targets[%d].probes[%d]", i, j)
			// A target the file disables runs nothing: its assignments are
			// left out of the render, enabled or not.
			want := store.ProbeAssignment{
				IntervalSeconds: int32(p.IntervalSeconds), TimeoutMs: pgInt(p.TimeoutMS), MaxRows: pgInt(p.MaxRows), Enabled: p.On(),
			}
			row, ok := existing[key{target.ID, p.Name}]
			if !ok {
				def, err := a.q.GetLatestProbeDefinitionVersion(a.ctx, store.GetLatestProbeDefinitionVersionParams{Engine: target.Engine, Name: p.Name})
				if errors.Is(err, pgx.ErrNoRows) {
					return refuse(at+".name", "%q: no such %s probe definition in PostgreSQL; the API's migrations add the built-in ones", p.Name, target.Engine)
				}
				if err != nil {
					return fmt.Errorf("read probe definition %s: %w", p.Name, err)
				}
				if !def.Enabled {
					return refuse(at+".name", "probe definition %s is disabled: enable it through the API, or take it out of the file", p.Name)
				}
				created, err := a.q.CreateProbeAssignment(a.ctx, store.CreateProbeAssignmentParams{
					DatabaseTargetID: target.ID, ProbeDefinitionID: def.ID, IntervalSeconds: want.IntervalSeconds,
					TimeoutMs: want.TimeoutMs, MaxRows: want.MaxRows, Enabled: want.Enabled, ManagedBy: configManaged,
				})
				if err != nil {
					return fmt.Errorf("create the assignment of %s to target %q: %w", p.Name, f.Name, err)
				}
				if _, err := a.approve(store.ProbeAssignment{}, created, audit.ActionCreate); err != nil {
					return err
				}
				continue
			}
			if err := apiManaged(at+".name", fmt.Sprintf("assignment of probe %s", p.Name), row.ManagedBy); err != nil {
				return err
			}
			next := row
			next.IntervalSeconds, next.TimeoutMs, next.MaxRows, next.Enabled = want.IntervalSeconds, want.TimeoutMs, want.MaxRows, want.Enabled
			if err := a.updateAssignment(row, next, audit.ActionUpdate); err != nil {
				return err
			}
		}
	}
	return nil
}

// updateAssignment changes an assignment's policy to next's and approves
// it; one whose policy is next's and that is approved already is left as
// it is.
func (a *applier) updateAssignment(row, next store.ProbeAssignment, action string) error {
	samePolicy := row.IntervalSeconds == next.IntervalSeconds && row.TimeoutMs == next.TimeoutMs &&
		row.MaxRows == next.MaxRows && row.Enabled == next.Enabled
	approved := row.State == StateApproved || row.State == StateApplied
	if samePolicy && approved {
		return nil
	}
	current := row
	if !samePolicy {
		updated, err := a.q.UpdateProbeAssignment(a.ctx, store.UpdateProbeAssignmentParams{
			ID: row.ID, ProbeDefinitionID: row.ProbeDefinitionID, IntervalSeconds: next.IntervalSeconds,
			TimeoutMs: next.TimeoutMs, MaxRows: next.MaxRows, Enabled: next.Enabled,
		})
		if err != nil {
			return fmt.Errorf("update probe assignment %s: %w", goUUID(row.ID), err)
		}
		current = updated
	}
	_, err := a.approve(row, current, action)
	return err
}

// approve sets current approved, by the file's merge, and audits the
// change from before; a zero before is a creation.
func (a *applier) approve(before, current store.ProbeAssignment, action string) (store.ProbeAssignment, error) {
	updated, err := a.q.SetProbeAssignmentState(a.ctx, store.SetProbeAssignmentStateParams{
		ID: current.ID, State: StateApproved, ApprovedAt: pgtype.Timestamptz{Time: a.now, Valid: true},
	})
	if err != nil {
		return current, fmt.Errorf("approve probe assignment %s: %w", goUUID(current.ID), err)
	}
	after := probeAssignment(updated)
	var was any
	if before.ID.Valid {
		was = probeAssignment(before)
	}
	return updated, a.record(audit.EntityProbeAssignment, action, after.ID.String(), was, after)
}

// dropped returns the target the file manages and no longer lists that is
// for f's server, port and database, as database_targets_identity_uq
// compares them.
func dropped(rows []store.DatabaseTarget, listed map[string]bool, f FileDatabaseTarget) (store.DatabaseTarget, bool) {
	for _, row := range rows {
		if !listed[row.Name] && row.ManagedBy == ManagedByConfig && row.Engine == f.Engine && row.Host == f.Host &&
			int(row.Port) == f.Port && strings.EqualFold(row.DatabaseName.String, f.DatabaseName) {
			return row, true
		}
	}
	return store.DatabaseTarget{}, false
}

// fileConflict places a ConflictError the rows returned at at in the file.
func fileConflict(at string, err error) error {
	var conflict *ConflictError
	if errors.As(err, &conflict) {
		return refuse(at, "%s", conflict.Detail)
	}
	return err
}

func labelsOrEmpty(labels map[string]string) map[string]string {
	if labels == nil {
		return map[string]string{}
	}
	return maps.Clone(labels)
}

// mustJSON encodes a source's config, which ParseFile decoded from JSON.
func mustJSON(v map[string]any) []byte {
	if v == nil {
		v = map[string]any{}
	}
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}
