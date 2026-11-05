-- Application components: each belongs to one application,
-- application_components.application_id, which nothing updates. A
-- component's environment is its application's, read through the join.

-- name: CreateComponent :one
-- Fails with a foreign key violation when the application does not exist.
-- managed_by is api when null.
insert into application_components (application_id, name, component_type, runtime_name, labels, managed_by)
values (@application_id, @name, @component_type, @runtime_name, @labels, coalesce(sqlc.narg(managed_by)::text, 'api'))
returning *;

-- name: GetComponent :one
select c.id, c.application_id, a.environment_id, c.name, c.component_type,
  c.runtime_name, c.labels, c.created_at, c.updated_at, c.managed_by
from application_components c
join applications a on a.id = c.application_id
where c.id = @id;

-- name: LockComponent :one
-- The component, locked until the transaction ends, for a change.
select c.id, c.application_id, a.environment_id, c.name, c.component_type,
  c.runtime_name, c.labels, c.created_at, c.updated_at, c.managed_by
from application_components c
join applications a on a.id = c.application_id
where c.id = @id
for update of c;

-- name: ListComponents :many
-- A page of components by name, then ID: those after the cursor's (name,
-- id) when one is given.
select c.id, c.application_id, a.environment_id, c.name, c.component_type,
  c.runtime_name, c.labels, c.created_at, c.updated_at, c.managed_by
from application_components c
join applications a on a.id = c.application_id
where (sqlc.narg(application_id)::uuid is null or c.application_id = sqlc.narg(application_id))
  and (sqlc.narg(after_name)::text is null
    or (c.name, c.id) > (sqlc.narg(after_name)::text, sqlc.narg(after_id)::uuid))
order by c.name, c.id
limit @row_limit;

-- name: UpdateComponent :one
-- The application is not updated: a component keeps the one it was created
-- in.
update application_components
set name = @name, component_type = @component_type, runtime_name = @runtime_name,
  labels = @labels, updated_at = now()
where id = @id
returning *;

-- name: DeleteComponent :execrows
delete from application_components where id = @id;
