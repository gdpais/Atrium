-- Expand only (CONTRIBUTING): indexes. No released API writes
-- database_targets, so no row can violate them.

-- A target's name is the `target` label of its metrics, beside the
-- environment's: two targets of one environment with the same name would
-- write the same series, and the collector refuses such a config
-- (validateUniqueSQLServerTargets). So a name is unique per environment.
create unique index database_targets_environment_name_uq on database_targets (environment_id, name);

-- One target per database: SQL Server compares database names without
-- regard to case by default, as the collector does (lower(host), port,
-- lower(database_name)), so Orders and orders on one server are one
-- database. The API stores hosts in lower case.
drop index database_targets_identity_uq;
create unique index database_targets_identity_uq on database_targets (environment_id, engine, host, port, lower(coalesce(database_name, '')));
