-- Telemetry sources: each belongs to one application,
-- telemetry_sources.application_id, which nothing updates; its environment
-- is its application's, read through the join (ADR 0017 §2).

-- name: CreateSource :one
-- Fails with a foreign key violation when the application does not exist.
-- managed_by is api when null.
insert into telemetry_sources (application_id, source_type, ingest_mode, status, config, managed_by)
values (@application_id, @source_type, @ingest_mode, @status, @config, coalesce(sqlc.narg(managed_by)::text, 'api'))
returning *;

-- name: GetSource :one
select s.id, s.application_id, a.environment_id, e.slug as environment_slug,
  s.source_type, s.ingest_mode, s.status, s.config, s.created_at, s.updated_at,
  s.managed_by
from telemetry_sources s
join applications a on a.id = s.application_id
join environments e on e.id = a.environment_id
where s.id = @id;

-- name: LockSource :one
-- The source, locked until the transaction ends, for a change.
select s.id, s.application_id, a.environment_id, e.slug as environment_slug,
  s.source_type, s.ingest_mode, s.status, s.config, s.created_at, s.updated_at,
  s.managed_by
from telemetry_sources s
join applications a on a.id = s.application_id
join environments e on e.id = a.environment_id
where s.id = @id
for update of s;

-- name: ListSources :many
-- A page of sources by application_name, then ID: those after the cursor's
-- (name, id) when one is given.
select s.id, s.application_id, a.environment_id, e.slug as environment_slug,
  s.source_type, s.ingest_mode, s.status, s.config, s.created_at, s.updated_at,
  s.managed_by
from telemetry_sources s
join applications a on a.id = s.application_id
join environments e on e.id = a.environment_id
where (sqlc.narg(application_id)::uuid is null or s.application_id = sqlc.narg(application_id))
  and (sqlc.narg(environment_id)::uuid is null or a.environment_id = sqlc.narg(environment_id))
  and (sqlc.narg(source_type)::text is null or s.source_type = sqlc.narg(source_type))
  and (sqlc.narg(status)::text is null or s.status = sqlc.narg(status))
  and (sqlc.narg(after_name)::text is null
    or (coalesce(s.config->>'application_name', ''), s.id) > (sqlc.narg(after_name)::text, sqlc.narg(after_id)::uuid))
order by coalesce(s.config->>'application_name', ''), s.id
limit @row_limit;

-- name: LockSourcesOfEnvironment :one
-- Takes the environment's row lock that every source change in it takes
-- first, so that two changes cannot both find an application_name free.
-- It does not block applications being added (they take a key share lock).
select id from environments where id = @id for no key update;

-- name: SourcesNamed :many
-- The sources of an environment whose config names application_name.
select s.id, s.application_id, s.source_type, s.ingest_mode
from telemetry_sources s
join applications a on a.id = s.application_id
where a.environment_id = @environment_id
  and s.config->>'application_name' = @application_name::text;

-- name: UpdateSource :one
-- The application and the source type are not updated.
update telemetry_sources
set ingest_mode = @ingest_mode, status = @status, config = @config, updated_at = now()
where id = @id
returning *;

-- name: DeleteSource :execrows
delete from telemetry_sources where id = @id;
