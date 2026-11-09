# Application Observability Architecture

How Atrium observes OutSystems applications: where their telemetry comes
from, which fields matter, the metrics derived from it and how requests are
correlated across logs. The decision behind the ingest path is
[ADR 0008](decisions/0008-outsystems-11-log-extraction.md); the operator flow
is in [workflows](workflows.md#application-telemetry-ingestion-outsystems-first).

> **Status: built for OutSystems 11 self-managed** (roadmap phase 2).
> Extraction to the OTel Collector and the derived metrics (data path steps
> 1, 2, 4 and 5) run as the db-collector's `outsystems_logs` kind, switched
> off by default
> ([configuration](../reference/configuration.md#outsystems_logs-collectors));
> the chart ships their recording rules, SLO alerts and dashboards.
> `outsystems.log_type` is one of Loki's index labels
> (`outsystems_log_type` in LogQL; all of them are in the
> [component details](overview.md#component-details)).
> The request timeline from logs is built
> ([step A](#step-a-request-timeline-from-logs-with-phase-2)); synthesized
> traces are designed, not built
> ([step B](#step-b-synthesized-otlp-traces-after-step-a)). Field names and
> log types follow OutSystems 11, and each log-type query was reviewed
> against a real logging database's surveyed DDL
> ([open questions](#open-questions)).

## Scope

| Deployment | Ingest path | Status |
| --- | --- | --- |
| OutSystems 11 self-managed (Traditional and Reactive) | The db-collector's `outsystems_logs` kind reads the `oslog_*` tables of the logging database and sends OTLP logs to the OTel Collector | First, phase 2 |
| OutSystems 11 on OutSystems Cloud | Log streaming add-on sends OTLP straight to the OTel Collector | When needed |
| ODC | App Analytics Stream sends OTLP straight to the OTel Collector | When needed |

Data path for self-managed OutSystems 11:

1. The collector runs each enabled log-type query (one `.sql` file per log
   type, see the
   [query contract](../../services/db-collector/internal/probes/outsystems/queries/README.md))
   for each logging-database target, from its watermark to now minus a lag,
   in batches of at most `batch_limit` rows and about 8 MiB of row data.
2. Each row becomes an OTLP log record: `Instant` is the timestamp, `Message`
   the body, every other column an attribute under its OutSystems name. The
   collector adds `error.fingerprint` to error records. Values longer than
   16 KiB, mostly `Stack` and `EnvironmentInformation`, are cut and the
   record is marked `outsystems.truncated`.
3. Records go to the OTel Collector, then to Loki. Loki indexes only
   `service.name`, `deployment.environment.name` and `outsystems.log_type`
   (`service.namespace` too, which the collector does not set). The records
   hold personal data, so Loki deletes them after 30 days
   (`loki.loki.limits_config.retention_period`; the compactor's
   `delete_request_store` must name the store that holds the chunks:
   `filesystem` on kind, `s3` in production). Compliance confirms the period
   before the first production deploy
   ([ADR 0012](decisions/0012-personal-data-in-telemetry.md)).
4. From the same rows the collector updates the
   [derived metrics](#derived-metrics), which Prometheus scrapes from its
   `/metrics`.
5. The watermark moves only after the batch is delivered: accepted by the
   OTel Collector, whose logs exporter sends batches of at most 4 MiB to
   Loki one at a time, in order, and retries a throttled one until Loki
   takes it (a push above Loki's 6 MB burst is never taken). Loki rejects
   for good a row more than an hour behind the newest row of its stream, so
   batches sent out of order lose rows. The logs pipeline has no batch
   processor, which would accept rows before they are queued.

   The exporter's queue holds one request, on disk (the
   `otel-collector-queue` volume), and the receiver waits for room in it.
   A larger queue does not keep its order across a Collector restart: it
   sends the requests it was sending when it stopped after the rest, and
   with 64 MiB a restart during a three-hour backlog lost 1,297 of 6,000
   rows in the kind test. With one request nothing queues behind the request
   being sent, and the disk keeps that request across the restart: the kind
   test restarts the Collector during backlogs of 45 minutes and three
   hours, with Loki down, and Loki ends with every row of both.

   Delivery is at least once, not exactly once: a row is never lost, but it
   can reach Loki twice. The db-collector sends a request again whenever it
   did not see it accepted, and the Collector can have kept that request
   all the same: an attempt that timed out while the receiver waited for
   room in the queue, or a Collector stopped before it answered, keeps the
   request on disk and sends it after the restart. Loki drops many such
   copies (the same timestamp and line in one stream) but not all of them:
   in one kind run a request of 1,266 rows was sent twice and 73 lines were
   kept twice. Counts from Loki (`count_over_time`) can then be slightly
   high; counts of distinct requests (`count(sum by (Request_Key) ...)`;
   a request's rows share one `Request_Key`) and the
   [derived metrics](#derived-metrics), which the db-collector computes
   from the rows it reads, are not affected. The kind test checks that no
   row is lost and that Loki holds no more lines twice than the Collector
   sent twice.

   So the Collector takes the next batch only once Loki has the one before,
   and while Loki is down it takes none. The collector waits for it without
   treating it as a failure: the export runs in the background, outside the
   probe deadline, and is retried until the Collector accepts it; meanwhile
   the log type reads nothing more, and `atrium_outsystems_export_blocked`
   is 1 (`OutSystemsLogExportBlocked` fires after 2 minutes). The target stays up and does not back off, so it resumes as soon
   as the Collector accepts the batch. The rows were read and the query
   closed before the export started, so nothing is held on the logging
   database while it waits, and the batches held in memory are bounded
   (64 MiB of row data). A backlog drains at about the rate Loki takes it
   ([throughput](#export-throughput)).

### Export throughput

Measured on kind with one log type of 12,000 error rows of about 6 KB
(72 MB) spanning three hours, polled every 10 s:

| Queue | Backlog with Loki up | Backlog after a Loki outage |
| --- | --- | --- |
| One request (this chart) | 85 s, 87 s, 102 s, 105 s | 61 s after Loki was back; the target stayed up while it waited |
| 64 MiB (earlier charts) | 108 s, 177 s | The queue filled, the target failed and backed off, and 15 minutes after Loki was back it held 12,339 lines for the 12,000 rows: batches sent again after 503s |

Loki's ingestion rate limit sets the pace either way: the Collector pushes
4 MiB at a time and waits at least 5 s after a throttled push. The 64 MiB
queue only moved the wait from the collector into the Collector. Other
OTLP sources of logs, none yet, wait the same way; one that gives up sooner
(an OpenTelemetry SDK retries for about a minute) drops its batch, where a
larger queue would have held it.

## OutSystems fields

The extractor and the [request correlation](#request-traces) look these
columns up by name. Other columns pass through unchanged.

| Column | Meaning | In Atrium |
| --- | --- | --- |
| `Instant` | When the screen request or timer **started** (`screen`, `cyclic_job`), when the integration or mobile request **completed** (`integration`, `mobile_request`), or when the row was logged (`error`, `general`, `extension`, `request_event`), in the platform servers' time zone | Record timestamp; watermark. Start-stamped types need their own `lag` |
| `Application_Name`, `Espace_Name` | Owning application and module | `service.name` (application); `Espace_Name` attribute and metric label |
| `Module_Name` (error and general logs) | The free text the row was logged with; `SLOWSQL` marks the platform's slow-query messages | Attribute; selects slow-SQL rows |
| `Request_Key` | One request handled by a front-end server; shared by the rows that request logs, across log types and modules | Request correlation key |
| `Session_Id`, `User_Id`, `Username` | Platform session and user (`Login_Id` on mobile requests is returned as `Session_Id`) | Investigation filters; distinct counts for active users |
| `Client_IP` | Caller address (`Source` on mobile requests and exposed integrations is returned as `Client_IP`) | Investigation filter |
| `Tenant_Id` | Multi-tenant apps | Attribute; optional business breakdown |
| `Server` | Front-end server that handled the request (`Executed_By` on most views is returned as `Server`) | `front_end` metric label |
| `Duration` | Elapsed time of screens, server calls, integrations, extensions, timers and slow queries, in milliseconds (the queries convert the log types the platform logs in seconds and parse the `SLOWSQL` message) | Latency metrics; span duration |
| `Screen`, `Action_Name` | The entry point and the operation that ran; the queries map each view's own column (`Entrypoint_Name`, `Endpoint`, `Action`, `Cyclic_Job_Name`, the event name) to these two | `operation` metric label (capped); span name |
| `Error_Id` | Links a row to its error log entry | Outcome `error` |
| `Message`, `Stack` | Error text and stack | Body; `error.fingerprint` (from the whole values); cut at 16 KiB |
| `Session_Bytes`, `Viewstate_Bytes` | Traditional web screen state sizes | Size histograms |
| `Endpoint`, `Type`, `Is_Expose` | Integration URL, protocol and direction | Attributes; dependency breakdown |
| `Should_Have_Run_At` | Timer schedule | Timer delay |

The mapping from each view's own columns to these names is the
[standard columns](../../services/db-collector/internal/probes/outsystems/queries/README.md#standard-columns)
table of the queries README.

Identifiers are never Loki labels or metric labels. They stay structured
metadata, queryable with `| User_Id="…"`. They are kept raw, with access to
Loki and Grafana restricted, and production needs compliance sign-off first
([ADR 0012](decisions/0012-personal-data-in-telemetry.md)); the extractor can
hash them before export without changing anything else.

## Derived metrics

The collector computes these from every row it delivers to the OTel
Collector and exposes them on its `/metrics`
([code](../../services/db-collector/internal/collectors/outsystems/derived.go)).
They are counted when the row is extracted, so they trail real time by the
lag plus the poll interval (about a minute by default), and a batch delivered
twice (a crash between delivery and the watermark save) is counted twice.

Every metric has the labels `collector`, `environment` and `application`
(the record's `service.name`), plus those shown. Labels are capped so a name
the platform invents cannot grow the series without bound: the first 100
values of `module` per application and of `operation` per application and log
type keep their own series, later ones are counted as `other`; `front_end`
is capped at 50 and integration `type` at 20 per collector, `application` at
1,000. The kept values are the first seen since the collector started, so a
restart can keep different ones. The caps multiply, so each collector also
holds at most `max_derived_series` series of these metrics; past it a row
that needs a new series is left out of that metric and counted, and
`OutSystemsDerivedSeriesOverflow` fires
([series budget](../reference/metrics-and-endpoints.md#derived-outsystems-metrics)).
User, session, request and IP never become labels.

Entry points are the rows that stand for one request or timer run: `screen`
(Traditional and Reactive screens; `operation` is the screen),
`mobile_request` (Reactive server calls; the endpoint), `cyclic_job` (the
timer) and `integration` rows of exposed APIs (the API action).

### SRE: requests, latency and errors

| Metric | Type | Labels | What it answers |
| --- | --- | --- | --- |
| `atrium_outsystems_operation_duration_seconds` | histogram | `module`, `log_type`, `outcome` | Latency percentiles and SLOs per module for entry points |
| `atrium_outsystems_operations_total` | counter | `module`, `log_type`, `operation`, `outcome` | Throughput and error rate per screen, server call, exposed API or timer |
| `atrium_outsystems_operation_seconds_total` | counter | `module`, `log_type`, `operation` | Mean latency per operation (with the counter above); finds the slow screen inside a slow module |
| `atrium_outsystems_integration_duration_seconds` | histogram | `direction` (consume/expose), `type` (REST/SOAP), `outcome` | External dependency latency; a slow partner shows here first |
| `atrium_outsystems_errors_total` | counter | `module` | Error rate by module, from the error log |
| `atrium_outsystems_slow_events_total` | counter | `module`, `kind` (sql/extension) | Slow SQL (`SLOWSQL`) and slow extension (`SLOWEXTENSION`) rows the platform writes to the general log |
| `atrium_outsystems_timer_delay_seconds` | histogram | `operation` | How late timers start against `Should_Have_Run_At`: scheduler saturation |
| `atrium_outsystems_screen_state_bytes` | histogram | `module`, `kind` (session/viewstate) | Traditional web pages whose state size makes them slow or loads front-end memory |

`outcome` is `error` when the row has an `Error_Id` or a severity of `ERROR` or
above, otherwise `success`; both series exist from an entry point's first
row, so an error ratio reads 0 rather than nothing. Rows without a usable
`Duration` (NULL or negative) are counted but not timed. Latency buckets
include the SLO thresholds (0.1, 0.25, 0.5, 1, 2, 3, 5, 10, 30, 60 s).

### Infrastructure

| Metric or source | What it answers |
| --- | --- |
| `atrium_outsystems_front_end_operations_total{front_end, outcome}` | Load balance and error skew per front-end server: one bad node |
| `atrium_outsystems_late_rows_total{log_type}`: rows the recount found committed after the extraction had read past their `Instant` | Whether the platform's log service falls behind by more than the lag; those rows are not in Loki and the metrics undercount ([ADR 0008](decisions/0008-outsystems-11-log-extraction.md)) |
| `atrium_outsystems_log_delay_seconds{log_type}`: now minus the newest `Instant` read, while rows arrive | How fresh extraction is: about the lag plus the poll interval. It cannot show the log service falling behind, since a row is read only once it is older than the lag; the late-row counter does |
| `atrium_outsystems_extract_lag_seconds`, `atrium_outsystems_extract_rows_total`, `atrium_outsystems_export_failures_total`, `atrium_outsystems_duplicate_rows_total`, `atrium_outsystems_recount_failures_total`, plus the collector's probe duration, target and freshness metrics | Health of Atrium's own extraction |
| The `sqlserver` collector kind on the logging and platform databases | Waits, blocking, storage growth and I/O of the databases OutSystems depends on; the log database's growth and the platform database's blocking are common OutSystems incidents |
| Front-end Windows/IIS hosts | Not collected: it needs an agent on the platform servers (for example `windows_exporter`). Optional, the operator's decision |

### Business

| Source | What it answers |
| --- | --- |
| Distinct `User_Id` and `Session_Id` per application over 5m and 1h, from Loki | How many people use each application now. Daily and monthly actives come from Loki in reports (phase 7) |
| `atrium_outsystems_operations_total` by `operation` | Feature and screen usage: what people actually use |
| `atrium_outsystems_integration_duration_seconds_count` by `direction` and `type`; per endpoint from Loki | Business transaction volume with partner systems |
| `Tenant_Id` (Loki, reports) | Usage per tenant, where apps are multi-tenant; not a metric label unless the tenant count is small |

### Not built, on purpose

- **Active users and sessions as gauges.** A distinct count needs every
  identifier of the window in the collector's memory; it resets on a restart
  and would double when collectors run as replicas (phase 8). Loki counts
  them correctly from the structured metadata already sent, so dashboards
  query Loki instead. A LogQL distinct count holds one series per ID, so
  the chart raises Loki's `max_query_series` from 500 to 20000.
- **New error fingerprints.** "Never seen before" needs a persisted,
  bounded set of fingerprints, or every restart reports all errors as new and
  looks like a bad deployment. It waits until a dashboard or alert needs it;
  until then `error.fingerprint` is on every error record in Loki.

### SLOs and alerts

Recording rules in
[`atrium.rules.yml`](../../infra/helm/atrium/files/prometheus/rules/atrium.rules.yml)
turn the metrics into SLIs per environment and application, over
user-facing requests (screens, Reactive server calls and exposed APIs;
timers are left out):

- Availability: the share of requests with `outcome="success"`
  (`atrium:outsystems_request_errors:ratio_rate<window>`).
- Latency: the share of requests answered within 2 s, from the histogram's
  `le="2"` bucket (`atrium:outsystems_request_slow:ratio_rate<window>`).

Alerts use multiwindow burn rates against static default objectives, 99.9%
available (a 0.1% error budget) and 99% within 2 s over 30 days: fast burn
(14.4× over 1h and 5m, an error ratio above 1.44%) is `critical`, slow burn
(6× over 6h and 30m, above 0.6%) is `warning`. Low-traffic applications are
left out so a handful of failures cannot page: the availability alerts need
five requests a minute over the long window, so a fast burn takes at least 5
failed requests in the hour; the latency alerts need one. Because the
SLIs go quiet, not red, when extraction stops, `OutSystemsLogExtractionBehind`
fires when a log type's extraction lag has been above 30 minutes for 10, and
`OutSystemsLogExportBlocked` sooner, 2 minutes after the OTel Collector stops
taking batches, as it does while Loki is down. These are
guardrails; per-application objectives and adaptive baselines come in
phase 7.

### Dashboards

Four dashboards in Grafana's **Atrium** folder read these metrics and the
log records
([`files/grafana/dashboards/`](../../infra/helm/atrium/files/grafana/dashboards/)).
Each one filters by environment and application. A header link opens the
others with the same filters and time range, and another opens the SQL Server
Overview for the same time.

| Dashboard | What it answers | From |
| --- | --- | --- |
| *OutSystems Overview* (`outsystems-overview`) | Are users served well right now: requests, failed and slow shares against the objectives, p95, error-budget burn, active users and sessions, extraction lag; then the busiest operations, front-end skew, integrations, slow SQL, timers and Atrium's own extraction | Metrics; users and sessions from Loki |
| *OutSystems Errors* (`outsystems-errors`) | What fails, where and for whom: errors by module and front-end, failed operations, the top fingerprints, users affected, and the error log | Metrics and Loki |
| *OutSystems Latency* (`outsystems-latency`) | Where time goes: percentiles and the duration distribution, slowest operations by mean, p95 by module, integrations, screen state size, and the requests over 2 s | Metrics and Loki |
| *OutSystems Request Timeline* (`outsystems-request`) | Everything one request logged, by `Request_Key`; the requests of a user or session; how many rows of each log type carry a `Request_Key` ([step A](#step-a-request-timeline-from-logs-with-phase-2)) | Loki |

Drill-down goes from the overview's top operations to a module's latency
or errors, then from any `Request_Key` in a table to the request timeline.
A fingerprint in the top errors filters the error log to that error. The
metric panels trail real time by the extraction delay (about a minute); the
Loki panels show rows as soon as they are in Loki. A Go test checks every
dashboard's datasources, variables and links
([`tests/dashboards_test.go`](../../tests/dashboards_test.go)); the
metric-reference test covers the metric names.

## Request traces

### The gap

APMs such as Dynatrace instrument the .NET processes behind OutSystems: they
see IIS requests, ASP.NET handlers and SQL statements, but not which
OutSystems application, module, screen or action they belong to, and
OutSystems 11 logs carry no trace ID to join them by. The result is traces
that are hard to map to the platform's own concepts.

OutSystems already ties its own logs together: every row a request writes
carries the same `Request_Key`, across log types and modules. Atrium can
build request timelines from that key, in OutSystems terms, without any agent.

### Step A: request timeline from logs (with phase 2)

- `Request_Key` is kept as structured metadata on every record.
- A Grafana dashboard shows one request across log types: entry point
  (screen, server call, timer or exposed API) with its duration, the
  integrations and extensions it called, slow SQL, and errors, ordered by
  time.
- Investigations filter by user, session or IP, then open the requests they
  find.

This needs no new storage and proves whether `Request_Key` is reliably
present in each log type. The *OutSystems Request Timeline* dashboard does
both ([dashboards](#dashboards)); its *Rows carrying a Request_Key* panel is
the measurement, which needs a real installation's logs to mean anything
(PatientZero writes the key on every row).
[`scripts/outsystems-log-queries.sql`](../../scripts/outsystems-log-queries.sql)
holds the SQL the SRE team runs against the logging database for the same
timelines.

### Step B: synthesized OTLP traces (after step A)

The extractor turns rows into spans as well as log records:

| Span | From |
| --- | --- |
| Trace ID | A hash of `Request_Key`, so the logs and the trace link both ways without storing a mapping |
| Root span | The entry-point row: screen, server call, timer or exposed API, with its duration |
| Child spans | Integration, extension and slow-SQL rows with the same `Request_Key`, timed from `Instant` and `Duration` (`Instant` is the end for integrations and mobile requests, the start for screens and timers) |
| Span events | Error rows |
| Attributes | Application, module, operation, front-end, outcome; identifiers as on the logs |
| Span links | Calls from one OutSystems app to another one's exposed API, matched by endpoint and time; marked as inferred |

The OTel Collector's `spanmetrics` connector can then produce the same RED
metrics from spans, and Tempo, the store Atrium's own traces already use
([ADR 0018](decisions/0018-atriums-own-telemetry.md)), shows waterfalls with
links to the logs and the SQL Server dashboards for the same time. Step B is not
part of phase 2's exit.

Limits, stated in the product: spans exist only where OutSystems logs
something. Fast SQL, internal actions and client-side time do not appear, and
timings are as precise as the platform's millisecond durations. That covers
what SREs triage first (which request, which module, which dependency) without
replacing a code-level profiler.

Not recommended now: OTel .NET auto-instrumentation on the front-end servers.
It needs an agent on the platform servers and produces the same unlabelled
.NET spans an APM does.

## Open questions

Answered from the OutSystems 11 documentation ([Log data reference](https://success.outsystems.com/Documentation/11/Managing_the_Applications_Lifecycle/Monitor_and_Troubleshoot/Logging_database_and_architecture/Log_data_reference),
[The log tables and views](https://success.outsystems.com/Documentation/11/Managing_the_Applications_Lifecycle/Monitor_and_Troubleshoot/Logging_database_and_architecture/The_log_tables_and_views)):

- **Is `Instant` the start or the end of the logged operation?** Per log
  type: the start for `screen` and `cyclic_job`, the completion for
  `integration` and `mobile_request`, the time of logging for `error`,
  `general` and `extension`, the time of the event for `request_event`. The
  start-stamped rows are written when the operation ends, hence the per-log-
  type `lag` ([ADR 0008](decisions/0008-outsystems-11-log-extraction.md)).
- **Which time zone?** The platform servers' own. OutSystems Cloud runs UTC;
  a self-managed installation uses whatever its front-ends and database
  share. `log_timezone` is set per collector.
- **Which log types carry `Request_Key`?** All eight current ones: general,
  error (`Request_key`), screen, integration, extension, mobile request,
  cyclic job and request event (`REQUESTKEY`). Only the deprecated web
  reference, web service and SMS logs and the detail tables lack it.
- **Retention.** Ten weekly cycle tables per log type; the `oslog_<TYPE>`
  view is the current week, `oslog_<TYPE>_Previous` the week before, switched
  every Friday at 23:45 database time. The retention period is set in the
  Configuration Tool (Log tab, Advanced settings); OutSystems Cloud keeps 9
  weeks, 7 with log separation. `initial_lookback` can reach back one week at
  most, since the queries read two weeks' views.

[`scripts/outsystems-log-survey.sql`](../../scripts/outsystems-log-survey.sql)
(read-only) ran against one installation's logging database on 2026-10-19.
Its native tables were empty and its data a flattened one-week extract, so
it answered some questions and left others open:

- **Schema:** answered. The test fixtures are its DDL: `varchar` text,
  `datetime2` instants, `Error_Id` as text in every log (empty for none), the
  error log's `EnvironmentInformation`, and cycle tables clustered on `Id`
  or not clustered at all, so rows that share an `Instant` come back in no
  particular order. The `oslog_Error` view also joins `oslog_Error_Detail`
  for full stack traces.
- **Rows per minute at peak:** about 3,800 across screen, integration,
  extension and slow-SQL rows at the busiest minute of the week, about 1,000
  on average; no single log type passed 2,200. The default `batch_limit` of
  5000 covers it.
- **`Duration` unit:** milliseconds for screen, integration and extension
  (the reference says seconds for extension); maxima sit at timeouts in
  milliseconds. Still open for mobile request and cyclic job: the survey had
  none of their rows.
- **Still open:** whether every request writes an entry-point row (screen or
  mobile request), and the installation's time zone (from an `Instant` next
  to the database's `SYSDATETIMEOFFSET()`; a stale copy cannot tell). The
  hourly curve peaks at 11:00 and bottoms at 04:00, which reads as local
  time.
