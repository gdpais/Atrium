# PatientZero

A synthetic OutSystems 11 application for Heartbeat's tests and demos
([ADR 0010](../../docs/architecture/decisions/0010-patientzero-test-application.md)).
Each request runs real SQL against PatientZero's business database and
writes the log rows the platform would write for it into its own logging
database, all under one `Request_Key`, with the caller's session, user and
client IP. The db-collector's `outsystems_logs` collector reads those rows
the same way it reads a real logging database.

It is test tooling: its own Go module, image and Helm chart, never part of
the Heartbeat module, chart, images or releases.

> **Status:** first slice [16.5]: the service, its two databases on the
> sandbox SQL Server, three entry points, k6 load, and the scenario runner
> with its run journal, seven in-process and two cluster faults, delivery
> checks, the SLO scenarios, the ingest scenarios and the 48 hour backfill.
> Timers, extension calls, the fake REST partner and the rest of the
> compressed calendar come later. The logging schema is the collector
> fixtures', the DDL of a surveyed OutSystems 11 logging database.

## Run it on kind

```bash
make sqlserver-dev-up    # the sandbox SQL Server, with Heartbeat deployed against it
make patientzero-up
kubectl --context kind-heartbeat -n patientzero port-forward svc/patientzero 8080
```

```bash
curl -H 'X-PatientZero-User: maria.santos' -H 'X-PatientZero-Session: s1' -H 'X-Forwarded-For: 10.20.30.41' \
  -d '{"productId":2,"quantity":3}' \
  http://localhost:8080/PatientZero_App/screenservices/PatientZero_App/MainFlow/Checkout/ServiceAPIPlaceOrder
```

About a minute later the rows are in Loki as `{service_name="PatientZero"}`
(the screen log's `lag` is longer, because a screen row is stamped with the
request's start).

With [load](#load) running (`make patientzero-load`), Grafana's *OutSystems
Overview* shows the application `PatientZero`, and the other OutSystems
dashboards drill into it
([dashboards](../../docs/architecture/application-observability.md#dashboards)).

```bash
make patientzero-down    # remove PatientZero, its databases and its login
```

[`scripts/patientzero.sh`](scripts/patientzero.sh) does the work: it takes
the same `KIND_CLUSTER`, `SQLSERVER_CONTAINER` and `SQLSERVER_ENV_FILE` as
`scripts/kind.sh`, so it also runs against a second, throwaway cluster.
`make patientzero-up` also redeploys the Heartbeat chart with an
`outsystems_logs` collector named `patientzero` that reads `patientzero_log`.
A later `make kind-deploy` drops that collector; run `make patientzero-up`
again to restore it.

### Who can reach it

PatientZero believes the identity its request headers claim
([entry points](#entry-points)), so anything that reaches port 8080 can
write rows as any user, session and client IP into its logging database
and, through the collector, into Loki. The chart keeps the callers to the
known ones:

- The Service is `ClusterIP`, never a NodePort or LoadBalancer.
- One NetworkPolicy denies ingress to every pod in the `patientzero`
  namespace, and another admits port 8080 only from the k6 load Job's pods
  (`app.kubernetes.io/name: patientzero-load`), which `make patientzero-load`
  and `make patientzero-scenario` start
  ([`templates/networkpolicy.yaml`](chart/templates/networkpolicy.yaml)).
- `kubectl port-forward` connects from inside the pod, so NetworkPolicy does
  not see it: the `curl` above, k6 on the host ([load](#load)), and the admin
  calls of `make patientzero-scenario`, locally and in the scenarios
  workflow, go that way. Kubelet probes come from the node, which
  NetworkPolicy does not filter either.
- Nothing else is admitted: a pod in another namespace, or another pod in
  this one, times out on port 8080. Prometheus does not scrape PatientZero,
  which has no metrics endpoint.

A new caller inside the cluster needs its own peer in that policy. The peer
matches a pod label, so whoever may create pods in the namespace can claim
it: RBAC on the namespace is part of the control. kind's kindnet enforces
NetworkPolicy; a CNI that does not ignores it.

## Entry points

The caller's identity comes from headers, so a load generator can give each
virtual user a stable user and session: `X-PatientZero-User` (the username;
without it the caller is anonymous, user 0), `X-PatientZero-Session` (a new
session per request without it) and `X-Forwarded-For` (the client IP; the
connection's peer without it).

| Entry point | Request | Business SQL | Log rows |
| --- | --- | --- | --- |
| Traditional web screen `OrderList` | `GET /PatientZero_Web/OrderList.aspx` | The user's newest orders | `screen`, stamped with the request's start |
| Reactive server action `Checkout.PlaceOrder` | `POST /PatientZero_App/screenservices/PatientZero_App/MainFlow/Checkout/ServiceAPIPlaceOrder` with `{"productId":2,"quantity":3}` | Takes the quantity from stock and records the order, in one transaction | `mobile_request`, stamped with the end |
| Exposed REST API `Orders.GetOrders` | `GET /PatientZero_API/rest/Orders/orders` | The user's newest orders | `integration` (exposed, `Source` is the caller) and `general` (the API's own message, which carries the user and session the integration log has no columns for) |

A failed request adds an `error` row under the same `Request_Key`, and the
other rows' `Error_Id` points at it; its `EnvironmentInformation` names the
module's version (`eSpaceVer`), which a scenario's bad deploy changes. A
business statement that takes more than 200 ms adds the platform's
slow-query row, a `general` row with `Module_Name` `SLOWSQL`
([scenarios](scenarios/README.md#fault-registry)). An unknown product or a quantity below 1
fails `PlaceOrder` with HTTP 400. Every user is a `Customer` row in the
business database, created on first use; its id is the `User_Id` in the logs.

Every response of the order list, its errors and `503`s included, carries
`X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY` and a
Content-Security-Policy under which the page loads nothing and cannot be
framed (`default-src 'none'; base-uri 'none'; form-action 'none';
frame-ancestors 'none'`): the page has no script, style, image or form of
its own. Every entry point answers a failure with a generic message; the
detail is only in the `error` row.

`GET /healthz` reports that the process runs; `GET /readyz` that both
databases answer and the schemas are in place. A pool whose every
connection is in use counts as answering, so a slow SQL Server does not take
the pod out of its Service while the entry points shed load with `503`
(`PATIENTZERO_MAX_IN_FLIGHT`).

## Load

[k6](https://grafana.com/docs/k6/) drives the entry points with virtual
users of stable identity, so distinct-user counts and per-user timelines in
Heartbeat have a known answer
([`load/patientzero.js`](load/patientzero.js)):

```bash
make patientzero-load                          # steady, 10 minutes
make patientzero-load LOAD=peak                # 3x the users and API rate
make patientzero-load LOAD=soak LOAD_DURATION=8h
make patientzero-load-stop
```

The load runs in the cluster as the `patientzero-load` Job, so no
port-forward limits it, and the command follows its output. Interrupting the
command stops following, not the load. The command fails when a k6 threshold
is crossed: more than 1% of checks failing, or more than 1% of requests
failing for a reason other than a refused order.

| Profile | Shoppers | API calls | Holds for |
| --- | --- | --- | --- |
| `smoke` | 2 | 30 a minute from one partner | 30s |
| `steady` | 20 | 120 a minute from three partners | 10m |
| `peak` | 20, ramped to 60 over 2 minutes and back down after | 120 a minute, ramped to 360 | 5m at the top |
| `soak` | as steady | as steady | 2h |
| `day` | 2 at 04:00 to 38 at 11:00, following a real day's curve | 10 to 227 a minute, the same curve | 24m, a minute per hour |

`LOAD_DURATION` sets how long a profile holds (peak: at the top; day: the
whole day, split evenly over its 24 hours).

- **Shoppers.** Virtual user *n* is `shopper.<n>` (`shopper.0001`, ...)
  from client IP `10.20.x.y`, the same on every run. One k6 iteration is one
  session, with the session id `<run>.v<n>.<session number>`. A session opens
  the order list, then opens it again or places an order, with think time
  between steps and a pause between sessions. One session in ten is
  anonymous: it only opens the order list. 0.3% of orders name an unknown
  product, so they get HTTP 400 and an `error` row: about 0.07% of all
  requests, inside the 99.9% objective of Heartbeat's
  [SLO alerts](../../docs/architecture/application-observability.md#slos-and-alerts).
- **Partners.** `partner.<k>` call the exposed REST API from `10.30.0.<k>`,
  with no session: each call gets a new one, as a stateless API client does.
- **Run id.** `<profile>-<UTC start time>`, printed at the start, begins
  every session id, so runs never share a session and a run's rows can be
  found in Loki with `| Session_Id=~"steady-20261019T101500Z.*"`.
- **Choices.** Every virtual user has its own seeded random sequence, so the
  same `LOAD_SEED` (default 1) gives every user the same steps on every run.
  Timing still varies with response times.

The mix (users, session length, order and anonymous shares, refused orders,
think times, API rate, peak factor, the day's curve) is in
[`load/traffic.js`](load/traffic.js). Its shapes come from the
logging-database survey [3.3]: the integration-to-screen ratio, the peak
factor and the hourly curve of one real week; its volume is scaled down to a
laptop.

Against PatientZero running anywhere else, for example a port-forward:

```bash
k6 run -e BASE_URL=http://localhost:8080 -e PROFILE=smoke tools/patientzero/load/patientzero.js
```

## Scenarios

A scenario injects a fault for a while and names the signals Heartbeat must
show for it: PromQL, LogQL and Alertmanager alerts, each within a window.
PatientZero runs it behind an admin endpoint, checks the signals and writes
a JSONL run journal: each fault's start and end, the users, sessions and
requests it served, and each check's result
([format, fault registry, journal](scenarios/README.md)).

```bash
make patientzero-scenario                        # list them
make patientzero-scenario NAME=checkout-errors   # run one with its load; exits 0 if every signal showed
make patientzero-scenario NAME=bad-deploy        # the error budget alerts, in Alertmanager (about 19 minutes)
make patientzero-scenario NAME=slow-sql          # the latency budget alerts (about 25 minutes)
make patientzero-scenario NAME=rotation          # the weekly rotation; Loki must hold every row
```

The journal is saved to `.tmp/patientzero-scenarios/<run>.jsonl`. The
ingest scenarios (`rotation`, `log-backlog`, `late-commits`, `log-growth`)
check that Loki holds exactly the rows PatientZero committed, which it
counts as it writes them
([delivery checks](scenarios/README.md#delivery-checks)). Faults that act
on other workloads (`restarts`, `loki-outage`, `otel-outage`,
`collector-outage`, `sqlserver-outage`, `tempo-outage`) need the kubeconfig
and Docker, which PatientZero does not have: `make patientzero-scenario`
applies them when PatientZero asks, as the run's host agent
([cluster faults](scenarios/README.md#cluster-faults)).

## Production-sized logging database

A production logging database cannot be load-tested before go-live, so a
test one is filled to the same size: two weeks of the surveyed
installation's log, written straight into the cycle tables of
`patientzero_scale` on the sandbox SQL Server, then every collector query
measured against it ([`cmd/oslog-scale`](cmd/oslog-scale/main.go),
[`internal/scale`](internal/scale/profile.go)). PatientZero on kind and its
`patientzero_log` are not touched.

```bash
make patientzero-scale                    # generate, then measure (about 2 minutes)
make patientzero-scale SCALE=2            # twice the surveyed installation
make patientzero-scale-measure            # measure again, on the data as it is
make patientzero-scale-down               # drop patientzero_scale
```

- **What it writes.** The previous week (full) and the current week up to
  now, each in the cycle table its view reads, with the surveyed week's
  volume and shape: 4.51 M screen, 1.43 M integration and 160 k extension
  rows a week, about 72 k slow-SQL rows, the hourly curve, a busiest minute
  about 3.8 times the average one. The survey's extract held no other
  general rows (here 90 k a week, 80% of them slow SQL), no errors (0.5% of
  requests), timers (20, every 10 minutes), mobile requests or request
  events (none): those are assumptions, in
  [`Surveyed`](internal/scale/profile.go).
- **Users, sessions and errors.** 4,829 users and 13,871 sessions a week,
  every user in every week; requests go to the sessions open at the time.
  Errors come from 40 recurring kinds with a long tail, and 3 new kinds a
  day after the first, at known times; a quarter of the kinds are database
  failures whose trace goes to `oslog_Error_Detail`. Each kind is one
  `error.fingerprint`.
- **Known answers.** `.tmp/patientzero-scale/expected.json`: per week, day
  and hour, the requests, rows per log type, distinct users and sessions,
  error rows, error kinds and new error kinds; the busiest minute; every
  error kind and when it was first seen. `make patientzero-scale-check`
  checks the dashboards against it ([below](#in-loki)).
- **The measurement.** `.tmp/patientzero-scale/report.md`: the rows and
  space per log type, then each query, run as the collector runs it
  (`batch_limit` 5000, `datetime2` bounds), over the busiest minute and
  backlogs of an hour, a day and a week (an outage; a week is
  `initial_lookback`'s limit), with logical reads, CPU and elapsed time.
  `SCALE_SEED` and `SCALE_END` (RFC 3339) make a run reproducible.
- **Memory.** Loading and reading 3 GB fills the sandbox SQL Server's
  buffer pool, and SQL Server keeps it after
  `make patientzero-scale-down`. On Docker Desktop's default 8 GB, next to
  kind, that starves the disposable server of `make test-sqlserver` (its
  tests time out). Restart the sandbox afterwards
  (`docker restart heartbeat-sqlserver-dev`; its data stays) or give Docker
  more memory.

On 2026-10-20, at `SCALE=1` (9.8 M rows, 3.0 GB, generated in 53 s), no
query took more than 160 ms, against a probe deadline of half the scrape
interval and at most 10 s:

| Log type | Busiest minute | 1 h backlog | 1 day backlog | 7 day backlog |
| --- | ---: | ---: | ---: | ---: |
| `screen` | 6 ms, 1,871 reads | 17 ms, 5,024 | 17 ms, 5,037 | 29 ms, 10,801 |
| `integration` | 4 ms, 2,340 | 34 ms, 20,273 | 34 ms, 20,296 | 59 ms, 41,813 |
| `extension` | 0 ms, 76 | 4 ms, 1,097 | 16 ms, 5,254 | 23 ms, 10,723 |
| `general` | 0 ms, 33 | 4 ms, 608 | 36 ms, 5,361 | 45 ms, 10,571 |
| `error` | 0 ms, 144 | 4 ms, 2,311 | 61 ms, 48,471 | 160 ms, 139,284 |
| `cyclic_job` | 0 ms, 6 | 1 ms, 124 | 7 ms, 2,894 | 22 ms, 10,404 |

`mobile_request` and `request_event` have no rows in this profile. A
backlog costs about the same as an hour: each query reads at most
`batch_limit` rows per weekly view. The error log reads the most per row: a
lookup into its cycle table, clustered on `Id`, and one into the detail
table.


### In Loki

```bash
make patientzero-scale-collect            # a collector reads patientzero_scale (SCALE_LOOKBACK, default 48h)
make patientzero-scale-check              # compare Loki with expected.json; writes check.md
make patientzero-scale-check SCALE_WAIT=45  # first wait up to 45 minutes for the collector to catch up
```

The [scenarios workflow](../../.github/workflows/scenarios.yml) runs all
three after the scenarios, nightly and on the release pull request, with
`SCALE_WAIT=45`; `check.md` is in the run's summary.

`patientzero-scale-collect` redeploys Heartbeat with a second
`outsystems_logs` collector, `patientzero-scale`, next to PatientZero's. Its
target and `deployment.environment.name` are `scale-<end of the log>`, so a
regenerated log starts with fresh watermarks and its own rows in Loki;
`make patientzero-scale` moves a running collector to the new log, and
`make patientzero-scale-down` removes it before dropping the database.
Loki takes rows up to 168 hours old, so a lookback of more than about six
days loses the oldest.

`patientzero-scale-check` reads Loki through a port-forward and needs no SQL
Server. With `SCALE_WAIT`, it first reads once a minute how many hours Loki
holds in full, each time through a new port-forward, until it holds the
last one (the collector delivers each log type in order) or the minutes run
out. For every hour from the collector's start minus its lookback, Loki
must hold every row, and the distinct users and sessions on screen rows
(where a request starts, as `expected.json` counts them) and the distinct
error fingerprints must match; so must each whole day's fingerprints. It
then runs the dashboards' distinct-count panels, read from the chart's
dashboard files, over 1 hour, 24 hours and everything collected (the
5-minute ones at their busiest), and reports each value as a share of
Loki's `max_query_series` (20,000): a distinct count holds one series per
value. Catching up 48 hours (1.78 M rows) takes about 16 minutes, held to
Loki's 4 MB/s ingestion rate.

On 2026-10-21, at `SCALE=1` with a 48 h lookback, all 48 hours matched,
and the panels' own counts over every log type matched too. The largest
panel held a tenth of the limit:

| Panel | 1 h | 24 h | 47 h |
| --- | ---: | ---: | ---: |
| Distinct errors | 56 | 74 | 74 |
| Users affected | 150 | 1,179 | 1,977 |
| Top errors (rows before `topk`) | 128 | 507 | 563 |
| Active users (5m), busiest | | | 598 |
| Active sessions (5m), busiest | | | 627 |

On 2026-11-02 the scenarios workflow ran it on CI, on the current export
path (the db-collector waits for the Collector in the background): all 48
hours matched again, Loki caught up in 16.5 minutes, and *Users affected*
over 47 h held 2,629 series
([run](https://github.com/gdpais/Heartbeat/actions/runs/41538460130)).

*Users affected* grows with the range, up to the users who hit an error,
at most the week's 4,829 at `SCALE=1`; it is the panel to watch on a larger
installation (`SCALE`) or a longer range.

The first runs lost rows on the way to Loki: 510,637 rows discarded as
`too_far_behind`, then 991,189 dropped at a full exporter queue, all after
the db-collector had moved its watermark past them. The OTel Collector's
logs exporter now sends in order, retries without a deadline, cuts batches
below Loki's ingestion burst, and has no batch processor in front of it,
so a full queue reaches the db-collector as a 503
([application observability](../../docs/architecture/application-observability.md)).

## Databases and privileges

| Database | Holds | Created by |
| --- | --- | --- |
| `patientzero` | `Customer`, `Product` (five seeded products), `SalesOrder` | PatientZero, at startup |
| `patientzero_log` | Ten cycle tables per log type, `oslog_<TYPE>_0` to `_9`, and the views `oslog_<TYPE>` (current week, cycle 1 at first) and `oslog_<TYPE>_Previous` (cycle 0) | PatientZero, at startup |
| `patientzero_scale` | The same schema, with two weeks of a production-sized log ([above](#production-sized-logging-database)) | `make patientzero-scale`, as `sa`; dropped by `make patientzero-scale-down` |

- PatientZero logs in as `patientzero`, whose password `make patientzero-up`
  generates into `.env.sqlserver-dev` (`PATIENTZERO_SQL_CREDENTIAL`). The
  login is `db_owner` of the two databases and has no server permissions:
  it creates its own schemas and, later, rotates the log views.
- The Heartbeat collector keeps its own least-privilege login. In
  `patientzero_log` it is a member of `heartbeat_oslog_reader`, which has
  `SELECT` on the log views only: what a real logging database grants it.
- Log rows are inserted through the current week's views, so a rotation
  only has to move the views. The error view's `Stack` is computed from a
  join with `oslog_Error_Detail_<cycle>`, so error rows go into the cycle
  table that `oslog_Error` reads, looked up in
  `sys.sql_expression_dependencies` on every write; a trace longer than the
  cycle table's `Stack` (2000 characters) also goes, whole, into that
  cycle's detail table. A database failure's trace is that long. A
  request's rows across log types commit in one transaction.

## One logging schema

The logging schema is not defined here. PatientZero reads the db-collector's
fixtures,
[`services/db-collector/internal/collectors/outsystems/testdata/oslog/<log type>.sql`](../../services/db-collector/internal/collectors/outsystems/testdata/oslog/),
takes the first cycle table and the current view of each (the error
view's join with its detail table included), and builds the ten cycle
tables, the error log's ten detail tables and the two views from them
([`internal/logschema`](internal/logschema/logschema.go)). The fixtures are
the DDL of a surveyed OutSystems 11 logging database, so when they change,
PatientZero follows without a change here. A fixture whose shape it cannot
read (no cycle table with a `Cycle` default, no current view with a column
list over it, or over it left-joined to a detail table) fails `make test`,
and a column PatientZero writes that the fixture no longer has fails the
entry-point tests. Existing tables and views are kept, so a logging
database built from older fixtures keeps their schema: `make
patientzero-down` drops it and the next `make patientzero-up` builds the
current one.

The image copies the fixtures to `/app/oslog`
([`Dockerfile`](Dockerfile), built from the repository root);
`PATIENTZERO_SCHEMA_DIR` points elsewhere.

What the survey left open: `mobile_request.Duration` is written in seconds,
as the OutSystems reference documents it; the surveyed database had no
mobile request rows to confirm the unit. Every other log's `Duration` is in
milliseconds. `Error_Id` is text in every log, empty when there is none.

## Configuration

Environment variables (the chart sets them):

| Variable | Default | Meaning |
| --- | --- | --- |
| `PATIENTZERO_SQL_HOST` | required | SQL Server host |
| `PATIENTZERO_SQL_PORT` | `1433` | SQL Server port |
| `PATIENTZERO_SQL_CREDENTIAL` | required | `<login>:<password>`; never `sa` |
| `PATIENTZERO_SQL_TRUST_SERVER_CERTIFICATE` | `false` | `true` only for the sandbox's self-signed certificate |
| `PATIENTZERO_SQL_MAX_OPEN_CONNS` | `20` | Connections each SQL Server pool (business and logging) may open (chart `sql.maxOpenConns`); requests past it wait for one |
| `PATIENTZERO_DATABASE` | `patientzero` | Business database |
| `PATIENTZERO_LOG_DATABASE` | `patientzero_log` | Logging database |
| `PATIENTZERO_SCHEMA_DIR` | `/app/oslog` | The oslog fixtures the logging schema is built from |
| `PATIENTZERO_LOG_READER_ROLE` | `heartbeat_oslog_reader` | Role granted `SELECT` on the log views; empty for none |
| `PATIENTZERO_FRONT_END` | the host name (the pod name on kind) | Front-end server in `Executed_By` and `Server` |
| `PATIENTZERO_LISTEN` | `:8080` | Listen address |
| `PATIENTZERO_MAX_IN_FLIGHT` | `64` | Entry-point requests served at once (chart `maxInFlight`); past it they get `503` with `Retry-After: 1`, and the journal counts them as `shed`. `0` turns the limit off. The health and admin endpoints are never limited |
| `PATIENTZERO_ADMIN_TOKEN` | none | Bearer token for the scenario admin endpoints; without it they answer 401. The chart reads it from the `patientzero-admin` Secret (`admin.existingSecret`) |
| `PATIENTZERO_JOURNAL_DIR` | `/var/lib/patientzero/journal` | Where run journals are written; the chart mounts an `emptyDir` (`journal.sizeLimit`) |
| `PATIENTZERO_PROMETHEUS_URL`, `PATIENTZERO_LOKI_URL`, `PATIENTZERO_ALERTMANAGER_URL` | none | Where scenario signals are read (chart `signals.*`); a scenario whose signals need a missing one is not runnable |

## Tests

```bash
go -C tools/patientzero test ./...    # also part of make test, make test-race and make vet
```

`TestLiveSQLServer` runs the entry points against a real SQL Server and reads
the rows back with the collector's own queries from
[`queries/`](../../services/db-collector/internal/probes/outsystems/queries/README.md).
It is skipped unless `PATIENTZERO_TEST_SQLSERVER` is a `sqlserver://` URL for a
login that may create databases. `make test-sqlserver` runs it against a
disposable SQL Server container as that container's `sa`, and so does CI's
`SQL Server probes` job, which runs whenever `tools/patientzero` changes
outside its chart. To run it on its own against the sandbox's `sa`:

```bash
PATIENTZERO_TEST_SQLSERVER="sqlserver://sa:$(grep '^MSSQL_SA_PASSWORD=' .env.sqlserver-dev | cut -d= -f2-)@127.0.0.1:11433?encrypt=true&TrustServerCertificate=true" \
  go -C tools/patientzero test -count=1 -run '^TestLive' ./internal/...
```

It creates two databases with random names and drops them at the end.
`TestLiveScale`, in the same runs, generates a small production-shaped log
into a `patientzero_scale_test_<random>` database, checks it against its
known answers and the long traces against the detail tables, measures the
queries on it, and drops it. The generator's own tests (no SQL Server)
check the profile's volumes, shape, users, sessions and error kinds.

`TestLoadSmokeProfile` runs the load script's smoke profile for a few
seconds against the real handlers and checks the identities in the rows
written. It is skipped when `k6` is not on the `PATH` (CI does not install
it); `TestLoadScriptUsesTheServersPathsAndHeaders` always checks that the
script calls the paths and headers the server serves.

## Why it lives in this repository

PatientZero shares two contracts with Heartbeat that change together with
the product: the logging schema above and, with the scenarios, Heartbeat's
own metric names and queries. In one repository a change to either is one
pull request and one CI run, not coordinated releases of two repositories. It is kept separable: its own module
with no imports from Heartbeat's, its own Dockerfile and chart, and nothing in
the product refers to it. It moves to its own repository if it gains users
outside Heartbeat, its own release cadence, or grows into a general
simulator.
