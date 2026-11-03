-- Database targets: each belongs to one environment,
-- database_targets.environment_id, which nothing updates. They hold a
-- credential reference, never a credential (ADR 0014 §6).

-- name: CreateDatabaseTarget :one
-- Fails with a foreign key violation when the environment does not exist,
-- and with a unique violation when the environment has a target of that
-- name, or of that engine, host, port and database.
insert into database_targets (environment_id, engine, name, host, port, database_name, status, credential_ref, labels)
values (@environment_id, @engine, @name, @host, @port, sqlc.narg(database_name), @status, @credential_ref, @labels)
returning *;

-- name: GetDatabaseTarget :one
select * from database_targets where id = @id;

-- name: LockDatabaseTarget :one
-- The target, locked until the transaction ends, for a change.
select * from database_targets where id = @id for update;

-- name: ListDatabaseTargets :many
-- A page of targets by name, then ID: those after the cursor's (name, id)
-- when one is given.
select * from database_targets
where (sqlc.narg(environment_id)::uuid is null or environment_id = sqlc.narg(environment_id))
  and (sqlc.narg(status)::text is null or status = sqlc.narg(status))
  and (sqlc.narg(after_name)::text is null
    or (name, id) > (sqlc.narg(after_name)::text, sqlc.narg(after_id)::uuid))
order by name, id
limit @row_limit;

-- name: UpdateDatabaseTarget :one
-- The environment, engine and name are not updated: a target keeps them.
update database_targets
set host = @host, port = @port, database_name = sqlc.narg(database_name), status = @status,
  credential_ref = @credential_ref, labels = @labels, updated_at = now()
where id = @id
returning *;

-- name: CountDatabaseTargetAssignments :one
-- The probe assignments of a target. They are deleted with it (on delete
-- cascade), so a delete checks them first.
select count(*)::bigint from probe_assignments where database_target_id = @database_target_id;

-- name: DeleteDatabaseTarget :execrows
delete from database_targets where id = @id;
