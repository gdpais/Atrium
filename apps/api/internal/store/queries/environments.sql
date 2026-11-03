-- Environments, the top ownership boundary (ADR 0001). Package inventory
-- validates what these write and audits each change in its transaction.

-- name: CreateEnvironment :one
insert into environments (name, slug, type, description, status, labels)
values (@name, @slug, @type, @description, @status, @labels)
returning *;

-- name: GetEnvironment :one
select * from environments where id = @id;

-- name: LockEnvironment :one
-- The environment, locked until the transaction ends, for a change.
select * from environments where id = @id for update;

-- name: ListEnvironments :many
select * from environments
where sqlc.narg(status)::text is null or status = sqlc.narg(status)
order by name, id;

-- name: UpdateEnvironment :one
-- The slug is not updated: it is the environment's name in telemetry.
update environments
set name = @name, type = @type, description = @description, status = @status,
  labels = @labels, updated_at = now()
where id = @id
returning *;

-- name: DeleteEnvironment :execrows
-- Fails with a foreign key violation while a row refers to it.
delete from environments where id = @id;
