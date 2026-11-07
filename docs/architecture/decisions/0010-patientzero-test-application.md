# 0010. PatientZero, a synthetic OutSystems application to test against

- Status: Accepted; first slice built (service, databases, three entry
  points, k6 load, scenario runner) [TODO 16.5]. Updated 2026-10-19: the survey ran and the replica is gone
  (see Context); its DDL and aggregates are what PatientZero uses. Updated
  2026-10-23: the `bad-deploy` and `slow-sql` rows below describe those
  scenarios as built.
- Date: 2026-10-18

## Context

Heartbeat only reads. The SQL Server collector reads DMVs, the
`outsystems_logs` collector reads the `oslog_*` tables of an OutSystems 11
logging database, and nothing in the stack makes either of them do anything.
Today the tests get their input three ways:

- The OutSystems log fixtures were written from the OutSystems 11 Log data
  reference, not from a logging database
  ([ADR 0008](0008-outsystems-11-log-extraction.md)).
- `make kind-e2e` and the [live demo](../../guides/live-demo.md) create
  load by hand: an `orders` table, a blocking chain started from the shell.
- No test has an application whose database load and application logs come
  from the same requests, although that link is what Heartbeat sells:
  "what happened to this user between 10:00 and 10:15", across logs,
  metrics and database evidence.

The exits of phases 2 to 7 need input that is realistic, repeatable and
comes with a known answer: dashboards and derived metrics in phase 2,
onboarding through the API and UI in phases 3 and 4, investigations,
alerts and reports in phase 7.

A replica of a real OutSystems 11 logging database was available to the
team on `127.0.0.1:1433` (an SSH tunnel), with the real DDL, units and
traffic shape, and real users' data. The survey
([`scripts/outsystems-log-survey.sql`](../../../scripts/outsystems-log-survey.sql))
ran against it on 2026-10-19. Its native schema was complete, ten cycle
tables per log type and their views, but every cycle table was empty: the
data was one flattened week extracted by a performance-analysis tool. So the
survey gave the DDL and an aggregate traffic profile (rows per hour, peak
minute, integration-to-screen ratio, millisecond durations), but no native
rows, no time zone and no entry-point coverage. The replica was then
deleted: real users' data does not stay on a laptop for numbers already
taken.

## Decision

| Concern | Choice | Why |
| --- | --- | --- |
| What | **PatientZero**: a small Go web application that behaves like an OutSystems 11 application, for tests and demos only. The name leaves room for more complex test applications later | A target Heartbeat can monitor end to end, under the test's control. |
| Behaviour | HTTP endpoints modelled on OutSystems entry points: Traditional web screens, Reactive server actions, an exposed REST API, timers, extension calls and a consumed REST partner (a fake partner inside PatientZero with injectable latency and errors). Each request runs real SQL against PatientZero's business database **and** writes the log rows the platform would write for it: one `Request_Key` across log types and modules, with the caller's session, user and client IP | Database load and application logs share a cause. Blocking in the business database really slows the screens and fills the error log, so correlation can be tested, not assumed. |
| Where it writes | Two databases on the sandbox SQL Server (`make sqlserver-dev-up`): `patientzero` (business tables) and `patientzero_log` (the ten cycle tables per log type, `oslog_<TYPE>` and `oslog_<TYPE>_Previous` views), created from the surveyed DDL. PatientZero logs in with its own login, with write rights on those two databases only | The collector reads PatientZero exactly as it reads a real installation, with its unchanged least-privilege login. |
| The survey | A real logging database is only **read**, never written to: the logging-database survey runs against it read-only. Its DDL becomes PatientZero's logging schema and the CI fixtures' schema; its aggregates become PatientZero's traffic profile ([`load/traffic.js`](../../../tools/patientzero/load/traffic.js)). Synthetic data is PatientZero's alone, written to its own databases | A real database holds real users' data and may itself be read-only; synthetic rows mixed into real ones would break every exact expectation. Reading it is how PatientZero stays faithful. |
| Load | [k6](https://grafana.com/docs/k6/) scripts drive virtual users with stable identities (user, session, client IP) through the endpoints. Profiles: smoke, steady, peak, soak, and day (the surveyed day's hourly curve, compressed) | Realistic concurrency and a known population, so distinct-user counts and per-user timelines can be checked. |
| Scenarios | Named fault scenarios, each a file under PatientZero: what it injects, for how long, and the signals it must produce (a PromQL expression, a rule or alert, a LogQL query). Started through PatientZero's admin endpoint; `make kind-e2e` runs them and checks the signals. First set in the table below | One scenario is one test with an exact expectation; the same files drive demos. |
| Known answers | Every run writes a JSONL journal: users, sessions and requests, and each fault with its start and end | The expected result for investigations (phase 7) and reports, written before Heartbeat looks. |
| Time | PatientZero never changes the collector's clock. It compresses the calendar instead: weekly rotation on demand (the view switch the platform does on Fridays at 23:45), backfill of past days within Loki's 168h `reject_old_samples_max_age`, commits held back past the `lag`, long-running screens and timers for the start-stamped log types, and `Instant` written in a configured time zone, daylight saving switches included | Exercises rotation, lag, watermarks, late rows and time zones in minutes, without a time-warp switch in production code. |
| Where it lives | `tools/patientzero/`, its own Go module (like `tools/docsite`), with its Dockerfile, k6 scripts, scenarios and a small Helm chart installed into a `patientzero` namespace on kind by `make patientzero-up` | Test code stays out of the product's module, chart, images and releases. |

First scenarios:

| Scenario | Injects | Must show |
| --- | --- | --- |
| `blocking` | A long transaction holding locks other requests need; a deadlock variant | Blocked requests, lock waits; slower screens and timeout errors for the same requests |
| `bad-deploy` | A broken publish of one module: its requests fail with a new exception until the previous version is published again | The module's error rate, the exception in Loki, and the error-budget burn-rate alerts in Alertmanager |
| `slow-partner` | Latency and errors from the consumed REST partner | `heartbeat_outsystems_integration_duration_seconds`, slower parent screens |
| `slow-sql` | A slow query: the business statement runs longer in SQL Server (a slow extension call comes with extensions, phase 7) | Running sessions on SQL Server, `heartbeat_outsystems_slow_events_total` from `SLOWSQL` rows, and the latency-budget burn-rate alerts in Alertmanager |
| `timer-backlog` | Timers that start late against their schedule | `heartbeat_outsystems_timer_delay_seconds` |
| `bad-front-end` | Errors and latency on one simulated front-end server | Skew in `heartbeat_outsystems_front_end_operations_total` |
| `log-growth` | Fast growth of `patientzero_log` | The SQL Server storage probes on the logging database |
| `log-backlog` | Rows committed late, then within the lag again | `heartbeat_outsystems_log_delay_seconds`; no gaps in Loki |
| `rotation` | The Friday view switch while traffic runs | No rows lost or duplicated across the switch |

Considered and not chosen:

- **A real OutSystems 11 installation.** The most faithful, but it needs
  Windows Server, IIS and a licence, and faults cannot be injected on
  demand. Personal Environments run on OutSystems Cloud, with no access to
  the logging database. A survey of a real logging database gives the
  realism this would give.
- **Replaying rows from the replica.** Realistic logs, but no database load
  behind them, no controlled faults, and real users' data in the test path.
- **A row generator without an application.** Cheaper, but logs and
  database load would be unrelated, which is the one thing that must be
  tested.
- **OpenTelemetry Demo, Online Boutique and similar.** They emit OTLP, which
  OutSystems 11 self-managed does not. For the OutSystems Cloud and ODC
  streams and OTLP traces, PatientZero gains an OTLP mode instead.
- **Its own repository, a dependency of this one.** It would keep
  Heartbeat's repository smaller, but the logging schema and the scenarios'
  expected signals (Heartbeat's metric names and queries) change together
  with the product: every such change would become two coordinated pull
  requests and a version bump. Revisit when PatientZero has users outside
  Heartbeat, its own release cadence, or grows into a general simulator; its
  own module, Dockerfile and chart, with no imports from Heartbeat's module,
  keep that move cheap.
- **Outside git (a local tool).** It produces the known answers the phase
  exit checks compare against, and CI must build it from the same commit, so
  it needs review and history like any test.
- **HammerDB or WideWorldImporters as the main target.** SQL Server load
  only, with no application logs and no exact expectations. HammerDB stays
  an option for probe-overhead soak tests.

## Consequences

- **Nothing from a real database reaches git but DDL and aggregates.** The
  repository is public. The fixtures' rows are synthetic; the traffic
  profile holds aggregates only.
- What the survey could not answer stays open: the installation's time zone
  (`log_timezone` remains a setting), the mobile request `Duration` unit,
  and how many requests lack an entry-point row. A survey of a live logging
  database answers them.
- Derived metrics are computed when rows are extracted, so backfilled rows
  land in Loki at their own times but in Prometheus all at once. Tests of
  baselines and weekly reports over days need elapsed time (soak runs) or a
  Prometheus TSDB backfill; phase 7 decides which.
- PatientZero follows the OutSystems schema the survey showed. Its DDL feeds
  both PatientZero and the collector's fixtures, so a schema
  change is made once: PatientZero builds its ten cycle tables and two views
  per log type from the fixtures' DDL at startup, so the fixtures are the
  only copy.
- PatientZero is the application onboarded in the end-to-end tests of
  phases 3 and 4, and the source of known answers in phase 7.
- Later modes, each when its phase needs it: OTLP logs and traces (OutSystems
  Cloud, ODC, the trace store), a MuleSoft-like source, an Oracle-backed
  variant.
