alter table probe_assignments drop column if exists managed_by;
alter table database_targets drop column if exists managed_by;
alter table telemetry_sources drop column if exists managed_by;
alter table application_components drop column if exists managed_by;
alter table applications drop column if exists managed_by;
alter table environments drop column if exists managed_by;
