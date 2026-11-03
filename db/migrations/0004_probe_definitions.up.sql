-- Expand only (CONTRIBUTING): looser columns, new columns with defaults and
-- seed rows. No released API reads probe_definitions, so the version column
-- changes type here too: the table has had no writer before this migration.

-- One row per version of a probe definition (TODO 4.5). A version never
-- changes once written, except whether its definition is enabled: a change
-- to what runs is a new version, and an assignment pins the version it was
-- approved with. Versions count from 1 per engine and name.
alter table probe_definitions alter column version type integer using version::integer;
alter table probe_definitions add constraint probe_definitions_version_ck check (version > 0);

-- A built-in definition is a probe of the db-collector's catalog: it runs the
-- collector's own query, so it has none here, and it changes only with the
-- collector. A custom definition replaces the query of the built-in it
-- overrides, whose metric descriptors decode its rows; its query_template is
-- checked as one read-only SELECT before it is stored.
alter table probe_definitions alter column query_template drop not null;
alter table probe_definitions add column builtin boolean not null default false;
alter table probe_definitions add column overrides text;
alter table probe_definitions add column description text not null default '';
alter table probe_definitions add constraint probe_definitions_kind_ck check (
  (builtin and query_template is null and overrides is null)
  or (not builtin and query_template is not null and overrides is not null)
);

-- Null leaves the timeout to the collector: half the interval, at most 10s.
alter table probe_definitions alter column timeout_ms drop not null;

-- The SQL Server catalog's probes (services/db-collector/probecatalog), at the
-- collector's default scrape interval. A test fails when these rows and the
-- catalog differ: a probe added to the catalog needs a migration that seeds
-- it, and one removed is disabled, never deleted, since assignments may
-- reference it.
insert into probe_definitions (engine, name, version, category, default_interval_seconds, builtin, description)
values
  ('sqlserver', 'blocking', 1, 'blocking', 30, true, 'Blocked requests per blocking session, from sys.dm_exec_requests'),
  ('sqlserver', 'buffer_cache', 1, 'buffer_cache', 30, true, 'Page life expectancy and buffer cache hit ratio, from sys.dm_os_performance_counters'),
  ('sqlserver', 'cpu', 1, 'cpu', 30, true, 'SQL Server and other-process CPU utilisation, from the scheduler monitor ring buffer'),
  ('sqlserver', 'file_io', 1, 'file_io', 30, true, 'Reads, writes, bytes and I/O stall time per database file, from sys.dm_io_virtual_file_stats'),
  ('sqlserver', 'memory_pressure', 1, 'memory_pressure', 30, true, 'Total server memory, from sys.dm_os_performance_counters'),
  ('sqlserver', 'sessions', 1, 'sessions', 30, true, 'Sessions by status, from sys.dm_exec_sessions'),
  ('sqlserver', 'storage', 1, 'storage', 30, true, 'Database file sizes, from sys.master_files'),
  ('sqlserver', 'throughput', 1, 'throughput', 30, true, 'Batch requests and transactions, from sys.dm_os_performance_counters'),
  ('sqlserver', 'waits', 1, 'waits', 30, true, 'Wait time by wait type, without idle and background waits, from sys.dm_os_wait_stats')
on conflict (engine, name, version) do nothing;
