-- Expand only (CONTRIBUTING): a unique index. No released API writes
-- database_targets, so no row can violate it.

-- A target's name is the `target` label of its metrics, beside the
-- environment's: two targets of one environment with the same name would
-- write the same series, and the collector refuses such a config
-- (validateUniqueSQLServerTargets). So a name is unique per environment.
create unique index database_targets_environment_name_uq on database_targets (environment_id, name);
