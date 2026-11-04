-- Expand only (CONTRIBUTING): a column with a default and a check.

-- One writer per row (ADR 0021 §4): `config` rows are the inventory file's,
-- applied by the API's reconciler, and the API's endpoints refuse to change
-- or delete them; `api` rows, every row so far, are the endpoints' and the
-- file never touches them.
alter table environments add column managed_by text not null default 'api'
  constraint environments_managed_by_ck check (managed_by in ('api', 'config'));
alter table applications add column managed_by text not null default 'api'
  constraint applications_managed_by_ck check (managed_by in ('api', 'config'));
alter table application_components add column managed_by text not null default 'api'
  constraint application_components_managed_by_ck check (managed_by in ('api', 'config'));
alter table telemetry_sources add column managed_by text not null default 'api'
  constraint telemetry_sources_managed_by_ck check (managed_by in ('api', 'config'));
alter table database_targets add column managed_by text not null default 'api'
  constraint database_targets_managed_by_ck check (managed_by in ('api', 'config'));
alter table probe_assignments add column managed_by text not null default 'api'
  constraint probe_assignments_managed_by_ck check (managed_by in ('api', 'config'));
