# Configuration Reference

Heartbeat services are configured by one integrations file plus environment
variables. Why config lives in YAML rather than PostgreSQL is explained in
[ADR 0002](../architecture/decisions/0002-runtime-config-in-yaml.md).

Sources of truth:

- Loader and validation: [`internal/config/config.go`](../../internal/config/config.go)
- JSON schema: [`packages/config-schema/src/integrations.schema.json`](../../packages/config-schema/src/integrations.schema.json)
- Deployed config: the `integrations` key of the Helm chart's values
  ([defaults](../../infra/helm/heartbeat/values.yaml), no SQL Server targets),
  overridden per environment by files in [`infra/helm/values/`](../../infra/helm/values/)
- Binary default for running a service outside Kubernetes:
  [`config/integrations.yaml`](../../config/integrations.yaml)

## Config files by environment

The chart renders `integrations` into the `heartbeat-integrations` ConfigMap,
mounted at `/app/config/integrations.yaml`. Helm merges maps across values
files but **replaces lists**, so an environment that changes `collectors`
restates the whole list.

| Environment | Where `integrations` comes from |
| --- | --- |
| kind | Chart defaults (`values.yaml`) plus [`kind.yaml`](../../infra/helm/values/kind.yaml) |
| kind with the SQL Server sandbox | Adds [`sqlserver-dev.yaml`](../../infra/helm/values/sqlserver-dev.yaml) |
| Production | Values in the `heartbeat-deploy` config repository; [`production.example.yaml`](../../infra/helm/values/production.example.yaml) shows the shape |
| Service run directly (`go run`) | `config/integrations.yaml`, or `HEARTBEAT_INTEGRATIONS_PATH` |

Helm validates `integrations` against the integrations schema (embedded in the
chart's `values.schema.json`) before anything is applied, and `make chart-check`
loads each profile's rendered file with the Go loader.

### Collectors rendered by the API

Approved probe assignments reach the collector as collectors of their own,
which the API renders into a generated values file it owns
([ADR 0014](../architecture/decisions/0014-probe-assignments-to-runtime-config.md)).
That file sets the chart key `managedCollectors`, not `integrations`, and the
chart appends that list to `integrations.collectors` when it renders
`integrations.yaml`. Because lists replace, the separate key keeps the two
apart: the environment's values hold the hand-written collectors, the API's
file holds its own, and neither overwrites the other. The rendered file has
the same format either way; the collector loads it like any other and does
not know which collectors came from the API.

The API side, which renders the file from approved assignments, is not built
yet, so `managedCollectors` is empty. Empty or absent, the chart renders
exactly what `integrations` alone renders. Managed targets count like
hand-written ones: the collector's egress NetworkPolicy opens their ports,
and the install notes count them.

Helm checks these rules whenever it renders the chart (`helm template`,
`install` and `upgrade`, and Argo CD's render), before anything is applied:

| Rule | Checked by | Helm's error |
| --- | --- | --- |
| Each `managedCollectors` entry is a [collector](#collectors) of kind `sqlserver`; `outsystems_logs` collectors stay hand-written ([ADR 0017](../architecture/decisions/0017-outsystems-sources-map-application-identity.md)) | values schema | ``at '/managedCollectors/0/kind': value must be 'sqlserver'`` |
| A `managedCollectors` id starts with `api-`; the API writes `api-<target id>-<interval>s` | values schema | ``at '/managedCollectors/0/id': 'finance' does not match pattern '^api-'`` |
| A hand-written id, in `integrations.collectors`, does not start with `api-`, leading spaces included | values schema | ``at '/integrations/collectors/0/id': 'not' failed`` |
| An id appears once across both lists, compared without surrounding spaces, as the collector compares them | chart templates | ``collector id "sql" is listed twice, at integrations.collectors[0] and integrations.collectors[1]`` |

The last rule covers the hand-written list on its own too: a duplicate id
there used to render, and the collector then rejected the file at its next
reload, or failed to start.

## `integrations.yaml`

### Endpoints

Heartbeat never writes to the observability platform itself: logs go to the
OpenTelemetry Collector, and metrics are scraped from `/metrics`
([ADR 0011](../architecture/decisions/0011-platform-agnostic-telemetry-boundary.md)).
These keys say where the platform's UI and query APIs are, for links and,
later, the API's queries.

| Key | Required | Notes |
| --- | --- | --- |
| `platform` | no | `grafana` (default: Grafana, Prometheus, Loki, Alertmanager) or `generic` (any other platform; `dashboards`, `logs` and `alerting` must then be empty). Anything else is rejected |
| `dashboards.base_url` | no | The platform's UI (Grafana), as seen from the operator's browser |
| `dashboards.dashboard_templates` | no | Map of name → dashboard path, e.g. `sqlserver-overview: /d/sqlserver-overview` |
| `dashboards.deep_link_templates` | no | Map of name → path with `${variables}`, e.g. `/d/sqlserver-overview?var-target=${target}` |
| `logs.base_url` | no | The platform's log query API (Loki) |
| `alerting.base_url` | no | The platform's alert API (Alertmanager) |
| `opentelemetry.endpoint` | for `outsystems_logs` | Absolute base URL of the OTel Collector's OTLP/HTTP receiver, e.g. `http://otel-collector:4318`; logs go to `<endpoint>/v1/logs`. The chart's NetworkPolicy admits the collector to the bundled Collector's port 4318 only ([network access](../guides/kubernetes-local.md#network-access)) |

No URL may embed credentials (`https://user:password@host`): the loader rejects
userinfo in every `base_url`, `endpoint`, dashboard and deep-link template, and
channel `target_ref`, for all four sections. Put secrets in a
`credential_ref` instead; URLs end up in logs, diagnostics and browser links.
The check also catches templates with `${variables}` in the host and the
forms browsers accept (`http:\\user@host`, `http:user@host`, tabs or newlines
inside the URL). A `target_ref` is also rejected when it is a scheme-less
`user:password@host`, which tools like curl read as credentials; plain
addresses (`ops@example.com`) and `mailto:` are fine.

### Notification channels

```yaml
notification_channels:
  - id: default-webhook          # required, unique
    channel_type: webhook        # required
    target_ref: http://…         # required
    credential_ref: env/HEARTBEAT_WEBHOOK_TOKEN   # optional secret reference
    config:                      # optional string map
      timeout: 10s
```

### Credential references

`credential_refs` is an optional map of name → secret reference. Every
credential reference in the file (`credential_refs.*`, collector, target and
channel `credential_ref`) must start with `env/`, `kv/` or `secret/`. Values
are never stored in the file.

### Collectors

```yaml
collectors:
  - id: sqlserver-default        # required, unique; stable ID used for reload diffs
    kind: sqlserver              # required; sqlserver or outsystems_logs (below)
    enabled: true
    credential_ref: kv/sqlserver-default   # default for targets without their own
    config:
      environment: local         # default environment label for targets
      scrape_interval: 30s       # Go duration, > 0; default 30s
      target_names: []           # optional subset of targets to run; empty = all
      probes:                    # default probe set for targets without their own
        - name: waits
        - name: blocking
          timeout_ms: 5000       # optional per-probe timeout override
          max_rows: 1000         # optional; the most rows the probe may return, 1–100000; default 10000
        - name: storage
          query_template: "…"    # optional SQL override of the catalog query: one read-only SELECT
      targets:
        - name: finance-prod     # required, unique within the collector; becomes the `target` label
          environment: prod      # optional; defaults to config.environment
          host: finance-sql.internal   # required; name, IPv4 or bare IPv6, no port
          port: 1433             # required, 1–65535
          database_name: FinanceDB     # initial database
          credential_ref: env/finance-prod   # optional; defaults to the collector's
          probes: []             # optional; defaults to config.probes
```

In the chart's values, collector ids that start with `api-` are reserved for
the [collectors the API renders](#collectors-rendered-by-the-api).

`host` is a DNS name or IPv4 address (letters, digits, `.`, `-`, `_`) or a bare
IPv6 address such as `2001:db8::5`, without brackets. A port (`db:1433`),
credentials, a path, an instance name (`db\INST`) or whitespace is rejected:
the collector builds the connection URL from it, and an unparsable URL would
put the password into the driver's error.

Built-in probe names: `waits`, `blocking`, `sessions`, `memory_pressure`,
`storage`, `throughput`, `cpu`, `buffer_cache`, `file_io`. See the
[metrics reference](metrics-and-endpoints.md#sql-server-probe-metrics). Any
other name, in an enabled collector or not, is rejected when the file is
loaded or reloaded, and the error lists the valid ones. A `query_template`
replaces a built-in probe's query; it cannot add a probe.

Probe timeout defaults to `min(scrape_interval / 2, 10s)`; `timeout_ms` overrides
it and is capped at the interval. `timeout_ms` may not exceed 25000 (25s): a
larger value is rejected when the configuration is loaded or reloaded. A probe
that has not stopped 2s after its timeout is abandoned and fails as a timeout.
Every database read also gives up after 30s, the 25s maximum plus a margin for
the cancel round trip, so the driver never cuts off a probe that is still
within its timeout; only an already-dead connection reaches it.

A probe reads at most `max_rows` rows (default 10000, at most 100000). When
its query returns more, the collector stops reading at the first row past the
limit, cancels the query and fails the probe for that cycle, as for a query
error: the error is logged (`returned more rows than max_rows`), the target is
reported failed, and `heartbeat_collector_probe_errors_total{reason="error"}`
grows ([metrics reference](metrics-and-endpoints.md#collector-self-observability)).
The built-in probes return at most a few hundred rows; the limit stops an
override, or a server with unusually many databases or files, from filling
the collector's memory and the metric series. `max_rows` applies to `sqlserver`
collectors only and is rejected on other kinds.

A `query_template` override runs with the same login and the same session
settings. It must be exactly one read-only `SELECT`, optionally led by `WITH`
(CTEs); the configuration is rejected when it is loaded or reloaded
otherwise, with an error that names the collector, target and probe and the
reason, without quoting the query. Rejected:

- more than one statement: anything after a `;` other than comments or more
  `;`, or a second top-level `SELECT` that does not follow `UNION`, `EXCEPT`
  or `INTERSECT` (T-SQL needs no `;` between statements);
- a first word other than `SELECT` or `WITH`, so a bare procedure name too;
- DML and DDL (`INSERT`, `UPDATE`, `DELETE`, `MERGE`, `TRUNCATE`, `CREATE`,
  `ALTER`, `DROP`, `ADD`), permissions (`GRANT`, `DENY`, `REVOKE`), `EXEC` and
  `EXECUTE`, `SELECT … INTO`, `OPENROWSET`, `OPENQUERY`, `OPENDATASOURCE`,
  `WAITFOR`, `SHUTDOWN`, `DBCC`, `BACKUP`, `RESTORE`, `USE`, and the other
  T-SQL statement keywords: `SET`, `DECLARE`, transactions (`BEGIN`,
  `COMMIT`, `ROLLBACK`, `SAVE`), `KILL`, `RECONFIGURE` and the like (the full
  list is in
  [`query.go`](../../internal/config/query.go)). The query hints
  `MERGE JOIN`, `MERGE UNION`, `USE HINT (…)` and `USE PLAN` are allowed;
- a name starting with `sp_` or `xp_` (system and extended procedures), and an
  `END` without a matching `CASE`;
- unbalanced parentheses, an unterminated string, comment or quoted
  identifier, or a control or Unicode line-separator character other than
  tab, CR and LF.

Keywords inside string literals (`'…'`, `N'…'`), comments (`--`, nested
`/* */`) and quoted identifiers (`[…]`, `"…"`) are ignored, so a column named
`[Backup]` is fine, while `t.Backup` is rejected (a reserved word must be
quoted there too). A statement after a comment, or glued to a number as in
`SELECT 1DROP …`, `SELECT 1.DROP …` or `SELECT 1eDROP …`, is still caught.
The built-in probe and log-type queries pass the same check. It is a safety
net, not the
control: a text check cannot prove a query has no side effects. What keeps
probes read-only is the collector's login, which holds only read grants
([login permissions](../guides/database-targets.md#collector-login-permissions)).
Review every override against the
[probe review checklist](../architecture/database-observability.md#probe-review-checklist)
and with the DBAs.

#### `outsystems_logs` collectors

An `outsystems_logs` collector extracts OutSystems 11 logs from the logging
database and sends them to the OTel Collector as OTLP logs
([ADR 0008](../architecture/decisions/0008-outsystems-11-log-extraction.md)).
It uses the same document as `sqlserver`: `environment`, `credential_ref`,
`target_names`, `targets` (the logging databases) and `probes`. Each probe is
a log type, named after its query file in
[`queries/`](../../services/db-collector/internal/probes/outsystems/queries/README.md)
(`error.sql` is `error`). `query_template` replaces that query for one
collector, under the same contract, and must be one read-only `SELECT` as for
SQL Server probes; `timeout_ms` works as for SQL Server probes. `max_rows`
does not apply: `batch_limit` bounds every read. `scrape_interval` is the poll
interval.

```yaml
collectors:
  - id: outsystems-prod
    kind: outsystems_logs
    enabled: true
    credential_ref: env/oslog-prod
    config:
      environment: prod
      scrape_interval: 30s     # poll interval; default 30s
      lag: 60s                 # read up to now minus lag; default 60s
      batch_limit: 5000        # rows per query, 1–50000; default 5000
      initial_lookback: 15m    # where a log type without a watermark starts; default 15m
      log_timezone: UTC        # IANA time zone Instant is stored in; default UTC
      recount_window: 10m      # recount behind the watermark; default 10m, 0s off
      max_derived_series: 50000 # derived-metric series this collector may hold, 1–1000000; default 50000
      probes:
        - name: error
        - name: cyclic_job
          lag: 25m             # this log type's lag: Instant is the timer's start
      targets:
        - name: oslogging-prod
          host: oslog-sql.internal
          port: 1433
          database_name: OSLogging
```

| Setting | Default | Notes |
| --- | --- | --- |
| `lag` | `60s` | Go duration, > 0. Rows the log service commits later than this after their `Instant` are missed |
| `probes[].lag` | the collector's `lag` | Go duration, > 0. Replaces `lag` for one log type. A `screen` or `cyclic_job` row has the operation's start as `Instant` and is written when it ends, so set it above the longest screen request or timer (the timer timeout, 20 minutes by default) |
| `batch_limit` | `5000` | 1–50000. A full batch is followed at once by the next, up to 10 per cycle. If every row of a full batch has the same `Instant`, the cycle fails and `heartbeat_outsystems_extract_stuck_batches_total` grows: raise `batch_limit`. A batch also ends early at about 8 MiB of row data (below) |
| `initial_lookback` | `15m` | Go duration, > 0. Also used when the stored watermark is unreadable or belongs to another database |
| `log_timezone` | `UTC` | IANA name such as `Europe/Lisbon`; `Local` is rejected. With a zone that has daylight saving time, rows logged in the hour that repeats when clocks go back can be missed: `Instant` itself is ambiguous then |
| `recount_window` | `10m` | `0s` (off), or whole minutes from `2m` to `1h`. Once a minute, each log type counts again the rows in this window behind its watermark, to find rows committed past the `lag` (`heartbeat_outsystems_late_rows_total`) or read twice ([recount](metrics-and-endpoints.md#outsystems-log-extraction)). A wider window finds later commits and counts more index rows each minute |
| `max_derived_series` | `50000` | 1–1000000. The most series the [derived metrics](metrics-and-endpoints.md#derived-outsystems-metrics) hold for this collector, counted as Prometheus stores them: one per counter, and a histogram's buckets, `+Inf`, sum and count (13 for the latency and timer histograms, 11 for screen state). Past it, a row that needs a new series is not counted in that metric, `heartbeat_outsystems_derived_series_overflow_total` grows and `OutSystemsDerivedSeriesOverflow` fires; series that exist keep counting. Each series takes about 690 bytes of the collector's memory, so 50000 is about 33 MiB. The default is a proposal, not a measured need: 29 times the 1,716 series of PatientZero's [production-sized profile](../../tools/patientzero/README.md#production-sized-logging-database) (6 applications, 14 modules, 8 screens each, 20 timers, 4 front-ends); size it from `heartbeat_outsystems_derived_series` on the real installation. Changing it restarts the collector, which deletes its derived series and starts counting again |

Rules:

- `opentelemetry.endpoint` is required while the collector is enabled.
  Changing it restarts every `outsystems_logs` collector.
- A log type with no query file is rejected when the file is loaded or
  reloaded, and so is a log type listed twice for one target (each has one
  watermark).
- A log type of one logging database (same host, port and database name,
  compared case-insensitively) can be extracted by one enabled collector
  only; two would send every row twice.
- `lag`, `batch_limit`, `initial_lookback`, `log_timezone`, `recount_window`,
  `max_derived_series` and `probes[].lag` are rejected on other kinds, and
  `probes[].max_rows` on this one.
- With the recount on, a `query_template` must be one a derived table holds:
  no `WITH`, top-level `OPTION`, or `FOR XML`, `JSON` or `BROWSE`, an
  `ORDER BY` only with `TOP (@limit)` or `OFFSET`, and every column named.
  All but unnamed columns are rejected when the file is loaded; otherwise
  set `recount_window: 0s`.
- A query reads at most `batch_limit` rows: the collector stops at the first
  row past it and fails the cycle without sending anything, so a
  `query_template` that ignores `@limit` cannot fill the collector's memory.
- A batch stops reading once it holds about 8 MiB of row data, at the next
  row with a new `Instant`, and is then handled as a full batch: rows that
  share an `Instant` are never split. This bounds the collector's memory when
  a backlog of error rows with long stacks builds up, which `batch_limit`
  alone does not; a smaller batch is followed at once by the next.
- The body and every text attribute of a record are cut at 16 KiB, on a
  character boundary and ending in `…[truncated]`, and the record gets
  `outsystems.truncated = true`. `Stack` and `EnvironmentInformation` are
  the usual ones; Loki keeps attributes as structured metadata, 64 KB per
  line by default. `error.fingerprint` is computed from the whole `Message`
  and `Stack`, so it does not change.
- The watermark of each collector, target and log type is a file under
  `HEARTBEAT_STATE_DIR` (`outsystems_logs/<collector>.<target>.<log type>.json`).
  It moves only after the OTel Collector accepted the batch, so delivery is at
  least once. The chart's Collector keeps the batch it accepted on disk (the
  `otelCollectorQueue` volume), so a Collector restart does not lose it, and
  accepts the next one only once Loki has taken it
  ([application observability](../architecture/application-observability.md)).
- A batch the Collector has not accepted by the end of the probe execution
  is not a failure: its export carries on in the background, retried until
  the Collector accepts it, and the log type reads nothing more meanwhile.
  `heartbeat_outsystems_export_blocked` is 1 and the target stays up
  ([metrics](metrics-and-endpoints.md#outsystems-log-extraction)). At most
  64 MiB of row data is held in such batches across all log types; near
  that, a batch reads only what is left, and with less than 1 MiB left the
  other log types wait too. In each cycle the calls of one target wait for
  the Collector at most half the poll interval in all, so its other log
  types still run. The error in `GET /admin/config` names the last failed
  attempt, and an attempt that failed for another reason than waiting (a
  wrong `opentelemetry.endpoint`, a 5xx) is also logged as a warning.
  Removing a collector leaves its files; a collector re-added with the same
  `id` resumes from them.
- The login needs `SELECT` on the `oslog_*` views the queries read (the
  current and the previous week's) and nothing else; see
  [database targets](../guides/database-targets.md#outsystems-logging-database).

### Validation and reload behavior

- The file is validated on startup and on every reload. Values that fail the
  schema never render; a ConfigMap edited by hand can still be invalid, and is
  then rejected as below.
- An invalid candidate is rejected and the previous config keeps running.
  `POST /admin/config/reload` returns 400 for invalid config and 500 for a
  failed apply. At startup there is no previous config: the collector exits,
  so a file left invalid stops the next pod from starting.
- A rejected reload, or a failed one that was rolled back, does not make the
  pod unready. Monitoring shows it:
  `heartbeat_collector_config_last_reload_successful` is 0 until the next
  successful reload, and `HeartbeatCollectorConfigReloadFailed` fires after 2
  minutes ([config reload metrics](metrics-and-endpoints.md#config-reloads)).
- Collectors are diffed by `id`; only changed, added or removed collectors are
  restarted. A failed apply is rolled back. If the rollback also fails,
  `runtime_diverged` is reported and `/readyz` returns 503.
- Reload triggers: `SIGHUP`, authenticated `POST /admin/config/reload`, and file
  polling when `HEARTBEAT_CONFIG_WATCH_INTERVAL` is set.
- `GET /admin/config` shows the active config redacted: credential values and
  every notification channel `config` value are masked, and userinfo is
  stripped from URLs.

## Credential resolution

The DB collector looks up a `credential_ref`, and the API its PostgreSQL and
Redis credential references, under one name:

```text
HEARTBEAT_CREDENTIAL_<REF>
```

`<REF>` is the full reference upper-cased, with `/`, `-` and `.` replaced by
`_`. The value must be `username:password`; the password is everything after
the first `:`.

| `credential_ref` | Name |
| --- | --- |
| `env/sqlserver-b` | `HEARTBEAT_CREDENTIAL_ENV_SQLSERVER_B` |
| `kv/sqlserver-default` | `HEARTBEAT_CREDENTIAL_KV_SQLSERVER_DEFAULT` |

It is looked up, in order:

1. A file of that name in `HEARTBEAT_CREDENTIALS_DIR`, when that is set. A
   trailing line break is ignored.
2. An environment variable of that name.

Both read the file every time they open a connection, never cache it, so a
new or changed credential needs no restart. A file that exists but cannot be
read, or holds no `:`, fails that connection (a collector target, or the
API's readiness); it never falls back to the environment variable. The
lookup is [`internal/credentials`](../../internal/credentials/credentials.go).

In Kubernetes the chart mounts the Secret named by
`dbCollector.credentials.existingSecret` as that directory
(`/var/run/secrets/heartbeat/credentials`), one file per Secret key. After
you add or change a key, the kubelet updates the files within about a minute,
and the next collection cycle uses them; the collector is not restarted. A
target whose credential is missing fails on its own, and the others keep
running. The Secret is not loaded into the environment, so a
`HEARTBEAT_CREDENTIAL_*` variable reaches the container only through
`dbCollector.extraEnv`.

The chart mounts the API's Secret, `api.credentials.existingSecret`, the
same way, in the API's container and in its `migrate` init container; its
keys are `HEARTBEAT_CREDENTIAL_SECRET_API_POSTGRES` and
`HEARTBEAT_CREDENTIAL_SECRET_API_REDIS` ([API](#api)).

Outside Kubernetes (`go run`, tests), leave `HEARTBEAT_CREDENTIALS_DIR` unset
and export the variables; exporting them in your host shell does not pass them
into a pod.

## Environment variables

### DB collector

| Variable | Default | Purpose |
| --- | --- | --- |
| `HEARTBEAT_DB_COLLECTOR_LISTEN_ADDR` | `:8082` | HTTP listen address |
| `HEARTBEAT_INTEGRATIONS_PATH` | `config/integrations.yaml` | Integrations file path |
| `HEARTBEAT_ADMIN_TOKEN` | unset | Bearer token for `GET /admin/config` and `POST /admin/config/reload`; both answer 401 without it, and startup logs a warning. Surrounding whitespace is ignored |
| `HEARTBEAT_CONFIG_WATCH_INTERVAL` | unset (off) | Go duration; poll the config file for changes (the chart sets `5s`, `dbCollector.configWatchInterval`) |
| `HEARTBEAT_STATE_DIR` | `/var/lib/heartbeat` | Writable directory for state that must survive a restart: the `outsystems_logs` watermarks. Created on the first save; the `sqlserver` kind never uses it. The chart mounts the `dbCollector.persistence` volume there |
| `HEARTBEAT_DB_COLLECTOR_SQLSERVER_TRUST_SERVER_CERTIFICATE` | `false` | Skip TLS certificate verification. **Local dev container only**; applies to every target. When set, startup logs a warning and `GET /admin/config` lists it under `warnings` |
| `HEARTBEAT_CREDENTIALS_DIR` | unset | Directory with one `HEARTBEAT_CREDENTIAL_<REF>` file per credential, read on every connection; references without a file come from the environment. The chart sets it to the mounted `dbCollector.credentials.existingSecret`. See [credential resolution](#credential-resolution) |
| `HEARTBEAT_CREDENTIAL_<REF>` | — | Target credentials, see [credential resolution](#credential-resolution) |
| `GOMEMLIMIT` | unset | Go's soft memory limit, such as `200MiB`. The chart sets 80% of `dbCollector.resources.limits.memory` (`204MiB` for the default `256Mi`), so the garbage collector works harder before the pod is OOM-killed; `dbCollector.goMemLimit` overrides it, `off` disables it |
| `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` | unset (stderr only) | Where the service sends its own logs, besides stderr: the OTel Collector's `otlp/self` receiver, such as `http://otel-collector:4319/v1/logs`. The chart sets it while `telemetry.enabled` and `otelCollector.enabled` are on. Not read from `OTEL_EXPORTER_OTLP_ENDPOINT`, whose port is the OutSystems delivery path ([ADR 0018](../architecture/decisions/0018-heartbeats-own-telemetry.md)) |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | unset (no spans) | Where the service sends its spans, such as `http://otel-collector:4318/v1/traces`. The chart sets it while `telemetry.traces.enabled` and `otelCollector.enabled` are on. The OpenTelemetry SDK reads the other standard variables itself |
| `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG` | always on | The chart sets `parentbased_traceidratio` and `telemetry.traces.sampleRatio`: every cycle and request by default |
| `OTEL_RESOURCE_ATTRIBUTES` | unset | `deployment.environment.name` is read from it: the environment Heartbeat itself runs in. The chart sets it from `telemetry.environment` |
| `OTEL_SDK_DISABLED` | unset | `true` turns off the OTLP export; stderr is unaffected |
| `HEARTBEAT_PPROF_ADDR` | unset (off) | Loopback address for the runtime profiles at `/debug/pprof/`, such as `127.0.0.1:6060`; any other address is refused with an error in the log. Set it with `dbCollector.extraEnv`, then `kubectl port-forward pod/db-collector-0 6060` |

### API

The chart sets these from the `api` values: `api.postgres` and `api.redis`,
or, when those are empty, the bundled `postgresql` and `valkey` without TLS
(`disable` and `false`); any other server gets `verify-full` and TLS unless
the values turn them off. `HEARTBEAT_API_SITE_ORIGIN` comes from
`api.siteOrigin`; the other session settings take their defaults, or
`api.extraEnv`. The telemetry variables are set as the collector's. A value
that cannot work stops startup with an error naming the variable, instead of
falling back to the default.

| Variable | Default | Purpose |
| --- | --- | --- |
| `HEARTBEAT_API_LISTEN_ADDR` | `:8081` | HTTP listen address, `host:port` |
| `HEARTBEAT_API_POSTGRES_HOST` | required | PostgreSQL server: a host name or IP address, without a port |
| `HEARTBEAT_API_POSTGRES_PORT` | `5432` | PostgreSQL port |
| `HEARTBEAT_API_POSTGRES_DATABASE` | `heartbeat` | Database name |
| `HEARTBEAT_API_POSTGRES_SSLMODE` | `verify-full` | `disable`, `require`, `verify-ca` or `verify-full`, with libpq's meaning. `allow` and `prefer` are rejected: they fall back to plaintext without saying so. `disable` is for a server without TLS on a trusted network: kind, a local container |
| `HEARTBEAT_API_POSTGRES_SSLROOTCERT` | unset (the system's CAs) | PEM file of the CA certificates `verify-ca` and `verify-full` trust. Amazon RDS needs its CA bundle here |
| `HEARTBEAT_API_POSTGRES_CREDENTIAL_REF` | `secret/api-postgres` | Credential reference of the login, `username:password` ([credential resolution](#credential-resolution)): by default `HEARTBEAT_CREDENTIAL_SECRET_API_POSTGRES`. `api migrate up` uses it too; give that command another reference to migrate as a different role, such as the schema's owner |
| `HEARTBEAT_API_REDIS_ADDR` | required | Redis (Valkey) server, `host:port` |
| `HEARTBEAT_API_REDIS_TLS` | `true` | TLS, verified against the system's CAs and the host name, as ElastiCache's in-transit encryption needs. `false` for a server without TLS on a trusted network: kind, a local container |
| `HEARTBEAT_API_REDIS_CREDENTIAL_REF` | `secret/api-redis` | Credential reference of the login, `username:password`: by default `HEARTBEAT_CREDENTIAL_SECRET_API_REDIS`. For Valkey's `requirepass` or an ElastiCache AUTH token, the username is `default`, or empty to send the password alone |
| `HEARTBEAT_CREDENTIALS_DIR` | unset | As for the DB collector: a directory of `HEARTBEAT_CREDENTIAL_<REF>` files, read on every new connection |
| `HEARTBEAT_API_AUDIT_ENDPOINT` | unset (records wait) | OTLP/HTTP base URL audit records are sent to: the OTel Collector's logs receiver with the persistent queue, the one the OutSystems logs use, such as `http://otel-collector:4318`; a trailing `/v1/logs` is dropped. The chart sets it while `otelCollector.enabled` is on. Unset, the API logs `audit delivery off` at startup and the records wait in the outbox, which `HeartbeatAuditDeliveryStalled` reports ([audit delivery](metrics-and-endpoints.md#audit-delivery)). Not `OTEL_SDK_DISABLED`'s to turn off: audit records are not the API's own telemetry, and never go to the `4319` receiver of its own logs |
| `HEARTBEAT_API_SITE_ORIGIN` | unset (unsafe requests refused) | The origin browsers reach the API at, such as `https://heartbeat.example.com`: scheme and host, a port only when it is not the scheme's default, no path. `POST`, `PUT`, `PATCH` and `DELETE` need one `Origin` header equal to it, or are refused with 403 before they run, logging in included ([sessions](metrics-and-endpoints.md#logging-in-and-sessions)). Written as a browser writes the header: lower case, without `:443` or `:80` or a trailing slash. Unset, the API logs a warning at startup and nobody can log in |
| `HEARTBEAT_API_SESSION_IDLE_TIMEOUT` | `8h` | A session ends after this long without a request. A Go duration, at least `1m`, at most the max age. Requests move the deadline at most once a minute |
| `HEARTBEAT_API_SESSION_MAX_AGE` | `168h` (7 days) | A session ends this long after login, however active. At least `1m`. The cookie's `Max-Age` too |
| `HEARTBEAT_API_TRUSTED_PROXIES` | unset (none) | Comma-separated addresses or CIDRs, such as `10.0.0.0/8`, of proxies (an ingress controller) whose `X-Forwarded-For` names the client. The API reads the header from its end, only as far as these proxies wrote it, so a client cannot choose its own address. The client address is what failed logins are counted by and what audit records show; unset, it is the TCP peer's |
| `HEARTBEAT_API_SHUTDOWN_DELAY` | `5s` | How long the API keeps serving after `SIGTERM`, with `GET /readyz` answering 503, before it stops accepting connections: time for its Service to stop sending it requests. `0s` to `1m`. The chart's `api.terminationGracePeriodSeconds` (30) covers 5s plus the 10s drain and the rest of the shutdown; raise it with the delay |
| `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT`, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`, `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG`, `OTEL_RESOURCE_ATTRIBUTES`, `OTEL_SDK_DISABLED`, `HEARTBEAT_PPROF_ADDR` | unset | As for the DB collector. The `api migrate` and `api users` commands send no telemetry |
| `GOMEMLIMIT` | unset | As for the DB collector, from `api.resources.limits.memory` and `api.goMemLimit` |
| `HEARTBEAT_CREDENTIAL_<REF>` | — | A credential without a file, see [credential resolution](#credential-resolution) |

The settings above win over libpq's `PG*` environment variables, which the
PostgreSQL driver also reads. Each replica keeps at most 8 PostgreSQL
connections (`application_name` `heartbeat-api`); a connection attempt gives
up after 5s.

The API connects lazily: it starts while PostgreSQL or Redis is down or its
credential is missing, and `GET /readyz` answers 503 until both work
([endpoints](metrics-and-endpoints.md#api-8081)).

The same binary applies the migrations and creates admins
([ADR 0013](../architecture/decisions/0013-api-design-and-subsystem-boundaries.md)):
the chart runs `api migrate up` in each API pod's `migrate` init container,
before the API starts. These commands read only the PostgreSQL settings and
the credential lookup, then exit:

| Command | Does |
| --- | --- |
| `api migrate up` | Applies, in order, each migration of [`db/migrations`](../../db/migrations) newer than the database's version, and records it in `schema_migrations`. Replicas may run it at once: a PostgreSQL advisory lock makes the others wait and each migration runs once. Each wait for the lock ends after 5 minutes, or on SIGTERM, and fails the command. A database already past the newest migration it embeds (after rolling the API back) is left alone, and the command succeeds. A dirty version fails it ([repair](../guides/local-development.md#change-a-query-or-a-migration)) |
| `api migrate version` | Prints the schema version, `0` when never migrated, with ` (dirty)` while a migration runs or after one failed partway. It only reads `schema_migrations`: it creates nothing and does not wait for a migration in progress |
| `api users create-admin --email ADDRESS [--name NAME]` | Creates a user with the `admin` role and the password read from standard input, and its audit record, in one transaction. There is no default password: this is how the first admin is made, and how anyone who can run a command in an API container gets back in. The email is stored in lower case and must not belong to a user already, in any case; the name defaults to the email's local part. The password is the first line of standard input, its line end dropped, from 15 characters ([NIST SP 800-63B-4](https://pages.nist.gov/800-63-4/sp800-63b.html)'s minimum for a single factor) to 1,024 bytes, stored only as its argon2id hash (RFC 9106's second recommended setting: 64 MiB, t=3, p=4). A terminal is refused, since it echoes what is typed: pipe the password in. Fails, writing nothing, on a schema older than the API's (`run api migrate up`). Logs the user's ID and the audit record's ID; the record is sent by the API's drainer, as `api users create-admin` on the pod's host name |

Creating the first admin, where the password never reaches the command line
or the shell history; on a cluster, the same through `kubectl exec -i` into an
API pod (the chart adds the API [4.1]):

```bash
read -rs PASSWORD && printf '%s\n' "$PASSWORD" | \
  api users create-admin --email admin@example.com --name "First Admin"; unset PASSWORD
```

### Local development files

| File | Tracked | Purpose |
| --- | --- | --- |
| `.env.sqlserver-dev.example` | yes | Template for the SQL Server sandbox |
| `.env.sqlserver-dev` | no (mode 600) | Random per-machine SA password and least-privilege collector credential (`heartbeat_collector:<password>`), generated by `make sqlserver-dev-init`; `make sqlserver-dev-up` creates that login with `sa` and copies only the collector credential into the `heartbeat-sqlserver-dev-credentials` Secret |
| `infra/helm/values/sqlserver-dev.yaml` | yes | Values that point the collector at the sandbox container |
