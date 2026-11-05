-- Applications: each belongs to one environment, applications.environment_id,
-- which nothing updates (ADR 0001, ADR 0013 §2).

-- name: CreateApplication :one
-- Fails with a foreign key violation when the environment does not exist.
-- managed_by is api when null.
insert into applications (environment_id, name, platform, owner_team, criticality, status, managed_by)
values (@environment_id, @name, @platform, @owner_team, @criticality, @status, coalesce(sqlc.narg(managed_by)::text, 'api'))
returning *;

-- name: GetApplication :one
select * from applications where id = @id;

-- name: LockApplication :one
-- The application, locked until the transaction ends, for a change.
select * from applications where id = @id for update;

-- name: ListApplications :many
-- A page of applications by name, then ID: those after the cursor's (name,
-- id) when one is given.
select * from applications
where (sqlc.narg(environment_id)::uuid is null or environment_id = sqlc.narg(environment_id))
  and (sqlc.narg(status)::text is null or status = sqlc.narg(status))
  and (sqlc.narg(after_name)::text is null
    or (name, id) > (sqlc.narg(after_name)::text, sqlc.narg(after_id)::uuid))
order by name, id
limit @row_limit;

-- name: UpdateApplication :one
-- The environment is not updated: an application keeps the one it was
-- created in.
update applications
set name = @name, platform = @platform, owner_team = @owner_team,
  criticality = @criticality, status = @status, updated_at = now()
where id = @id
returning *;

-- name: DeleteApplication :execrows
-- Fails with a foreign key violation while a row refers to it.
delete from applications where id = @id;
