-- Probe assignments (TODO 4.5, ADR 0014): a probe definition version, pinned,
-- run against a database target at an interval, with an approval state.
-- PostgreSQL holds the request; the rendered values file is the desired state
-- and the collector's admin config the observed state.

-- name: CreateProbeAssignment :one
-- Fails with a foreign key violation when the target or the definition
-- version does not exist, and with a unique violation when the target has
-- this version already.
insert into probe_assignments (database_target_id, probe_definition_id, interval_seconds, timeout_ms, max_rows, enabled, created_by)
values (@database_target_id, @probe_definition_id, @interval_seconds, sqlc.narg(timeout_ms), sqlc.narg(max_rows), @enabled, sqlc.narg(created_by))
returning *;

-- name: GetProbeAssignment :one
select * from probe_assignments where id = @id;

-- name: LockProbeAssignment :one
-- The assignment, locked until the transaction ends, for a change.
select * from probe_assignments where id = @id for update;

-- name: ListProbeAssignments :many
-- A page of assignments by creation, then ID: those after the cursor's
-- (created_at, id) when one is given.
select * from probe_assignments
where (sqlc.narg(database_target_id)::uuid is null or database_target_id = sqlc.narg(database_target_id))
  and (sqlc.narg(state)::text is null or state = sqlc.narg(state))
  and (sqlc.narg(after_created_at)::timestamptz is null
    or (created_at, id) > (sqlc.narg(after_created_at)::timestamptz, sqlc.narg(after_id)::uuid))
order by created_at, id
limit @row_limit;

-- name: ListTargetAssignmentProbes :many
-- The other assignments of a target, with the probe each runs, for the checks
-- a change makes under the target's lock.
select a.id, a.state, a.enabled, a.interval_seconds,
  d.engine, d.name, coalesce(d.overrides, d.name)::text as probe_name
from probe_assignments a
join probe_definitions d on d.id = a.probe_definition_id
where a.database_target_id = @database_target_id and a.id <> @except_id;

-- name: UpdateProbeAssignment :one
-- A change of what an assignment runs: it goes back to draft, unapproved.
update probe_assignments
set probe_definition_id = @probe_definition_id, interval_seconds = @interval_seconds,
  timeout_ms = sqlc.narg(timeout_ms), max_rows = sqlc.narg(max_rows), enabled = @enabled,
  state = 'draft', state_detail = '', pull_request_url = null, approved_by = null, approved_at = null,
  updated_at = now()
where id = @id
returning *;

-- name: SetProbeAssignmentState :one
-- A move to another state. approved_by and approved_at are set for the
-- approved states and cleared for draft and rejected.
update probe_assignments
set state = @state, state_detail = @state_detail, approved_by = sqlc.narg(approved_by),
  approved_at = sqlc.narg(approved_at), updated_at = now()
where id = @id
returning *;

-- name: MarkProbeAssignmentApplied :execrows
-- approved -> applied, once the collector runs it as rendered. Only from
-- approved, so a change that raced the check is not marked.
update probe_assignments
set state = 'applied', state_detail = '', updated_at = now()
where id = @id and state = 'approved';

-- name: DeleteProbeAssignment :execrows
delete from probe_assignments where id = @id;

-- name: ListRenderedProbeAssignments :many
-- What a render holds (ADR 0014 §2): the enabled assignments in an approved
-- state, of active targets and enabled definitions, with their target, its
-- environment and the definition version each pins.
select a.id, a.interval_seconds, a.timeout_ms, a.max_rows, a.state,
  t.id as target_id, t.name as target_name, t.host, t.port, t.database_name, t.credential_ref,
  e.slug as environment_slug,
  d.name as definition_name, d.builtin, coalesce(d.overrides, d.name)::text as probe_name,
  d.query_template, d.timeout_ms as definition_timeout_ms
from probe_assignments a
join database_targets t on t.id = a.database_target_id
join environments e on e.id = t.environment_id
join probe_definitions d on d.id = a.probe_definition_id
where a.enabled and a.state in ('approved', 'proposed', 'applied')
  and t.status = 'active' and d.enabled
order by t.id, a.interval_seconds, probe_name, a.id;

-- name: ListApprovedTargetAssignments :many
-- A target's assignments in an approved state, locked until the transaction
-- ends: a change to where the target is, or how the collector logs in, sends
-- them back to draft.
select * from probe_assignments
where database_target_id = @database_target_id and state in ('approved', 'proposed', 'applied')
order by created_at, id
for update;

-- name: ResetProbeAssignment :one
-- Back to draft, unapproved, with nothing else changed.
update probe_assignments
set state = 'draft', state_detail = '', pull_request_url = null, approved_by = null, approved_at = null,
  updated_at = now()
where id = @id
returning *;
