# 0008. Extract OutSystems 11 logs from the logging database

- Status: Accepted. Extraction, the eight log-type queries, the watermark
  volume, the Loki path, derived metrics and dashboards are built. The survey
  of a real logging database ran on 2026-10-19: the fixtures hold its DDL,
  and each query has been reviewed against it
  ([TODO 3.3](../../../TODO.md)).
- Date: 2026-10-10. Amended 2026-10-26: the recount (below) shows the rows
  committed past the lag, which the log delay metric could not. Amended
  2026-11-04: the delivery row names the duplicates a Collector restart can
  leave in Loki.

## Context

The first OutSystems deployment Atrium must observe is **OutSystems 11
self-managed** (Traditional and Reactive apps). Unlike OutSystems 11 on
OutSystems Cloud (log streaming add-on) and ODC (App Analytics Stream), it has
no native telemetry export: the platform writes its logs to the `oslog_*`
tables of its logging database, a SQL Server database. The SRE team already
has queries that read those tables well.

The phase 2 plan assumed a push path: something sends JSON events to the OTel
gateway, which normalizes them. Nothing in a self-managed installation sends
those events, so that path does not apply. Atrium needs to poll the logging
database, under the same rules as the SQL Server collector: no agents on
database hosts, a least-privilege login, bounded queries that never block the
platform.

The db-collector already has everything such a poller needs except the log
handling itself: SQL Server connections with pooling, credential references,
login checks and session settings (`LOCK_TIMEOUT`, `DEADLOCK_PRIORITY LOW`), a
hard deadline per query, per-target failure isolation and backoff, hot reload,
readiness and self-metrics.

## Decision

| Concern | Choice | Why |
| --- | --- | --- |
| Source | The `oslog_*` tables of the OutSystems 11 logging database, read with a login that has `SELECT` on them and nothing else | The only complete log source in a self-managed installation; no agent on the platform servers. |
| Where the extractor runs | A new collector kind, `outsystems_logs`, in the **db-collector** | Reuses the SQL Server connector, safety settings, reload, backoff and readiness instead of copying them into another service. The logging database is a database; the collector already owns how Atrium reads databases. |
| Where the queries live | One `.sql` file per log type in [`services/db-collector/internal/probes/outsystems/queries/`](../../../services/db-collector/internal/probes/outsystems/queries/README.md), embedded in the binary. The file name is the log type (`error.sql` → `error`) | Queries are code: reviewed with the [probe review checklist](../database-observability.md#probe-review-checklist), tested in CI against SQL Server and versioned with the release. Not in Prometheus or OTel Collector config, which never talk to SQL Server. |
| Choosing log types | `probes:` in the collector's YAML lists the log types to extract, as for the `sqlserver` kind. A `query_template` override per environment is allowed under the same contract and review | Same configuration model and hot reload as the SQL Server probes (ADR 0002). |
| Query contract | Parameters `@from` (exclusive), `@to` (inclusive) and `@limit`; at most `@limit` rows ordered by an `Instant` column. Every other column becomes a log attribute **under its OutSystems name, or under one standard name where the views disagree** (entry point, operation, server, session, caller address, duration unit); `Message` becomes the log body; an optional `Severity` column sets the severity. Details in the [queries README](../../../services/db-collector/internal/probes/outsystems/queries/README.md) | OutSystems field names stay queryable where they are consistent, so SRE habits carry over; one name per concept lets the derived metrics, the request timeline and Loki searches work across log types. The query author, who knows the tables, controls joins, filters and severity. |
| Incremental reads | A watermark per target and log type: read `Instant` in (`watermark`, `now − lag`], advance only after the batch is delivered. When a batch is full, rows sharing its last `Instant` are left for the next cycle. The watermark is stored in a file on the collector's volume; without one, start at `now − initial_lookback` | No gaps from rows the log service commits late (`lag`), none at batch boundaries, and a restart resumes where it stopped. |
| Weekly rotation | Each query reads the current week's view (`oslog_<TYPE>`) and the previous week's (`oslog_<TYPE>_Previous`) with `UNION ALL`. The platform keeps ten weekly cycle tables per log type and switches the views every Friday at 23:45 | Rows logged just before the switch are committed into the old table after it; reading only the current view would lose them every Friday, and could never look back past the switch. |
| Lag per log type | `lag` is set per collector and can be overridden per log type (`probes[].lag`) | A `screen` or `cyclic_job` row carries the start of the operation as `Instant` but is written when it ends, so its lag must cover the longest screen request or timer; the other log types keep the short default. |
| Rows committed past the lag (2026-10-26) | Once a minute, each log type counts again, per minute of `Instant`, the rows its own query returns in the `recount_window` (default 10 minutes) behind its watermark, and compares them with the rows it delivered there. More rows in the database are late rows, counted in `atrium_outsystems_late_rows_total` with an alert; fewer are rows read twice, `atrium_outsystems_duplicate_rows_total`. Each minute's difference is counted once. Nothing is read again or sent | The extractor never reads behind its watermark, so a late row was lost silently: only the derived request rate dipped. Counting by minute finds each late row without a row identity, which the log tables do not have; wrapping the log type's own query counts exactly what extraction reads, overrides included, and SQL Server answers it from the `Instant` index (a few hundred logical reads for 100,000 rows). Sending the late rows too would need Loki to take rows out of order and a way to tell them from rows already sent; counting first shows whether it is needed. |
| Delivery (amended 2026-11-04) | OTLP/HTTP logs to the OTel Collector (`opentelemetry.endpoint`), then Loki. At least once: a failed export is retried from the same watermark, so a retried batch can appear twice in Loki. So can a request the OTel Collector kept though the db-collector did not see it accepted (an attempt that timed out, a Collector restart): Loki drops many of those copies, not all ([delivery](../application-observability.md)) | Keeps Loki behind the Collector like every other log source; the Collector stays the place for routing and processing. |
| Loki labels | Resource attributes `service.name` (the application), `deployment.environment.name` and `outsystems.log_type`. User, session and request IDs, client IP, module, screen and the rest stay structured metadata | Few streams per application; high-cardinality fields stay filterable without becoming labels. |
| Derived metrics | Computed by the extractor from the rows it reads and exposed on the collector's `/metrics`, scraped by Prometheus. The catalog is in [application observability](../application-observability.md#derived-metrics) | Exact counts and durations from every row, testable with fixtures, and the same Prometheus-scraped model as the SQL Server metrics. |
| Traces | None in this decision. Synthesizing request traces from `Request_Key` is designed in [application observability](../application-observability.md#request-traces) and needs its own decision (storage) | First prove the identifiers are reliable in real logs. |

Considered and not chosen:

- **The OTel Collector's `sqlquery` receiver.** No custom code, but it tracks
  progress by one column only, so it either loses rows that share an `Instant`
  at a batch boundary or rereads them; it has no lag window for late commits,
  no hard deadline or `LOCK_TIMEOUT` guarantee, and none of the collector's
  readiness and backoff.
- **The OTel gateway.** It would need its own copy of the SQL Server
  connector, safety settings, reload and readiness.
- **A separate extractor service.** The same copy, plus another image and
  deployment, for no isolation the per-collector failure isolation does not
  already give.
- **Queries in `integrations.yaml` only.** Editable without a release, but
  unreviewed SQL against a production database, with no CI test. The
  per-environment `query_template` override covers the exceptions.

## Consequences

- The db-collector is no longer stateless for this kind: it keeps a watermark
  per target and log type, so its StatefulSet gets a small persistent volume.
  The `sqlserver` kind stays stateless.
- The db-collector now pushes logs over OTLP as well as serving metrics, so it
  needs network access to the OTel Collector (NetworkPolicy egress).
- The OTel gateway takes no part in OutSystems 11 ingest. Its JSON event
  endpoint and alert sink are superseded; the service was then removed
  ([ADR 0009](0009-remove-the-otel-gateway.md)).
- CI needs a SQL Server fixture per log type with the cycle tables and the
  two views the query reads, built from the real DDL, and rows copied from
  the logging database on both sides of a rotation.
- OutSystems 11 on OutSystems Cloud and ODC, when needed, send OTLP straight to
  the OTel Collector; the attribute names they use are mapped there, not in
  the extractor.
- `Instant` is in the platform servers' own time zone: OutSystems Cloud runs
  UTC, a self-managed installation whatever its front-ends and database share
  (the platform requires them to agree). The time zone is a per-collector
  setting (default UTC), confirmed per installation against the database. In
  a zone with daylight saving time, rows logged in the hour that repeats when
  clocks go back can be missed; UTC avoids it.
- Rows the platform's log service commits later than the lag after their
  `Instant` are missed, and the recount counts them (`OutSystemsLateLogRows`
  fires) while they are within its window; rows committed later still go
  uncounted. For `screen` and `cyclic_job` the lag also delays every row by
  the longest operation it has to cover. The log delay metric
  (`atrium_outsystems_log_delay_seconds`) measures the newest row read,
  which is never newer than the lag: it shows how fresh extraction is, not
  how far behind the log service is, so it cannot warn of late commits; the
  late-row counter does.
- The recount's delivered counts are in memory. After a restart, or a reload
  that changes `recount_window` or the query, it compares only the minutes
  after the watermark it starts again at. A
  `query_template` must be a query a derived table holds (no `WITH`,
  `OPTION` or `FOR XML`, and an `ORDER BY` only with `TOP` or `OFFSET`), or
  the collector sets `recount_window: 0s`.
- The duplicate-row counter sees rows the extraction read twice, or rows
  deleted after they were sent. It does not see a batch sent again after
  the Collector had taken it, which delivery at least once allows.
