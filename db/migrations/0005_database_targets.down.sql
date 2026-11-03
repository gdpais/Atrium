drop index if exists database_targets_identity_uq;
create unique index database_targets_identity_uq on database_targets (environment_id, engine, host, port, coalesce(database_name, ''));
drop index if exists database_targets_environment_name_uq;
