-- Probe definitions (TODO 4.5): one row per version. A definition is the
-- rows that share an engine and a name; its latest version is the highest.

-- name: ListLatestProbeDefinitions :many
-- The latest version of every definition, built-in and custom.
select distinct on (p.engine, p.name) sqlc.embed(p)
from probe_definitions p
order by p.engine, p.name, p.version desc;

-- name: GetProbeDefinition :one
-- One version, and whether it is its definition's latest.
select sqlc.embed(p),
  (p.version = (select max(o.version) from probe_definitions o where o.engine = p.engine and o.name = p.name))::boolean as latest
from probe_definitions p
where p.id = @id;

-- name: ListProbeDefinitionVersions :many
-- Every version of the definition @id is a version of, the newest first.
select sqlc.embed(p),
  (p.version = max(p.version) over ())::boolean as latest
from probe_definitions p
join probe_definitions v on v.engine = p.engine and v.name = p.name
where v.id = @id
order by p.version desc;

-- name: GetBuiltinProbeCategory :one
-- The category of the built-in probe a custom definition overrides; no rows
-- when there is none, or it is disabled.
select category
from probe_definitions
where builtin and enabled and engine = @engine and name = @name;

-- name: LockLatestProbeDefinitionVersion :one
-- The latest version of a definition, locked until the transaction ends, so
-- that concurrent changes to it take turns.
select id, version, builtin, enabled
from probe_definitions
where engine = @engine and name = @name
order by version desc
limit 1
for update;

-- name: InsertProbeDefinition :one
-- A version of a custom definition. Fails with a unique violation when the
-- version exists.
insert into probe_definitions (
  engine, name, version, builtin, overrides, category, description,
  default_interval_seconds, timeout_ms, query_template
)
values (
  @engine, @name, @version, false, @overrides, @category, @description,
  @default_interval_seconds, @timeout_ms, @query_template
)
returning *;

-- name: SetProbeDefinitionEnabled :execrows
-- Enables or disables a definition: every version of it.
update probe_definitions
set enabled = @enabled, updated_at = now()
where engine = @engine and name = @name and enabled <> @enabled;
