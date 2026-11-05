-- The inventory file's reconciler (ADR 0021 §3): it reads an environment's
-- rows, locked, and changes them through the queries of each entity.

-- name: LockInventoryApply :exec
-- One replica applies an inventory file at a time: a transaction-level
-- advisory lock, released when the apply commits or rolls back. The key is
-- the inventory reconciler's alone.
select pg_advisory_xact_lock(7230041802311954241);

-- name: LockEnvironmentBySlug :one
-- The environment with slug, locked until the transaction ends.
select * from environments where slug = @slug for update;

-- name: LockEnvironmentApplications :many
-- Every application of an environment, locked, by name.
select * from applications where environment_id = @environment_id
order by name, id
for update;

-- name: LockEnvironmentComponents :many
-- Every component of an environment's applications, locked.
select c.* from application_components c
join applications a on a.id = c.application_id
where a.environment_id = @environment_id
order by c.application_id, c.name, c.component_type, c.id
for update of c;

-- name: LockEnvironmentSources :many
-- Every telemetry source of an environment's applications, locked.
select s.* from telemetry_sources s
join applications a on a.id = s.application_id
where a.environment_id = @environment_id
order by s.application_id, s.id
for update of s;

-- name: LockEnvironmentDatabaseTargets :many
-- Every database target of an environment, locked, by name.
select * from database_targets where environment_id = @environment_id
order by name, id
for update;

-- name: LockEnvironmentProbeAssignments :many
-- Every probe assignment of an environment's targets, locked, with the
-- engine and name of the definition each pins.
select sqlc.embed(pa), d.engine as definition_engine, d.name as definition_name
from probe_assignments pa
join database_targets t on t.id = pa.database_target_id
join probe_definitions d on d.id = pa.probe_definition_id
where t.environment_id = @environment_id
order by pa.database_target_id, pa.id
for update of pa;
