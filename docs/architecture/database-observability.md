# Database Observability Architecture

## Scope
- SQL Server is the first DB engine in scope.
- Oracle is the intended future monitored engine; PostgreSQL is Atrium's metadata store, not a monitoring target in the current scope.
- PostgreSQL stores target metadata, probe definitions and probe assignments for the control plane (the API and the inventory file).
- The collector never reads PostgreSQL: its desired state is YAML, into which the API renders approved probe assignments ([ADR 0014](decisions/0014-probe-assignments-to-runtime-config.md)).

## SQL Server signal coverage

Target signal set for the product, compared with what the built-in probe catalog
emits today. Metric names and labels are listed in the
[metrics reference](../reference/metrics-and-endpoints.md#sql-server-probe-metrics).

| Signal | Status | Current probe / metric |
| --- | --- | --- |
| Wait statistics | Collected | `waits` → `atrium_sqlserver_wait_seconds_total` (counter) |
| Blocking and locks | Collected (blocked requests) | `blocking` → `atrium_sqlserver_blocked_requests` |
| Sessions and connections | Collected | `sessions` → `atrium_sqlserver_sessions` |
| Memory pressure | Collected | `memory_pressure` → `atrium_sqlserver_total_server_memory_bytes` |
| Database size / file size | Collected | `storage` → `atrium_sqlserver_database_file_size_bytes` |
| Throughput counters (batch requests, transactions) | Collected | `throughput` → `atrium_sqlserver_batch_requests_total`, `atrium_sqlserver_transactions_total` (counters) |
| Instance availability | Partial | `atrium_collector_target_up` (collector reachability) |
| CPU utilisation | Collected (one-minute granularity; other processes on Windows only) | `cpu` → `atrium_sqlserver_cpu_sql_process_ratio`, `atrium_sqlserver_cpu_other_process_ratio` |
| Buffer/cache hit ratio, page life expectancy | Collected | `buffer_cache` → `atrium_sqlserver_buffer_cache_hit_ratio`, `atrium_sqlserver_page_life_expectancy_seconds` |
| Physical reads/writes, I/O, IOPS, I/O latency | Collected per database file | `file_io` → reads, writes, bytes and I/O stall seconds per file, e.g. `atrium_sqlserver_database_file_reads_total` (counters) |
| Logical reads (page lookups) | Planned | — |
| Query latency | Planned | — |
| Rollbacks, user transactions | Planned | — |
| Free space and growth trends | Planned | — |
| TempDB pressure | Planned | — |
| Error events | Planned | — |

Output expectations: stable metric names; labels for environment, target,
database, instance and, where safe, application; dashboards for overview,
waits/locks, sessions, storage and regressions. Only
`infra/helm/atrium/files/grafana/dashboards/sqlserver-overview.json` exists so far.

## Safety rules
- Credentials are referenced by `credential_ref` only.
- The collector logs in with only `VIEW SERVER STATE` and `VIEW ANY DEFINITION`
  and warns when its login is sysadmin-equivalent (`sysadmin` or
  `CONTROL SERVER`)
  ([login permissions](../guides/database-targets.md#collector-login-permissions)).
- Production probes must be non-blocking: every probe passes the
  [probe review checklist](#probe-review-checklist), and every collector batch
  runs with `LOCK_TIMEOUT` 1s and `DEADLOCK_PRIORITY LOW`
  ([what the collector runs](../guides/database-targets.md#what-the-collector-runs-on-the-server)).
- Probe definitions are versioned and can be disabled instead of deleted.
  The built-in catalog is versioned with the code, and the API holds one
  read-only definition per catalog probe; custom definitions, which replace a
  built-in's query, are managed through
  [`/api/v1/probe-definitions`](../reference/metrics-and-endpoints.md#probe-definitions)
  by admins: each change is a new immutable version, and disabling one
  disables every version. Approved probe assignments are rendered into the
  chart's `managedCollectors`
  ([ADR 0014](decisions/0014-probe-assignments-to-runtime-config.md));
  hand-written collectors run what their `probes` list names.
- Runtime collector grouping, target activation, probe activation, and scaling live in `config/integrations.yaml` and Kubernetes delivery.

### Probe review checklist

Apply it to every new or changed probe in the built-in catalog
([`catalog.go`](../../services/db-collector/internal/probes/sqlserver/catalog.go)),
to every `query_template` override in `integrations.yaml`, and to every
version of a custom probe definition created through
[`POST /api/v1/probe-definitions`](../reference/metrics-and-endpoints.md#probe-definitions)
(or added with `POST /api/v1/probe-definitions/{id}/versions`), before it
reaches a shared or production target. A reviewer other than the author signs
it off; an override also needs the target's DBAs. A probe added to the
built-in catalog also needs a migration that seeds its definition: a test
fails until one does.

**Read-only and least privilege**

- [ ] One read-only query: `SELECT`, optionally with CTEs. No DML, DDL,
  `EXEC`, `DBCC`, temporary tables, `BEGIN TRAN` or `USE`. A bare stored
  procedure name does not work either: every batch starts with the session
  settings, so it is no longer sent as a procedure call. Loading the
  configuration rejects a `query_template` that is not one read-only
  `SELECT` ([rules](../reference/configuration.md#collectors)), the API
  refuses to store a custom definition whose query fails the same check, and
  the built-in queries pass it in their tests; the check is a safety net, the
  read-only login is the control.
- [ ] Returns a bounded number of rows: well under the probe's `max_rows`
  (default 10000) on the largest target, or the probe fails every cycle.
- [ ] Needs no grant beyond `VIEW SERVER STATE` and `VIEW ANY DEFINITION`.
  `make test-sqlserver` runs every catalog probe as such a login; a view the
  login cannot see often returns no rows instead of an error, so check the
  probe returns data.
- [ ] Does not change session settings: no `SET LOCK_TIMEOUT`,
  `SET DEADLOCK_PRIORITY`, `SET TRANSACTION ISOLATION LEVEL`, and no locking
  hints (`HOLDLOCK`, `UPDLOCK`, `TABLOCK`, `XLOCK`, `NOLOCK`).

**Non-blocking**

- [ ] Reads server-level DMVs (`sys.dm_os_*`, `sys.dm_exec_*`) or
  server-scoped catalog views (`sys.master_files`, `sys.databases`), never user
  tables. DMVs read in-memory state and take no data locks; catalog views can
  briefly wait on metadata locks during DDL, which the 1-second `LOCK_TIMEOUT`
  bounds.
- [ ] No per-database iteration (`sp_MSforeachdb`, cursors, dynamic SQL), no
  cross-database metadata functions per row (`OBJECT_NAME(id, db_id)`,
  `OBJECT_SCHEMA_NAME`), and nothing that scans data
  (`sys.dm_db_index_physical_stats`, `sys.dm_db_database_page_allocations`).
- [ ] Joins to `sys.dm_exec_sql_text` or `sys.dm_exec_query_plan` are bounded
  (`TOP`) and justified; they are expensive on busy servers.
- [ ] Measured on a production-sized non-production server
  (`SET STATISTICS TIME, IO ON`): it finishes far below the probe timeout
  (half the scrape interval, at most 10s) and well below the 1s lock timeout
  when nothing blocks it.

**Bounded output**

- [ ] Returns one row per bounded entity (wait type, counter, database file,
  status), never per session, request, query text, login or host, and at most
  one row per label set (the live test fails on duplicate series).
- [ ] Label columns come from a small, stable set and are trimmed (`RTRIM` on
  `nchar` columns such as `counter_name`).

**Metrics and tests**

- [ ] Metric names follow Prometheus conventions with the unit in the name;
  cumulative values are exported as counters (TODO §2.4).
- [ ] Metric descriptors, the
  [metrics reference](../reference/metrics-and-endpoints.md#sql-server-probe-metrics),
  dashboards and rules are updated together (a Go test fails on references to
  metrics the catalog does not emit).
- [ ] `make test` and `make test-sqlserver` pass.

## Collector recovery and high availability (planned)

Status: items 1-3 are implemented for a single collector replica (failure
isolation, a hard deadline per probe so a query the driver cannot cancel never
stalls collection, backoff, freshness metrics, per-probe error counters and
durations, stale-series cleanup, readiness, reload rollback); see the [DB collector README](../../services/db-collector/README.md#failure-isolation-and-readiness). Items 4-5 remain planned.

The remaining items are planned improvements, not guarantees of the current runtime. The
current SQL path is remote queries -> custom collector's in-memory metrics ->
Prometheus scrape/storage -> Grafana. There is no separate custom loader or
durable collector-side sample queue. PostgreSQL and Redis do not buffer this
metric stream.

Implement recovery before adding replicas:

1. Isolate probe and target failures. A failed query must not permanently stop
   unrelated collection. Retry transient failures with bounded exponential
   backoff and jitter, enforce query/concurrency budgets, and surface persistent
   credential/configuration failures without creating retry storms.
2. Expose per-target/probe success, failures, last successful collection time,
   and freshness. Remove or expire stale series, including series belonging to
   removed targets. An HTTP endpoint being reachable is not proof that SQL
   collection is healthy.
3. Make readiness reflect expected collector state. Define degraded behavior
   separately from process liveness: an unavailable database should not cause
   endless container restarts. Add deployment probes and recovery policies for
   actual process failures, and test reload failure/rollback behavior before
   claiming that the previous working runtime is preserved.
4. Design target ownership and takeover across replicas. Evaluate active/standby
   ownership or sharding with reassignment; use leases plus fencing, or an
   equivalent mechanism, to limit duplicate SQL polling during partitions and
   takeover. Increasing replica count alone is not an HA design. Account for
   rolling updates, node placement, takeover delay, and aggregate database load.
5. Define downstream resilience independently. Agree recovery-time and acceptable
   data-gap objectives, then choose storage availability and any durable
   buffering/replay mechanism needed to meet them. Specify retention, disk
   limits, backpressure, replay ordering, and duplicate handling. Buffering can
   preserve already-collected samples; it cannot reconstruct observations missed
   while collection was stopped.

Validate with non-production target outages, slow queries, collector/process and
node loss, network partitions, configuration reloads during failure, and
Prometheus/downstream outages. Record recovery time, data gaps, stale or duplicate
series, effects on healthy targets, and database load. Keep these reliability
improvements in the core hardening phase; they do not depend on the later Alloy
evaluation.

## Custom collector and Alloy comparison (late roadmap)

The agreed plan is a parallel evaluation, not a decision to replace the custom
collector. It is the last planned roadmap phase, after the core workflows,
reliability work and Oracle integration are validated. Perform the experiment in
a dedicated comparison branch created at that time; do not mix it with ordinary
collector fixes or introduce Alloy into production as part of planning.

Oracle work starts with a short, time-boxed check of what Alloy and established
Oracle exporters cover. It is a desk check, not this comparison. It decides
whether to build a custom Oracle collector, adopt Alloy for Oracle, or run this
comparison before the Oracle phase, so that a custom collector is not built only
to be replaced.

Run the custom collector and Grafana Alloy against equivalent non-production
SQL Server workloads. Isolate their metric identities or storage destinations so
dashboards, recording rules, and alerts cannot accidentally double-count the
same database. Budget the combined polling load; use controlled sequential runs
as well when concurrent collection would distort performance measurements.

Use the same workload, intervals, permissions, and comparable queries wherever
possible. Compare:

- Signal coverage across waits, blocking, sessions, memory, storage, and
  throughput; units, counter/gauge semantics, labels, and cardinality.
- Custom query support, structured investigation evidence, and integration with
  Atrium's target/probe configuration and operator workflows.
- Database overhead, collector CPU/memory, connection limits, query timeouts,
  least privilege, credentials, and TLS behavior.
- Reload behavior, error isolation, retries, target ownership/takeover, freshness,
  and behavior during collector, node, network, and downstream outages.
- Buffering/replay limits, recovery time, gaps, duplicates, deployment complexity,
  upgrades, diagnostics, and ongoing maintenance cost.
- SQL Server and Oracle fit, assessed separately rather than assuming
  equivalent engine support.

Record versions/configurations, reproducible tests, measurements, coverage gaps,
and operational tradeoffs in a decision record on the comparison branch. Verify
Alloy's capabilities at evaluation time. The outcome may be to retain the custom
collector, use Alloy for suitable workloads, or keep both available for distinct
needs. Supporting both requires a shared telemetry contract and explicit
per-target/backend selection, ownership, and duplicate-prevention rules; it does
not imply routinely polling every production target twice. Review the evidence
before merging any selected integration or planning production adoption.

## Metadata model

The control-plane tables (`database_targets`, `probe_definitions`,
`probe_assignments`) and the deferred asset/topology extension are described in
the [data model](data-model.md#database-observability).
