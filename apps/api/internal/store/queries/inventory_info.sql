-- The inventory's info metrics (heartbeat_api_inventory_*_info), read at
-- each scrape of the API's /metrics. Each returns at most @row_limit rows,
-- in a fixed order, so a scrape's series are bounded and stable: the
-- collector asks for one more than its cap to tell that a kind was cut.
-- Only names, enumerated values and the manager are read: never an ID, a
-- host, a config other than its application_name, a label or a
-- credential reference.

-- name: EnvironmentInfo :many
select slug, type, status, managed_by
from environments
order by slug
limit @row_limit;

-- name: ApplicationInfo :many
select e.slug as environment_slug, a.name, a.platform, a.criticality, a.status, a.managed_by
from applications a
join environments e on e.id = a.environment_id
order by e.slug, a.name
limit @row_limit;

-- name: SourceInfo :many
-- application_name is the config's, '' when it has none.
select e.slug as environment_slug, a.name as application, s.source_type, s.ingest_mode,
  coalesce(s.config ->> 'application_name', '')::text as application_name,
  s.status, s.managed_by
from telemetry_sources s
join applications a on a.id = s.application_id
join environments e on e.id = a.environment_id
order by e.slug, a.name, s.source_type, s.ingest_mode, application_name, s.id
limit @row_limit;

-- name: DatabaseTargetInfo :many
select e.slug as environment_slug, t.name, t.engine, t.status, t.managed_by
from database_targets t
join environments e on e.id = t.environment_id
order by e.slug, t.name
limit @row_limit;
