# Metrics and Endpoints Reference

Ports, HTTP endpoints and Prometheus metrics exposed by the local stack and
Heartbeat services. For how the pieces connect, see the
[architecture overview](../architecture/overview.md).

## Local ports (kind)

The kind cluster ([`infra/kind/cluster.yaml`](../../infra/kind/cluster.yaml))
maps fixed NodePorts to these ports on 127.0.0.1 only.

| Service | Host port(s) | Notes |
| --- | --- | --- |
| DB collector | 8082 | Heartbeat service |
| Prometheus | 9090 | 15s scrape interval |
| Grafana | 3001 | Local login `admin`/`admin`, anonymous viewer enabled. **Local only.** |
| Alertmanager | 9093 | |
| OpenTelemetry Collector | 4317 (OTLP gRPC), 4318 (OTLP HTTP) | `full` profile. Heartbeat's own logs use a third port, 4319, in-cluster only |
| Loki | none | In-cluster only (`loki:3100`); query it through Grafana, or `kubectl port-forward svc/loki 3100` |
| Tempo | none | In-cluster only (`tempo:3200`, OTLP on `tempo:4318`); query it through Grafana, or `kubectl port-forward svc/tempo 3200`. `full` profile |
| SQL Server (dev target only) | 127.0.0.1:11433 | Loopback-bound Docker container; see the [local development guide](../guides/local-development.md#sql-server-sandbox) |

Inside the cluster every component keeps its Compose-era name and port
(`prometheus:9090`, `alertmanager:9093`, `loki:3100`, `otel-collector:4318`,
`db-collector:8082`), and NetworkPolicies admit only the components that use
each one ([network access](../guides/kubernetes-local.md#network-access)): a
pod of your own times out on them, so reach them through the host ports above,
Grafana or `kubectl port-forward`. PostgreSQL and Redis are not deployed
until a service uses them (ADR 0003).

## DB collector (`:8082`)

| Endpoint | Auth | Behavior |
| --- | --- | --- |
| `GET /metrics` | none | Prometheus exposition: probe metrics and self-observability |
| `GET /healthz` (alias `/healthcheck`) | none | Liveness only; 200 while the process serves HTTP |
| `GET /readyz` | none | 503 until every collector finishes its first cycle, when a collector is failed or crash-looping, when a collector has not completed a cycle within 2× its interval + 10s, or when the runtime diverged after a failed rollback. A monitored database being down does **not** make it unready, even when every target fails: readiness says whether the collector itself works, so a database outage never takes it out of its Service or fails a `helm upgrade --wait`. Target health is `heartbeat_collector_target_up` and the target states below. The body is only `{"status":"ready"}` or `{"status":"not_ready"}`; the reasons are in `GET /admin/config`. |
| `GET /admin/config` | `Authorization: Bearer $HEARTBEAT_ADMIN_TOKEN` | Diagnostics, see below |
| `POST /admin/config/reload` | `Authorization: Bearer $HEARTBEAT_ADMIN_TOKEN` | 400 invalid config, 500 apply failure, 503 before the first reconcile or while another reload holds the lock. The body includes the error text and the readiness report |

Reload can also be triggered with `SIGHUP` or file polling.

`GET /admin/config` returns:

| Field | Content |
| --- | --- |
| `version`, `loaded_at`, `last_reload_at` | Active config version (SHA-256 of the file) and timestamps |
| `last_reload_err`, `runtime_diverged`, `rollback_err` | Why the last reload was rejected or failed; set until the next successful reload. Monitoring sees it as [`heartbeat_collector_config_last_reload_successful`](#config-reloads) 0 |
| `warnings` | Insecure startup settings, e.g. `sqlserver_trust_server_certificate` |
| `readiness` | What `/readyz` decided and why: `status`, `reasons`, and per collector its phase, cycle age, restarts and targets. Each target has its state (`ok`, `failed`, `backoff`, `blocked` for an `outsystems_logs` target waiting for the OTel Collector, or `degraded` for a target with some probes failing), consecutive failures, last success and the raw driver `error` of its last failed cycle, kept while it backs off, which can name the host, port and login but never contains the password. A `blocked` target's `error` says which log type waits, with how many rows, and for how long. A `degraded` target lists its failing `probes`, each with its state (`failed`, or `backoff` while it waits out its own backoff), consecutive failures, next attempt and `error` |
| `config` | The active config, redacted ([configuration reference](configuration.md#validation-and-reload-behavior)) |

```bash
curl -s -H "Authorization: Bearer local-admin-token" localhost:8082/admin/config | jq .readiness   # kind
```

**Admin authentication.** Both admin endpoints go through one check: exactly
one `Authorization: Bearer <token>` header (scheme case-insensitive), whose
SHA-256 digest is compared in constant time with the digest of
`HEARTBEAT_ADMIN_TOKEN` computed at startup, so neither the token nor its
length leaks through timing. A missing or wrong token gets 401 with a
`WWW-Authenticate: Bearer` challenge before the method is checked (an
authenticated wrong method gets 405). Admin responses are `Cache-Control:
no-store`. With no token configured, both endpoints always answer 401.

**Network access.** In Kubernetes the `db-collector` NetworkPolicy admits only
Prometheus and the configured operator peers to port 8082
([what the chart deploys](../guides/kubernetes-local.md#what-the-chart-deploys)).
Operators reach the admin endpoints with
`kubectl -n heartbeat port-forward pod/db-collector-0 8082`, which
NetworkPolicy does not apply to, or from a listed peer such as a VPN range.

## API (`:8081`)

The control plane
([ADR 0013](../architecture/decisions/0013-api-design-and-subsystem-boundaries.md)).
Not deployed yet: the chart adds it with PostgreSQL and Valkey [4.1]. Its
contract is [`apps/api/openapi.yaml`](../../apps/api/openapi.yaml); `/metrics`
is served beside it.

| Endpoint | Auth | Behavior |
| --- | --- | --- |
| `GET /metrics` | none | Prometheus exposition: [request metrics](#api-requests), [audit delivery](#audit-delivery), Go runtime and process metrics |
| `GET /healthz` | none | Liveness only; 200 `ok` while the process serves HTTP. It checks no dependency, so a PostgreSQL or Redis outage never restarts the pod |
| `GET /readyz` | none | 200 when every readiness check passes, 503 otherwise. The checks run concurrently on each call, 2s at most in all; a check without an answer by then fails. `postgresql`: a query reaches the database and its schema version is at least the newest migration the API embeds, and that migration is not dirty (a newer version passes, dirty or not, so the previous release keeps serving while the next one migrates). `redis`: a `PING` reaches Redis (Valkey) and authenticates. Each passes again on its own once its dependency is back. The body is only `{"status":"ready"}` or `{"status":"not_ready"}`; the failing checks and their errors (`schema not migrated`, a refused connection, a failed login, never a password) are logged when the status changes |

On `SIGTERM` or `SIGINT` the API stops accepting connections and its audit
drainer, waits up to 10s for the requests in flight, closes the rest, then
closes its PostgreSQL and Redis connections. An audit export in flight is
abandoned, and its records are sent again by another replica or the next
one; records the Collector already accepted are still deleted, within 5s.

## Metrics

### SQL Server probe metrics

Every probe metric carries `environment` and `target` labels, plus the probe's
own label columns. Names follow Prometheus conventions: base units (seconds,
bytes) and a `_total` suffix on counters only.

| Probe | Source view | Metric | Type | Extra labels | Series per target |
| --- | --- | --- | --- | --- | --- |
| `waits` | `sys.dm_os_wait_stats` | `heartbeat_sqlserver_wait_seconds_total` | counter | `wait_type` | one per wait type with non-zero wait time, benign idle waits excluded: usually tens to low hundreds, at most the ~1,000 wait types SQL Server defines |
| `blocking` | `sys.dm_exec_requests` | `heartbeat_sqlserver_blocked_requests` | gauge | `blocking_session_id` | one per blocking session; none while nothing is blocked |
| `sessions` | `sys.dm_exec_sessions` | `heartbeat_sqlserver_sessions` | gauge | `status` | one per session status (about 5) |
| `memory_pressure` | `sys.dm_os_performance_counters` | `heartbeat_sqlserver_total_server_memory_bytes` | gauge | none | 1 |
| `storage` | `sys.master_files` | `heartbeat_sqlserver_database_file_size_bytes` | gauge | `database_name`, `file_name`, `file_type` | one per database file; tempdb files report their configured startup size, not their current size |
| `throughput` | `sys.dm_os_performance_counters` | `heartbeat_sqlserver_batch_requests_total` | counter | none | 1 |
| `throughput` | `sys.dm_os_performance_counters` | `heartbeat_sqlserver_transactions_total` | counter | none | 1 (server-wide `_Total`) |
| `cpu` | `sys.dm_os_ring_buffers` | `heartbeat_sqlserver_cpu_sql_process_ratio` | gauge | none | 1 |
| `cpu` | `sys.dm_os_ring_buffers` | `heartbeat_sqlserver_cpu_other_process_ratio` | gauge | none | 1 on Windows, none on Linux |
| `buffer_cache` | `sys.dm_os_performance_counters` | `heartbeat_sqlserver_page_life_expectancy_seconds` | gauge | none | 1 |
| `buffer_cache` | `sys.dm_os_performance_counters` | `heartbeat_sqlserver_buffer_cache_hit_ratio` | gauge | none | 1 |
| `file_io` | `sys.dm_io_virtual_file_stats`, `sys.master_files` | `heartbeat_sqlserver_database_file_reads_total`, `heartbeat_sqlserver_database_file_writes_total` | counter | `database_name`, `file_name`, `file_type` | one per database file each |
| `file_io` | `sys.dm_io_virtual_file_stats`, `sys.master_files` | `heartbeat_sqlserver_database_file_read_bytes_total`, `heartbeat_sqlserver_database_file_written_bytes_total` | counter | `database_name`, `file_name`, `file_type` | one per database file each |
| `file_io` | `sys.dm_io_virtual_file_stats`, `sys.master_files` | `heartbeat_sqlserver_database_file_read_stall_seconds_total`, `heartbeat_sqlserver_database_file_write_stall_seconds_total` | counter | `database_name`, `file_name`, `file_type` | one per database file each |

**Counters.** Waits, throughput and file I/O are cumulative in SQL Server (the
`Batch Requests/sec` and `Transactions/sec` performance counters are totals
despite their names), so they are exported as counters with the value SQL
Server reports. Query them with `rate()` or `increase()`, never raw. A SQL
Server restart, a failover, or `DBCC SQLPERF('sys.dm_os_wait_stats', CLEAR)`
lowers the value; `rate()` and `increase()` treat that drop as a counter reset,
so rates stay correct (the increase between the last scrape before the reset
and the reset itself is lost). A failed probe, a target backing off or a
collector restart leaves a gap but no reset: the counter resumes at SQL
Server's value. A wait type appears when its wait time first becomes non-zero,
so its first increase after SQL Server starts is not counted.

The `waits` probe leaves out benign idle waits (system tasks sleeping, queues
waiting for work, timers such as `SLEEP_TASK`, `LAZYWRITER_SLEEP` or
`XE_TIMER_EVENT`), which grow by about a second per second on an idle server
and would dominate top-N panels. The list follows Paul Randal's widely used
wait statistics query and lives in
[`catalog.go`](../../services/db-collector/internal/probes/sqlserver/catalog.go).

**CPU.** SQL Server's scheduler monitor writes one record a minute with the
host's CPU split over that minute: the SQL Server process, other processes,
and idle. The `cpu` probe reads only the newest record, so both ratios (0–1,
of all the CPUs SQL Server sees) change once a minute whatever the scrape
interval, and lag the real load by up to a minute; idle is `1 -` their sum.
SQL Server on Linux reports no idle time (always 0, measured on 2019 and
2022), so `heartbeat_sqlserver_cpu_other_process_ratio` is exported on Windows
only (the probe reads the platform from `@@VERSION`). Nothing is exported in
the first minute after SQL Server starts, nor while the newest record is 3
minutes old or more, so a scheduler monitor that stops writing shows as a gap
rather than a frozen value.

**Buffer cache.** Page life expectancy (seconds) and the buffer cache hit ratio
(0–1) come from the server-wide `Buffer Manager` performance counters, on
default and named instances alike. There is no fixed good page life
expectancy; compare a target with its own baseline. The hit ratio stays close
to 1 on most servers even under memory pressure, so read it together with page
life expectancy and file reads. Both are server-wide: on a server with several
NUMA nodes, a page life expectancy drop on one node can hide in the
`Buffer Manager` value. Per-node values (`Buffer Node` counters) are planned
(TODO §11.2).

**File I/O.** `file_io` exports six counters per database file, labelled like
the `storage` probe's file size: read and write operations, bytes read and
written, and I/O stall seconds (the time SQL Server waited for reads or writes
to complete). Average latency per operation is the stall rate over the
operation rate, e.g.
`rate(heartbeat_sqlserver_database_file_read_stall_seconds_total[5m]) / (rate(heartbeat_sqlserver_database_file_reads_total[5m]) > 0)`,
where `> 0` leaves no value instead of dividing by zero for an idle file. The
counters restart at 0 when SQL Server restarts or a database comes back online,
which `rate()` treats as a reset. A stock instance has about 10 files (60
series); budget 6 series per file, so a server with 100 databases and two files
each exports about 1,200. `storage` and `file_io` label a database whose name
SQL Server does not return (the login lacks `VIEW ANY DATABASE`, or the
database was dropped during the query) `database_id:<id>`.

A target, identified by environment and name, may be enabled in only one
sqlserver collector: configuration that lists it twice is rejected, because
both collectors would write the same series and double the load on the
database. The exporter also keeps each counter series to a single writer and
logs any other write instead of applying it.

Each catalog metric descriptor sets the type and the unit scale applied to the
query column (milliseconds to seconds, KB or 8 KB pages to bytes). A collector
that overrides a probe's `query_template` must return the same columns in the
same units.

A failed probe clears its series instead of exporting stale values, and a
removed collector's series are deleted. A failing probe fails only itself.
For example, `storage` and `file_io` read `sys.master_files`, which a long
`RESTORE` or schema change can lock until the probe times out. While the
target's other probes succeed, the target is `degraded`, stays up
(`heartbeat_collector_target_up` 1) and keeps collecting, and the failing
probe (`heartbeat_collector_probe_up` 0) backs off on its own, with the same
schedule as a target (the next cycle, then doubling up to 5m, with jitter)
and its series cleared until it succeeds again. A target fails, and backs off
as a whole, only when every probe that ran failed, as when its database is
unreachable or its login is refused; the probes' own backoffs are then
forgotten. When every probe of a target is backing off on its own, they all
run. While a target backs off none of its probes run and all its probe
series are cleared, so they leave a gap instead of a frozen value; only the
`heartbeat_collector_target_*` and `heartbeat_collector_probe_up` series stay.

A probe that returns no rows exports no series:
`heartbeat_sqlserver_blocked_requests` is absent, not 0, while nothing is
blocked. Queries that need a zero fall back to targets whose `blocking` probe
succeeded (`heartbeat_collector_probe_up{probe="blocking"} == 1`), as the
dashboard's Blocked Requests panel does, so neither an unreachable target nor
a failing `blocking` probe reads as 0. The catalog lives in
[`catalog.go`](../../services/db-collector/internal/probes/sqlserver/catalog.go).

### Collector self-observability

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `heartbeat_collector_target_up` | gauge | `collector`, `environment`, `target` | 1 if the last cycle for the target succeeded, including an `outsystems_logs` target that waits for the OTel Collector (`blocked`) and one with some probes failing (`degraded`) |
| `heartbeat_collector_probe_up` | gauge | `collector`, `environment`, `target`, `probe` | 1 if the probe's last execution succeeded, waiting for the OTel Collector included; 0 if it failed, backs off on its own, or its target failed or backs off |
| `heartbeat_collector_target_consecutive_failures` | gauge | `collector`, `environment`, `target` | Failed cycles in a row; a `degraded` cycle is not one |
| `heartbeat_collector_target_last_success_timestamp_seconds` | gauge | `collector`, `environment`, `target` | Unix time of the last cycle that did not fail the target (`ok`, `blocked` or `degraded`); use it for freshness alerts, and `heartbeat_collector_probe_up` for a single probe |
| `heartbeat_collector_cycle_duration_seconds` | gauge | `collector` | Duration of the last collection cycle |
| `heartbeat_collector_cycle_seconds` | histogram | `collector` | Duration of every collection cycle; buckets 10ms, 50ms, 100ms, 500ms, 1s, 5s, 10s, 30s, 60s |
| `heartbeat_collector_cycles_total` | counter | `collector`, `outcome` | Cycles by outcome: `on_time` (finished within its interval), `late` (used the whole interval: the deadline ended what was still running, and the next cycle starts late) or `panic`. Created at 0. A cycle ended by the collector stopping is not counted |
| `heartbeat_collector_cycle_start_delay_seconds` | histogram | `collector` | How much later than the interval after the previous start a cycle started; buckets 10ms, 100ms, 500ms, 1s, 5s, 15s, 60s |
| `heartbeat_collector_db_connections` | gauge | `environment`, `target`, `state` | Connections to a monitored database, `in_use` or `idle`. Read at each scrape, so a target's series go with its last connection pool. Targets on one server and database with one credential share a pool, reported under the first of them, in name order, used in the last 15 minutes; a renamed target takes its series over within 15 minutes |
| `heartbeat_collector_db_connection_waits_total`, `heartbeat_collector_db_connection_wait_seconds_total` | counter | `environment`, `target` | Waits for a free connection, and the time spent in them, including those of the target's pools closed since, as after a credential rotation |
| `heartbeat_collector_http_requests_total` | counter | `method`, `route`, `code` | Requests to the collector's own endpoints, as [the API's](#api-requests) |
| `heartbeat_collector_http_request_duration_seconds` | histogram | `method`, `route` | Time to serve them |
| `heartbeat_collector_target_login_sysadmin` | gauge | `collector`, `environment`, `target` | 1 if the collector's login for the target is sysadmin-equivalent: a member of `sysadmin`, or holding `CONTROL SERVER` (it should be 0; see [login permissions](../guides/database-targets.md#collector-login-permissions)). Checked when the collector first connects to the target and about every 10 minutes after; absent until the first check, and while SQL Server cannot tell |
| `heartbeat_collector_probe_duration_seconds` | histogram | `collector`, `environment`, `target`, `probe` | Probe execution time, failed and timed-out executions included (an abandoned probe is observed at the time it was abandoned); buckets 5ms, 10ms, 50ms, 100ms, 500ms, 1s, 5s, 10s, 30s, 60s |
| `heartbeat_collector_probe_errors_total` | counter | `collector`, `environment`, `target`, `probe`, `reason` | Failed probe executions. `reason` is one of `timeout` (probe timeout or cycle deadline reached while running, including a probe abandoned because it did not stop within 2s, or a database read or write that timed out), `error` (connection, login, query or decoding error, or more rows than the probe's `max_rows`), `not_started` (cycle deadline passed before the probe could start, or an abandoned probe of the target is still running), `panic`. Probes interrupted because the collector is stopping (shutdown or reload) are not counted |
| `go_*`, `process_*` | | none | Go runtime (goroutines, GC, memory) and process (CPU, resident memory, open file descriptors, start time) metrics of the collector itself |

The probe error counters are created at 0 for every scheduled probe and reason
when a collector starts, so `rate()` and `increase()` see the first error.
Targets skipped during backoff run no probes, so they add neither errors nor
durations; `heartbeat_collector_target_up` and
`heartbeat_collector_target_consecutive_failures` cover them. Error text is
logged, never used as a label. The probe series of a collector are deleted when
it stops and recreated when it starts, so a reload that changes a collector
resets its error counters to 0 (a counter reset for `rate()`) and drops the
series of removed targets and probes.

Per target, the self-observability series are 4 target gauges plus, per
scheduled probe, 13 histogram series (10 buckets, `+Inf`, sum, count) and 4
error counters: 157 series for a target with the 9 built-in probes. Probe
metric series are listed in the table above.

### Config reloads

The outcome of every load of `integrations.yaml`: at startup, and on each
reload, whatever triggered it (file polling, `SIGHUP` or
`POST /admin/config/reload`). 7 series in all.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `heartbeat_collector_config_last_reload_successful` | gauge | none | 1 if the last load or reload succeeded; 0 if it was rejected (invalid file) or could not be applied, until the next successful one. The last good config keeps running meanwhile, unless the rollback of a failed apply failed too (`runtime_diverged`, and `/readyz` returns 503). It is 0 exactly while `GET /admin/config` shows a `last_reload_err` |
| `heartbeat_collector_config_last_reload_success_timestamp_seconds` | gauge | none | Unix time of the last successful load or reload: when the active config was loaded |
| `heartbeat_collector_config_reloads_total` | counter | `result` | Loads and reloads by result, the `result` of `POST /admin/config/reload`: `ok`, `invalid_candidate` (unreadable or invalid file, nothing applied), `apply_failed` (valid file, collectors not reconciled; rolled back or diverged), `busy` (gave up waiting for another reload; nothing read, the gauges unchanged). Created at 0 for every result |
| `heartbeat_collector_config_info` | gauge | `version` | Always 1. `version` is the active config's SHA-256, as in `GET /admin/config`. One series, which changes only when a reload activates a different file |

The startup load counts as one `ok`: a file that fails validation at startup
stops the collector instead (it exits, and Kubernetes restarts it), so
`HeartbeatServiceDown` reports that case. A rejected reload therefore matters
twice: the change has not taken effect, and if the file in the ConfigMap stays
invalid, the next pod restart fails. `HeartbeatCollectorConfigReloadFailed`
(below) fires on it.

### OutSystems log extraction

`outsystems_logs` collectors
([configuration](configuration.md#outsystems_logs-collectors)) report these
on the same `/metrics`, labelled `collector`, `environment`, `target` and
`log_type`. Their targets also get the self-observability series above, with
the log type as the `probe` label: one probe execution reads, delivers and
acknowledges up to 10 batches.

A batch the OTel Collector has not accepted by the end of the probe
execution is backpressure, not a failure. Its export carries on in the
background and the log type reads nothing more until the Collector takes it
(the chart's Collector takes the next batch only once Loki has the one
before; see [application observability](../architecture/application-observability.md)).
Meanwhile `heartbeat_outsystems_export_blocked` is 1, the target stays up
(`heartbeat_collector_target_up` 1, no backoff, no probe error), and
`GET /admin/config` shows it as `blocked`, counted in `targets_blocked`,
with what it waits for. A database the collector cannot read still fails
the target as before.

| Metric | Type | Meaning |
| --- | --- | --- |
| `heartbeat_outsystems_extract_rows_total` | counter | Rows extracted and accepted by the OTel Collector |
| `heartbeat_outsystems_extract_lag_seconds` | gauge | How far the watermark trails now, in real time: about the lag (the collector's, or the log type's own) plus the poll interval when extraction keeps up; it grows while extraction fails or falls behind |
| `heartbeat_outsystems_extract_lag_setting_seconds` | gauge | The lag the log type is configured with, its own or the collector's: `heartbeat_outsystems_extract_lag_seconds` never goes below it. Set when the collector starts |
| `heartbeat_outsystems_export_failures_total` | counter | Batches the OTel Collector rejected for good (a 4xx status other than 429). Network errors, timeouts, 429 and 5xx are retried with backoff (up to 5 s apart, 10 s per attempt) until the Collector accepts the batch. The watermark stays, so the rows are read again |
| `heartbeat_outsystems_export_blocked` | gauge | 1 while the log type waits for the OTel Collector to accept a batch, past the probe execution that read it: its extraction is paused, but its target is up. 0 otherwise. `OutSystemsLogExportBlocked` fires once it has been 1 for 2 minutes; meanwhile the lag grows, and `OutSystemsLogExtractionBehind` fires once it has been above 30 minutes for 10 |
| `heartbeat_outsystems_extract_stuck_batches_total` | counter | Cycles that failed because every row of a full batch had the same `Instant`. The log type does not move until `batch_limit` is raised |
| `heartbeat_outsystems_log_delay_seconds` | gauge | Now minus the newest `Instant` extracted, set when rows arrive: about the lag plus the poll interval while extraction keeps up. A row is read only once it is older than the lag, so this cannot show OutSystems' log service falling behind; `heartbeat_outsystems_late_rows_total` does. Absent until the first row |
| `heartbeat_outsystems_late_rows_total` | counter | Rows the recount found in the logging database behind the watermark that were never read: committed later than the lag after their `Instant`. They are not in Loki and are not sent ([ADR 0008](../architecture/decisions/0008-outsystems-11-log-extraction.md)). `OutSystemsLateLogRows` fires on any |
| `heartbeat_outsystems_duplicate_rows_total` | counter | Rows the recount found delivered more often than the logging database holds them: read twice by the extraction, a defect, or deleted from the database after they were sent. It does not count a batch sent again after the OTel Collector had taken it (its answer lost, or a restart before the watermark was saved), which delivery at least once allows ([ADR 0008](../architecture/decisions/0008-outsystems-11-log-extraction.md)), so 0 does not mean Loki holds no row twice. `OutSystemsDuplicateLogRows` fires on any |
| `heartbeat_outsystems_recounts_total` | counter | Recounts run, failed ones included: one in each minute that has a poll |
| `heartbeat_outsystems_recount_failures_total` | counter | Recounts that failed (query error, more than 1,000,000 rows in the window). Late and duplicate rows are not counted meanwhile; extraction goes on |

Two more have other labels. `heartbeat_outsystems_stage_duration_seconds`
(histogram; `collector`, `log_type`, `stage`) is the time of one step of a batch:
`read` (the query), `build` (rows to OTLP records), `export` (until the OTel
Collector accepted it, waiting for Loki included), `derive` (the derived metrics)
and `watermark` (the save to disk); buckets 10ms to 300s, at most 440 series per
collector. `heartbeat_outsystems_held_bytes` (gauge, no labels) is the row data
held in memory now, read, being exported or pending, against the 64 MiB allowed.

**The recount.** Once a minute (in each minute that has a poll), each log type counts again, per minute of
`Instant`, the rows its query returns in the `recount_window` (default 10
minutes, [configuration](configuration.md#outsystems_logs-collectors)) behind
its watermark, and compares them with the rows it delivered in each minute.
Each minute's difference is counted once, however many recounts cover it.
The count wraps the log type's own query, so it reads what extraction reads,
from the `Instant` index. The delivered counts are in memory and start again after the current
watermark after a restart, and when a reload changes `recount_window` or the
query: only the minutes after it are compared. A row committed more than about the window after its `Instant`
goes uncounted.

The counters and `heartbeat_outsystems_export_blocked` are created at 0 when
a collector starts and, like the probe series, deleted when it stops. Error text (contract violations, login and
query errors) is logged and reported per target in `GET /admin/config`.

#### Derived OutSystems metrics

Computed from the rows each batch delivers, labelled `collector`,
`environment` and `application` plus the labels shown. Design, label caps
and the entry-point log types:
[derived metrics](../architecture/application-observability.md#derived-metrics).

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `heartbeat_outsystems_operations_total` | counter | `module`, `log_type`, `operation`, `outcome` | Entry points: screens, Reactive server calls, timers and exposed APIs |
| `heartbeat_outsystems_operation_duration_seconds` | histogram | `module`, `log_type`, `outcome` | Entry-point duration; buckets 0.1 to 60 s |
| `heartbeat_outsystems_operation_seconds_total` | counter | `module`, `log_type`, `operation` | Entry-point seconds, for the mean per operation |
| `heartbeat_outsystems_front_end_operations_total` | counter | `front_end`, `outcome` | Entry points per front-end server |
| `heartbeat_outsystems_integration_duration_seconds` | histogram | `direction`, `type`, `outcome` | Integration calls, consumed and exposed |
| `heartbeat_outsystems_errors_total` | counter | `module` | Error log rows |
| `heartbeat_outsystems_slow_events_total` | counter | `module`, `kind` | Slow SQL and slow extension rows of the general log |
| `heartbeat_outsystems_timer_delay_seconds` | histogram | `operation` | Timer start minus its schedule; buckets 1 s to 1 h |
| `heartbeat_outsystems_screen_state_bytes` | histogram | `module`, `kind` | Traditional web screen session and viewstate size; buckets 1 KiB to 16 MiB |

They are deleted with their collector. Per application, each operation adds
3 counter series (two outcomes and its seconds) and each module, log type and
outcome 13 histogram series (11 buckets, sum, count). The label caps bound
each label, but they multiply: 100 modules and 100 operations per
application and log type, up to 1,000 applications. So each collector holds
at most `max_derived_series` of these series
([configuration](configuration.md#outsystems_logs-collectors)), counted as
Prometheus stores them. Past it, a row that needs a new series is not
counted in that metric; series that exist keep counting, so the metrics of
applications, modules and operations seen first stay right and later ones
are missing. Each metric is admitted on its own: as the budget fills, a row
can be counted in some metrics and not in others. The budget is freed with
the series when the collector stops or a reload changes it.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `heartbeat_outsystems_derived_series` | gauge | `collector` | Derived series the collector holds, as the budget counts them; at most its `max_derived_series` |
| `heartbeat_outsystems_derived_series_overflow_total` | counter | `collector`, `metric` | Rows a derived metric (`metric`, its name) did not count because they needed a new series past `max_derived_series`: one per row and metric. `OutSystemsDerivedSeriesOverflow` fires on any |

Both are created, the counter at 0 for each of the nine metrics, when a
collector starts, and deleted when it stops.

### API requests

The API records every request it serves, `/metrics` and the health endpoints
included. Not scraped yet: the API is not deployed.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `heartbeat_api_http_requests_total` | counter | `method`, `route`, `code` | Requests served, by status code |
| `heartbeat_api_http_request_duration_seconds` | histogram | `method`, `route` | Time from receiving a request to its handler returning; the Prometheus client's default buckets, 5ms to 10s |
| `heartbeat_api_db_connections` | gauge | `state` | PostgreSQL pool connections: `in_use`, `idle` or `constructing` |
| `heartbeat_api_db_connections_max` | gauge | none | The most connections the pool opens |
| `heartbeat_api_db_connection_waits_total`, `heartbeat_api_db_connection_wait_seconds_total` | counter | none | Acquires that found no idle connection, and the time they waited |
| `heartbeat_api_redis_connections` | gauge | `state` | Redis pool connections: `in_use` or `idle` |
| `heartbeat_api_redis_connection_timeouts_total` | counter | none | Commands that gave up waiting for a connection from the pool |

`route` is the pattern that served the request, without the method
(`/readyz`; later `/api/v1/applications/{id}`, never the ID itself), or
`unmatched` when none did: a 404, a 405 or a redirect. `method` is the
request's method, or `other` outside GET, HEAD, POST, PUT, PATCH, DELETE and
OPTIONS. A scan of made-up paths and methods therefore lands in the
`unmatched` and `other` series instead of adding new ones.

### Audit delivery

Every admin or operator change writes an audit record to the `audit_outbox`
table in its own transaction; a drainer in each API replica sends the oldest
records to the OTel Collector and deletes them once it accepts them
([ADR 0016](../architecture/decisions/0016-audit-records-through-an-outbox.md)).
So far the one audited change is `api users create-admin`
([configuration](configuration.md#api)).

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `heartbeat_api_audit_outbox_rows` | gauge | none | Records waiting in the outbox, read at each scrape. Every replica reports the same table. Absent from a scrape while PostgreSQL does not answer within 2s; the rest of the scrape still works |
| `heartbeat_api_audit_outbox_oldest_age_seconds` | gauge | none | Age of the oldest waiting record, 0 when none waits; normally a few seconds at most. `HeartbeatAuditDeliveryStalled` fires above 2 minutes for 2 minutes |
| `heartbeat_api_audit_records_delivered_total` | counter | none | Records this replica delivered: the Collector accepted them and they left the outbox. A record can be delivered twice (an export the Collector took after the API gave up on it); its id tells the copies apart |

The drainer claims up to 100 of the oldest rows at a time with
`FOR UPDATE SKIP LOCKED`, so replicas never send the same rows at once, and
sends them with the OTLP client the db-collector uses (`client="audit"` in
`heartbeat_otlp_export_attempts_total`): one export, retried for up to 30s,
holds a round's rows. It goes on at once while it finds full batches, polls
every 2s once the outbox is empty, and backs off from 1s to 30s while the
Collector fails, logging `audit: delivery failed` once, then every minute,
and `audit: delivery resumed`. A request the Collector accepts while refusing
some of its records (OTLP partial success) is not sent again: those records
are counted in `heartbeat_otlp_export_rejected_records_total{client="audit"}`
and `HeartbeatOTLPRecordsRejected` fires. Without `HEARTBEAT_API_AUDIT_ENDPOINT`, nothing
is sent and records wait. A round that finds records is traced: `audit.drain`
→ `audit.export` and the `db.query` of the delete.

Each record is an OTLP log record: severity INFO, the record's time, the
record as compact JSON in the body
([`audit_record.schema.json`](../../packages/telemetry-contracts/src/audit_record.schema.json)),
attributes `event.name` = `heartbeat.audit` and `log.record.uid` = the record's
id, and the resource `service.name` = `heartbeat-api` with the replica's
`service.version`, `service.instance.id` and `deployment.environment.name`.
There is no `service.namespace`, so audit records stay out of Heartbeat's own
logs (`service_namespace="heartbeat"`). The record's fields are `id`, `time`,
`request_id`, `actor` (a user's `user_id` and `email`, or a `cli` command and
the `host` it ran on), `action`, `entity_type`, `entity_id`, `before`, `after`
and, for runtime config, `config_version`. In Grafana's Explore, on Loki:

```logql
{service_name="heartbeat-api"} | event_name="heartbeat.audit" | json | entity_type="user"
```

### Heartbeat's own telemetry

Both services expose these
([ADR 0018](../architecture/decisions/0018-heartbeats-own-telemetry.md)).

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `heartbeat_service_info` | gauge | `service`, `version`, `revision`, `environment` | Always 1: what is running. `environment` is `telemetry.environment`, where the service itself runs; the `environment` of a `heartbeat_collector_*` series is the monitored target's |
| `heartbeat_telemetry_log_records_total` | counter | `outcome` | The service's own log records sent over OTLP: `exported`, `dropped_queue_full` (the queue holds 1,024 records and 8 MiB, while the Collector is slow or down) or `dropped_export_failed` (two attempts within 5 s failed, or the service stopped first). All are created at 0, and a service with no endpoint stays at 0. Dropped records are still on stderr |
| `heartbeat_telemetry_trace_errors_total` | counter | none | Errors the tracing SDK reported, mostly batches of spans it could not send; it keeps at most 2,048 spans waiting and drops the rest |
| `heartbeat_otlp_export_attempts_total` | counter | `client`, `outcome` | OTLP/HTTP attempts: `accepted`, `retryable` (network error, timeout, 429, 5xx) or `rejected` (any other status). `client` is `self-logs`, `outsystems` on the collector, or `audit` on the API; `self-logs` and `audit` exist only while the service has their endpoint. Created at 0. An attempt cancelled because the service is stopping is not counted |
| `heartbeat_otlp_export_attempt_duration_seconds` | histogram | `client` | Time of one attempt, whatever its outcome; buckets 5ms to 30s. Attempts near 10 s mean the receiver waits: for `outsystems`, the Collector waiting for Loki |
| `heartbeat_otlp_export_bytes_total` | counter | `client` | Compressed bytes of accepted requests |
| `heartbeat_otlp_export_rejected_records_total` | counter | `client` | Records the receiver refused inside a request it accepted (OTLP partial success). Such a request is not sent again, so the records are lost; `HeartbeatOTLPRecordsRejected` fires on any. Created at 0 |

The logs themselves are in Loki with the labels `service_namespace="heartbeat"`,
`service_name` (`db-collector`, `api`) and `deployment_environment_name`:

```logql
{service_namespace="heartbeat", service_name="db-collector"} | detected_level="error"
```

Traces are in Tempo. A collection cycle is one trace, `collector.cycle` →
`collector.target` → `collector.probe` → `db.query` or the `outsystems.read`,
`outsystems.build`, `outsystems.export`, `outsystems.derive` and
`outsystems.watermark` steps of a batch. An API request is `<method> <route>` →
`db.query` and `redis.<command>`. `/metrics` and the health endpoints make none.
A client's `traceparent` is linked, not continued. Spans name what ran, never
query values, command arguments or error text, which can name a host and a
login. A log line in a sampled span has its `trace_id`, and Grafana links the
two ways; a line in a span that was not sampled has none, since Tempo never
gets that trace.

#### What the telemetry costs

Measured on kind on 2026-11-02 under `make patientzero-load LOAD=steady`, the
same way as the [memory budget](../guides/local-development.md#daily-loop),
against the build before it (`2844ec1`), each on a freshly restarted node:

| | Before | With it | Budget |
| --- | --- | --- | --- |
| Full profile in the kind node, peak less PatientZero | 1.44 GiB | 1.79 GiB (Tempo about 210 MiB of it) | 2.5 GiB |
| db-collector resident memory, peak | 24.6 MiB | 32.9 MiB | 20 MiB more at most |
| db-collector CPU | 2.7 millicores | 2.9 millicores | within run-to-run noise (1.8 to 2.7) |
| Collector `/metrics` series; scrape time, mean and max | 1,015; 7.5 and 12.5 ms | 1,697; 7.8 and 10.7 ms | under 100 ms |
| Probe p95 | 35 ms | 30 ms | no regression |
| Rows extracted a second; extraction lag, max | 9.5; 61 s | 9.7; 60 s | no regression |

Tempo kept every trace (`telemetry.traces.sampleRatio: 1` on kind), about 5.6
spans a second, and its memory moved between 140 and 210 MiB against its
256 MiB limit without growing. A log call costs about 1.4 µs and 18
allocations with the OTLP export on, 0.45 µs and none with it off; a request
that is not sampled about 1.7 µs in the tracing middleware.

### Prometheus scrape jobs and rules

Scrape jobs (`prometheus.scrapeConfigs` in the chart's
[`values.yaml`](../../infra/helm/heartbeat/values.yaml)): `prometheus`,
`otel-collector`, `db-collector`, `alertmanager`, `loki`, `tempo`; the `minimal`
profile drops `otel-collector`, `loki` and `tempo`, which it does not deploy. `db-collector` uses
DNS discovery on the headless `db-collector-headless` Service, which lists the
pod even while it is unready, so the collector's failure metrics are still
scraped. Prometheus sends alerts to `alertmanager:9093`.

Rules ([`heartbeat.rules.yml`](../../infra/helm/heartbeat/files/prometheus/rules/heartbeat.rules.yml),
[`heartbeat-config-reload.rules.yml`](../../infra/helm/heartbeat/files/prometheus/rules/heartbeat-config-reload.rules.yml)
for the config reload alert, and
[`heartbeat-collector-missing.rules.yml`](../../infra/helm/heartbeat/files/prometheus/rules/heartbeat-collector-missing.rules.yml)
for a db-collector with no pod, and
[`heartbeat-self.rules.yml`](../../infra/helm/heartbeat/files/prometheus/rules/heartbeat-self.rules.yml)
for Heartbeat's own objectives):

| Rule | Type |
| --- | --- |
| `heartbeat:up:count`, `heartbeat:service_up:ratio` | Recording |
| `heartbeat:sqlserver_wait_seconds:rate5m` | Recording: wait seconds per second by `wait_type`, `rate()` of `heartbeat_sqlserver_wait_seconds_total` |
| `heartbeat:sqlserver_blocked_requests:sum` | Recording: blocked requests per target; 0 when the target's `blocking` probe succeeded and nothing is blocked |
| `heartbeat:sqlserver_sessions:sum` | Recording: sessions per target, all statuses |
| `heartbeat:outsystems_operations:rate5m`, `heartbeat:outsystems_errors:rate5m` | Recording: OutSystems entry points per second by log type and outcome; error log rows per second by module |
| `heartbeat:outsystems_request_duration_seconds:p95_5m`, `heartbeat:outsystems_integration_duration_seconds:p95_5m` | Recording: p95 request and integration latency |
| `heartbeat:outsystems_request_errors:ratio_rate{5m,30m,1h,6h}`, `heartbeat:outsystems_request_slow:ratio_rate{5m,30m,1h,6h}`, `heartbeat:outsystems_requests:rate{1h,6h}` | Recording: availability and latency (2 s) SLIs per application over screens, Reactive server calls and exposed APIs, and their traffic |
| `OutSystemsErrorBudgetBurnFast`, `OutSystemsLatencyBudgetBurnFast` | Alert (critical): 14.4× burn of a 99.9% availability or 99%-within-2-s objective over 1h and 5m, for applications with at least five (availability) or one (latency) requests a minute |
| `OutSystemsErrorBudgetBurnSlow`, `OutSystemsLatencyBudgetBurnSlow` | Alert (warning): 6× burn over 6h and 30m |
| `OutSystemsLogExtractionBehind` | Alert (warning): a log type's extraction lag above 30 minutes for 10m; the OutSystems metrics and SLO alerts of that target are stale |
| `OutSystemsLogExportBlocked` | Alert (warning): `heartbeat_outsystems_export_blocked` 1 for 2m, one alert per target: the OTel Collector has not taken a batch, so its log types read nothing more, usually because Loki is down or slow. Nothing is lost while the rows are still in the logging database's views |
| `OutSystemsLateLogRows` | Alert (warning): `heartbeat_outsystems_late_rows_total` grew in the last 15 minutes, per log type: rows committed later than the lag, not in Loki. Raise the log type's `lag` |
| `OutSystemsDuplicateLogRows` | Alert (warning): `heartbeat_outsystems_duplicate_rows_total` grew in the last 15 minutes: rows the extraction read twice, a defect, or rows deleted from the logging database after they were sent |
| `OutSystemsDerivedSeriesOverflow` | Alert (warning): `heartbeat_outsystems_derived_series_overflow_total` grew in the last 15 minutes, per collector: its derived series reached `max_derived_series`, so rows that need a new series (a new application, module, operation or front-end) are missing from the derived metrics, dashboards and SLO alerts. Raise `max_derived_series` if the collector's memory allows |
| `OutSystemsLogRecountFailing` | Alert (warning): more than half of a log type's recounts failed in the last 15 minutes, of three or more, whatever the poll interval; the reason is in the collector's log |
| `HeartbeatServiceDown` | Alert (warning): `up == 0` for 2m for the DB collector, the OTel Collector, Loki or Tempo; and no `up` series at all for the DB collector, whose DNS discovery finds no target while it has no pod (`absent(up{job="db-collector"})`). Where a name on Prometheus' DNS search path fails instead of answering NXDOMAIN, discovery keeps the collector's last target, and the `up == 0` rule fires instead |
| `LokiDiscardingLogs` | Alert (warning): Loki discarded log rows in the last 10 minutes for a reason other than its rate limit (`loki_discarded_samples_total`); those rows are lost |
| `HeartbeatCollectorLoginElevated` | Alert (warning): `heartbeat_collector_target_login_sysadmin == 1` for 15m, the collector's login for a target is sysadmin-equivalent |
| `HeartbeatCollectorConfigReloadFailed` | Alert (warning): `heartbeat_collector_config_last_reload_successful == 0` for 2m, the collector rejected or could not apply its last config change and still runs the last good config. The reason is `last_reload_err` in `GET /admin/config` and the `config reload rejected` or `config reload failed` line in the pod log. Clears on the next successful reload. On by default; `prometheusRules.configReloadAlert.enabled: false` leaves it out |
| `heartbeat:collector_cycles_late:ratio_rate{5m,30m,1h,6h}`, `heartbeat:outsystems_extract_fresh:ratio_avg{5m,30m,1h,6h}`, `heartbeat:api_request_errors:ratio_rate{5m,30m,1h,6h}`, `heartbeat:api_request_slow:ratio_rate{5m,1h}` | Recording: Heartbeat's own SLIs: late cycles per collector, the share of time a log type's extraction lag stayed within 5 minutes of its configured lag, with a target that is down counted as behind, and the API's failed and slower-than-250-ms requests, without `/metrics` and the health endpoints |
| `HeartbeatCollectorCyclesLateFast`, `HeartbeatCollectorCyclesLate` | Alert (warning): 14.4× over 1h and 5m, and 6× over 6h and 30m, burn of the objective of 99.5% of cycles on time, for collectors with at least 30 cycles an hour. A slow monitored database can cause it as well as Heartbeat, so it stays a warning |
| `HeartbeatExtractionFreshnessBurn`, `HeartbeatExtractionFreshnessBurnSlow` | Alert (warning): the same burn rates against an extraction lag within 5 minutes of its configured lag 99% of the time |
| `HeartbeatAPIErrorBudgetBurnFast` (critical), `HeartbeatAPIErrorBudgetBurnSlow` (warning), `HeartbeatAPISlow` (warning) | Alert: 99.9% of the API's requests succeed and 95% answer within 250 ms, with at least five (errors) or one (latency) requests a minute |
| `HeartbeatServiceRestarting` | Alert (warning): more than 3 process starts in an hour, from `process_start_time_seconds` |
| `HeartbeatServiceMemoryHigh` | Alert (warning): resident memory above `GOMEMLIMIT`, 80% of the container's limit under the chart, for 15m |
| `HeartbeatGoroutinesGrowing` | Alert (warning): over 200 goroutines and growing by more than 20 an hour over 6 hours, for 1h |
| `HeartbeatOwnLogsDropped` | Alert (warning): `heartbeat_telemetry_log_records_total` counted drops in the last 15 minutes |
| `HeartbeatOTLPExportRejected` | Alert (warning): the OTel Collector rejected a request of a client for good in the last 15 minutes |
| `HeartbeatOTLPRecordsRejected` | Alert (warning): the OTel Collector accepted a request but refused some of its records (OTLP partial success) in the last 15 minutes, per client; those records are lost |
| `HeartbeatAuditDeliveryStalled` | Alert (warning): the oldest audit record in the API's outbox above 2 minutes old for 2 minutes, `max()` over the replicas, which report the same table: the Collector does not take audit records, or `HEARTBEAT_API_AUDIT_ENDPOINT` is unset. Changes still commit and no record is lost ([audit delivery](#audit-delivery)) |
| `Watchdog` | Alert: always firing. Alertmanager routes it to the `deadmans-switch` receiver (healthchecks.io in production, ADR 0005), which notifies when it stops arriving |

`files/prometheus/rules/generated/` (in the chart) is loaded but empty; it is
reserved for rules rendered from alert policies. The chart ships all rule files
in the `heartbeat-prometheus-rules` ConfigMap. An alert that can be turned off
has a rule file of its own, which the chart leaves out of the ConfigMap when
its value is false, so operators do not edit rule files:
`prometheusRules.configReloadAlert.enabled` for
`heartbeat-config-reload.rules.yml`. `heartbeat-collector-missing.rules.yml`
is in it only when the release has the db-collector and the bundled Prometheus
scrapes it as job `db-collector` (not disabled, no `job_name` override):
`absent()` fires wherever nothing matches. A shared Prometheus never gets it,
even one that loads this ConfigMap and scrapes the collector as `db-collector`;
it needs its own copy of the rule, for its own job name. `prometheusRules.enabled: false` leaves
out every rule. `make rules-check` validates the
rules and runs their promtool unit tests (`files/prometheus/rules/tests/`). A Go
test fails if a rule, rule test, dashboard, script or metrics doc names a SQL
Server metric the probe catalog does not emit, an OutSystems metric or a
collector self-metric (`heartbeat_collector_*`) the collector does not export,
or if a rule or dashboard reads a counter without `rate()` or `increase()`.

## Known gaps

- **Alerts notify no one by default.** Alerts reach Alertmanager, but the
  chart's default receiver has no integrations
  ([ADR 0009](../architecture/decisions/0009-remove-the-otel-gateway.md)).
  Chat and WhatsApp receivers exist only in
  [`production.example.yaml`](../../infra/helm/values/production.example.yaml).
- **Admin token attempts are not rate limited.** The NetworkPolicy limits who
  can try. Tracked in TODO §15.2.
