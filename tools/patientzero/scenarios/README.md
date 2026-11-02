# PatientZero scenarios

A scenario is one test with an exact expectation
([ADR 0010](../../../docs/architecture/decisions/0010-patientzero-test-application.md)):
it injects a fault into the running system for a while and names the
signals Heartbeat must show for it. PatientZero runs it, checks the signals
against Prometheus, Loki and Alertmanager, and writes everything that
happened to a run journal, the known answer later investigations and
reports are compared with.

```bash
make patientzero-scenario                        # list the scenarios
make patientzero-scenario NAME=checkout-errors   # run one, with its load, and check its signals
make patientzero-scenarios                       # run every one in order, about 2.5 hours
```

`make patientzero-scenarios` runs them one after the other in a fixed order
(`SCENARIO_ORDER` in [`patientzero.sh`](../scripts/patientzero.sh)), goes on
after a failure and writes `.tmp/patientzero-scenarios/summary.md`;
`SCENARIOS="restarts loki-outage"` runs some. The SLO scenarios come first,
while the burn-rate windows hold no other fault, and `late-commits` last,
since its late-row alert keeps firing for 15 minutes; run it on a fresh
cluster, as CI does. CI runs it nightly on `master`, by hand, and on the
release pull request ([CONTRIBUTING](../../../CONTRIBUTING.md#ci)).

It needs PatientZero on kind (`make sqlserver-dev-up`, then
`make patientzero-up`). The command starts the k6 load profile the scenario
names (`SCENARIO_LOAD=0` skips it, for a load you run yourself), starts the
scenario through PatientZero's admin endpoint, prints each step and each
check as it settles, saves the journal to
`.tmp/patientzero-scenarios/<run>.jsonl`, and exits 0 only if every check
passed. Interrupting it stops the run, which removes its fault. For a
scenario with cluster faults it is also the run's host agent, which needs
`docker` ([cluster faults](#cluster-faults)).

| Scenario | Fault | Must show | Takes |
| --- | --- | --- | --- |
| [`checkout-errors`](checkout-errors.yaml) | Half of `PlaceOrder` calls fail for 3 minutes | The `PlaceOrder` error ratio in `heartbeat_outsystems_operations_total` above 0.3, the injected error rows in Loki, the ratio back below 0.2 after the fault | about 6 minutes |
| [`orderlist-latency`](orderlist-latency.yaml) | `OrderList` takes 3 s longer for 3 minutes | The screen p95 from `heartbeat_outsystems_operation_duration_seconds` above the 2 s SLO threshold, then back below 1 s | about 7 minutes |
| [`bad-deploy`](bad-deploy.yaml) | A broken version of `PatientZero_App` fails every `PlaceOrder` with a new exception for 8 minutes, then is rolled back | The module's error rate and the exception in Loki; the error SLI past 1.44%; `OutSystemsErrorBudgetBurnFast` firing, then resolved; `OutSystemsErrorBudgetBurnSlow` firing | about 19 minutes |
| [`slow-sql`](slow-sql.yaml) | The order list's query runs 2.5 to 3.5 s longer in SQL Server for 15 minutes | More running sessions on SQL Server; slow-SQL rows in Loki and `heartbeat_outsystems_slow_events_total`; the latency SLI past 14.4%; `OutSystemsLatencyBudgetBurnFast` firing, then resolved; `OutSystemsLatencyBudgetBurnSlow` firing | about 25 minutes |
| [`rotation`](rotation.yaml) | The weekly rotation of the logging database's views, while the load runs | Extraction keeps up; Loki holds every row of the run, once | about 8 minutes |
| [`log-backlog`](log-backlog.yaml) | Every request's log rows committed 10 s late for 4 minutes, within the collector's 15 s lag | Extraction keeps up; Loki holds every row of the run, once | about 8 minutes |
| [`late-commits`](late-commits.yaml) | Every request's log rows committed 90 s late for 3 minutes, past every log type's lag | The extracted rows stop, then return; `OutSystemsLateLogRows` firing and nothing sent twice; Loki holds every row of the run but the late ones, once, and `heartbeat_outsystems_late_rows_total` counted exactly the late ones | about 9 minutes |
| [`log-growth`](log-growth.yaml) | 20 extra general rows of 2,000 characters per request for 5 minutes, about 100 MB | The SQL Server storage probe sees `patientzero_log`'s data file grow; extraction keeps up; Loki discards nothing and holds every row, once | about 9 minutes |
| [`restarts`](restarts.yaml) | The db-collector, the OTel Collector and Loki restarted one after the other | No `HeartbeatServiceDown`, `OutSystemsLogExportBlocked`, `OutSystemsLateLogRows` or `OutSystemsDuplicateLogRows`, no discarded rows; Loki holds every row of the run, once | about 9 minutes |
| [`loki-outage`](loki-outage.yaml) | Loki down for 4 minutes | `heartbeat_outsystems_export_blocked` 1 while the target stays up; `OutSystemsLogExportBlocked` and `HeartbeatServiceDown` for Loki firing, then resolved; extraction catches up; Loki holds every row, once | about 8 minutes |
| [`otel-outage`](otel-outage.yaml) | The OTel Collector down for 4 minutes | `HeartbeatServiceDown` for it and `OutSystemsLogExportBlocked` firing, then resolved; extraction catches up; Loki holds every row, once | about 8 minutes |
| [`tempo-outage`](tempo-outage.yaml) | Tempo down for 4 minutes | `HeartbeatServiceDown` for it firing, then resolved; the target stays up, its export never blocks and extraction keeps up; Loki holds every row of the run, once | 8 min |
| [`collector-outage`](collector-outage.yaml) | The db-collector down for 4 minutes | No collector target up and `HeartbeatServiceDown` for it firing, then both back; extraction catches up; Loki holds every row, once | about 8 minutes |
| [`sqlserver-outage`](sqlserver-outage.yaml) | The sandbox SQL Server stopped for 2 minutes | Both collector targets down, then up again; Loki holds every row PatientZero could write, once | about 7 minutes |

The last six have cluster faults: `make patientzero-scenario` applies them
as the run's host agent ([cluster faults](#cluster-faults)). What the
scenarios leave out is under [not covered](#not-covered).

### SLO scenarios

`bad-deploy` and `slow-sql` fire the OutSystems SLO burn-rate alerts
([SLOs and alerts](../../../docs/architecture/application-observability.md#slos-and-alerts))
through the whole path: PatientZero's log rows, the collector's derived
metrics, the chart's recording rules and alerts, and Alertmanager. They run
against the rules as shipped, with their real windows and `for` durations;
nothing is shortened for kind. That sets their length:

- A fast burn (14.4 times the budget over 1 hour and 5 minutes, `for: 2m`)
  must fire during the fault, so that the run sees it resolve after it, and
  resolves about 5 minutes after the fault, once the 5-minute window has
  emptied. On kind it fired 3.5 minutes into `bad-deploy`'s fault and 13
  minutes into `slow-sql`'s, whose slowed requests are a smaller share and
  had more traffic before them in the 1-hour window.
- A slow burn (6 times the budget over 6 hours and 30 minutes, `for: 15m`)
  fires after at least 15 minutes: on kind 17.5 minutes after
  `bad-deploy`'s fault started, after its rollback, and 22 minutes after
  `slow-sql`'s. It resolves only once the 30-minute window has emptied, 40
  to 50 minutes after the fault started; the scenarios do not wait for
  that.
- The windows reach back before the run. PatientZero traffic in the hours
  before dilutes the fault's share of them, so each scenario first checks
  that there was not too much (`last-hour-leaves-room-for-the-fast-burn`,
  `history-leaves-room-for-the-slow-burn`) and that its alerts are quiet
  (`fast-burn-quiet-before`, `slow-burn-quiet-before`). A second run of the
  same scenario fails those checks until the first one's slow burn has
  resolved: wait about 50 minutes, or use a fresh cluster. `slow-sql` can
  run right after `bad-deploy`, whose run serves about 9,500 requests: their
  alerts differ.

PatientZero's steady load stays inside both objectives without a fault:
about 0.07% of its requests fail (refused orders) and none take 2 s.

### Not covered

No scenario fires these alerts from the chart's rules. The rules' unit tests
([`heartbeat.rules.test.yml`](../../../infra/helm/heartbeat/files/prometheus/rules/tests/heartbeat.rules.test.yml),
`make rules-check`) check each one against made-up series; no run shows it
firing from a real fault. A test keeps the table in step with the rules and
the scenarios.

| Alert | Fires when | Why no scenario fires it |
| --- | --- | --- |
| `Watchdog` | Always: it is the dead-man's switch | It fires through every run, but no check reads it |
| `HeartbeatCollectorConfigReloadFailed` | The db-collector rejects a config reload | No fault changes the collector's config |
| `HeartbeatCollectorLoginElevated` | A collector login is in `sysadmin` or holds `CONTROL SERVER` | No fault changes the login's grants |
| `OutSystemsDerivedSeriesOverflow` | A row needs a new derived series and the collector already holds `max_derived_series` | PatientZero's applications, modules and operations stay far below the budget |
| `OutSystemsLogExtractionBehind` | A log type has not been extracted for about 40 minutes (30, then its `for: 10m`) | The longest outage lasts 4 minutes |
| `OutSystemsLogRecountFailing` | Most recounts of a log type fail for 15 minutes | No fault breaks the recount for that long |
| `OutSystemsDuplicateLogRows` | A recount finds rows delivered twice | `late-commits` and `restarts` check that it stays quiet |
| `LokiDiscardingLogs` | Loki discards rows, for any reason but rate limiting | `log-growth` and `restarts` check that it stays quiet |
| `HeartbeatAPIErrorBudgetBurnFast` | The API fails over 1.44% of its requests over 1 hour and 5 minutes | The API is not deployed yet [4.1] |
| `HeartbeatAPIErrorBudgetBurnSlow` | The API fails over 0.6% of its requests over 6 hours and 30 minutes | The API is not deployed yet [4.1] |
| `HeartbeatAPISlow` | Over 72% of the API's requests take more than 250 ms over 1 hour and 5 minutes | The API is not deployed yet [4.1] |
| `HeartbeatCollectorCyclesLateFast` | Over 7.2% of a collector's cycles use their whole interval over 1 hour and 5 minutes, with at least 30 cycles an hour | No fault slows a probe past the interval that long |
| `HeartbeatCollectorCyclesLate` | Over 3% of them do over 6 hours and 30 minutes | As above, and no scenario lasts 6 hours |
| `HeartbeatExtractionFreshnessBurn` | A log type's lag stays more than 5 minutes past its setting, or its target down, for over 14.4% of an hour and of 5 minutes | The longest outage lasts 4 minutes |
| `HeartbeatExtractionFreshnessBurnSlow` | The same for over 6% of 6 hours and of 30 minutes | No scenario lasts 6 hours |
| `HeartbeatServiceRestarting` | A service starts more than 3 times in an hour | `restarts` restarts it once |
| `HeartbeatServiceMemoryHigh` | Resident memory stays above `GOMEMLIMIT` for 15 minutes | No fault grows the collector's memory that far |
| `HeartbeatGoroutinesGrowing` | Over 200 goroutines, growing by more than 20 an hour over 6 hours | No fault leaks goroutines |
| `HeartbeatOTLPExportRejected` | The OTel Collector rejects a request for good | No fault misconfigures the endpoint |
| `HeartbeatAuditDeliveryStalled` | The oldest audit record waits in the API's outbox for over 4 minutes (2, then its `for: 2m`) | The API is not deployed yet [4.1], and no scenario makes audited changes |

`HeartbeatServiceDown` is covered, but only one of its two rules fires for
the db-collector in a given cluster: `collector-outage` fires the `absent()`
rule where every name on Prometheus' DNS search path answers NXDOMAIN, as on
local kind, and the `up == 0` rule on GitHub's runners (the scenario's
`collector-target-down` check).

The `blocking` (with deadlocks), `slow-partner`, `timer-backlog` and
`bad-front-end` scenarios come in phase 7, with the analyzer and alert work
that uses them, and with what PatientZero does not do yet: timers,
extension calls, a consumed REST partner, long-running screens, and
`Instant` across daylight saving switches
([TODO 16.5](../../../TODO.md#165-patientzero-test-application)).

A backfill within Loki's 168 hours is not a scenario:
`make patientzero-scale-collect` and `make patientzero-scale-check` backfill
a generated log through the same extractor, Collector and Loki and check it
against its known answers ([PatientZero](../README.md#in-loki)).

## Format

One YAML file per scenario, named after it (`<name>.yaml`), in this
directory. The files are built into the PatientZero binary, which validates
every one at startup and refuses to start on an invalid file; `make test`
loads them too. [`scenario.schema.json`](scenario.schema.json) describes the
format for editors (the first line of each file points at it) and reviews.

```yaml
# yaml-language-server: $schema=scenario.schema.json
version: 1                 # format version
name: checkout-errors      # lower case, digits, dashes; the file's name
description: >-            # what it injects and what it must show
  ...
load: steady               # k6 profile run during the scenario: none, smoke, steady, peak, soak, day [steady]
steps:                     # one after another
  - name: baseline
    duration: 1m           # Go duration; at most 24h
  - name: errors
    duration: 3m
    fault:                 # optional: a step without one is a baseline or recovery
      type: errors         # a type from the fault registry
      params: {entry_points: [PlaceOrder], ratio: 0.5, message: checkout-errors}
signals:
  - name: error-ratio-rises
    promql: sum(rate(...{outcome="error"}[1m])) / sum(rate(...[1m]))
    condition: "> 0.3"
    step: errors           # the step the window is anchored on [the first step with a fault]
    from: start            # the window opens at the step's start or end [start]
    within: 4m             # how long it stays open; at most 6h
    mode: eventually       # eventually or always [eventually]
```

Each signal reads exactly one source:

| Field | Source | Value |
| --- | --- | --- |
| `promql` | Prometheus, `/api/v1/query` at the evaluation time | Every series of the instant vector, or the scalar |
| `logql` | Loki, `/loki/api/v1/query` at the evaluation time | Every series of a metric query. A log query (lines, no metric) is an error: count lines with `sum(count_over_time(... [2m]))` |
| `alert` with `state` and optional `labels` | Alertmanager, `/api/v2/alerts` | The number of active (not silenced or inhibited) alerts with that name and those labels |
| `delivery` | Loki, `/loki/api/v1/query`, against the rows PatientZero committed | Loki's lines of each log type in the rows' window ([delivery checks](#delivery-checks)) |

`condition` (query signals) is `present`, `absent`, or a comparison
(`>`, `>=`, `<`, `<=`, `==`, `!=` and a number). A comparison must hold for
every series the query returns, and never holds for none: write the query so
it returns the series you mean (`sum(...)` for one). `state` (alert signals)
is `firing` (at least one such alert active) or `resolved` (none).

A window opens at its planned time from the start of the run, but never
before its step's fault has actually started (`from: start`) or stopped
(`from: end`). A signal is evaluated every 10 seconds from the moment its
window opens, and once more when it closes. `eventually` passes at the first evaluation that
holds and fails if none did by the end of the window; `always` fails at the
first evaluation that does not hold (a query error counts) and passes if every
one held. A run ends when its last step has ended and every signal has a
result, so a signal anchored at the end of the last step can keep a run
going past its steps (`max_duration_seconds` in the listing).

### Delivery checks

A delivery check compares Loki with what PatientZero wrote: for each log
type, Loki must hold exactly the rows PatientZero committed with an
`Instant` from the start of one step to the end of another, no more (a row
stored twice) and no fewer (a row lost).

```yaml
  - name: every-row-once
    delivery:
      log_types: [screen, mobile_request, integration, general, error]
      from_step: before    # the rows' window opens at this step's start [the first step]
      to_step: after       # and closes at this step's end [the last step]
      lost_after: 30s      # rows a commit-delay fault held back longer must be missing []
    step: after            # the check's own window, as for any signal; it opens
    from: end              # at or after the end of to_step
    within: 4m
```

- **What was written.** PatientZero counts every row it commits, per log
  type and second of its `Instant`, as the logging database stores it (UTC,
  to the millisecond), and keeps the counts for 8 hours: the known answer.
  A row counts once its transaction commits, so rows a fault held back
  count when they are written, and rows that failed to write do not count.
- **What Loki holds.** One instant query per log type at the window's end:
  `sum(count_over_time({service_name="PatientZero", outsystems_log_type="<type>"} [<seconds>s]))`.
  Both sides count `(start, end]`, widened to whole seconds.
- **The check** passes at the first evaluation where every log type's two
  counts are equal, so it waits for the collector's lag (screen rows trail
  by a minute) and fails if Loki has not caught up by the end of its window.
  A row lost and another stored twice in the same log type would cancel
  out; the restart test of `make kind-e2e` counts distinct rows for that.
- **`lost_after`.** The collector reads each log type up to now minus its
  lag and never goes back, so a row committed later than that is never read
  ([ADR 0008](../../../docs/architecture/decisions/0008-outsystems-11-log-extraction.md)).
  With `lost_after`, the rows a `commit-delay` fault held back longer than it
  must be missing and the others present. It only splits the scenario's
  delays, so choose them so the split is certain: every delay below
  `lost_after` under the smallest lag (15 s for PatientZero's collector on
  kind), every delay above it longer than the largest lag plus its poll (1
  minute plus 15 s, for screen rows). A delay in between loses some rows and
  not others. A check also fails when no row was written in its window, and
  its mode is always `eventually`.
- **`late_rows`.** With `lost_after`, a counter selector such as
  `heartbeat_outsystems_late_rows_total{collector="patientzero"}`: per log
  type (its `log_type` label), the counter must also have grown by exactly
  the rows lost, from the window's start to the evaluation. The collector's
  recount counts the late rows, so this checks it finds each one, once.

The journal's `check` line records the rows' window and each log type's
counts.

## Fault registry

A fault type has a name, parameters and a **scope**, which says which runner
can apply it ([`internal/scenario/registry.go`](../internal/scenario/registry.go)).
PatientZero's runner applies `in-process` faults itself, and hands
`cluster` faults to the run's host agent.

| Type | Scope | Parameters | Does |
| --- | --- | --- | --- |
| `latency` | in-process | `delay` (required), `jitter`, `ratio` (0 to 1, default 1), `entry_points` | Delays `ratio` of the requests by `delay` plus up to `jitter` (together at most 1m), before their business SQL, so the delay is in the log rows' `Duration` |
| `errors` | in-process | `ratio` (required, 0 to 1), `message` (at most 200 bytes), `entry_points` | Fails `ratio` of the requests before their business SQL: an `error` row with `Message` `injected by scenario: <message>` under the request's `Request_Key`, linked from its other rows, and the entry point's failure response (HTTP 500) |
| `deploy` | in-process | `module` (required), `error_ratio` (0 to 1), `exception` (at most 200 bytes; .NET's `Object reference not set to an instance of an object.` by default), `delay` (at most 1m); `error_ratio`, `delay` or both | Publishes a new version of one module, as a broken publish does: its requests wait `delay` and `error_ratio` of them fail with an `error` row whose `Message` is the exception itself, and every error row of the module carries the new version in `EnvironmentInformation` (`eSpaceVer: 2 ...`). When the step ends, the previous version is published again |
| `sql-latency` | in-process | `delay` (required), `jitter`, `ratio` (0 to 1, default 1), `entry_points` | Makes `ratio` of the requests' business statement run `delay` plus up to `jitter` (together at most 1m) longer in SQL Server: the statement's batch waits (`WAITFOR DELAY`) before its first statement, before any lock is taken, so SQL Server holds the session running for that long |
| `rotation` | in-process | none | Rotates the logging database as the platform does every Friday at 23:45, when its step starts: in one transaction, every log type's previous view moves to the cycle table its current view read, and its current view to the next table. Rows stay where they are and new rows follow the current view. The end of the step changes nothing |
| `commit-delay` | in-process | `delay` (required, at most 10m) | Holds every request's log rows back for `delay` after the request, which answers at once, as a log service that has fallen behind commits them late. Rows held when the step ends are still committed `delay` after their request |
| `log-flood` | in-process | `rows` (required, 1 to 200), `bytes` (1 to 2,000, default 1,000), `entry_points` | Each request logs `rows` more `general` rows of its module, under its `Request_Key`, each with a `Message` of `bytes` characters, as verbose logging left on does |

`entry_points` names entry points as Heartbeat's `operation` label does:
`OrderList`, `PlaceOrder`, `GetOrders`; empty or absent for all three.
`module` names a module (eSpace): `PatientZero_Web` (`OrderList`),
`PatientZero_App` (`PlaceOrder`) or `PatientZero_API` (`GetOrders`). Two
faults on at once add up: delays sum, and a request fails if any fault
fails it.

`sql-latency` is time SQL Server spends, not PatientZero: the request is
active in SQL Server (the sessions probe counts its session as running, and
`sys.dm_exec_requests` shows it waiting in the business database), and the
request's log rows, and its slow-query row, carry it. What it does not
reproduce is the CPU and the reads of a real scan: making PatientZero's
small tables slow for real would take a production-sized table or the
laptop's CPUs, and SQL Server reports `WAITFOR` as an idle wait, which the
waits probe leaves out. Lock waits come with the `blocking` scenario
(phase 7).

**Slow-query rows.** PatientZero logs a business statement that takes more
than 200 ms as the platform logs a slow query: a `general` row under the
request's `Request_Key` with `Module_Name` `SLOWSQL` and the `Message`
`<statement> took <n> ms`, which the collector counts in
`heartbeat_outsystems_slow_events_total` and reads as the row's `Duration`.
It times the statement as it runs, so anything that slows it writes one,
not only a `sql-latency` fault.

A fault type implements `scenario.Fault` (`Start` and `Stop`) and is
registered with `Registry.Register`. `Stop` is called exactly once after a
successful `Start`, also when the run is stopped, with a fresh context. A
step's fault is built (its parameters checked) before the run starts, so a
bad parameter fails the start request, not half a run.

### Cluster faults

Restarts and outages of the db-collector, the OTel Collector, Loki and SQL
Server are `cluster` faults: they act on other workloads, which needs
Kubernetes RBAC (delete a pod, scale a StatefulSet) or the Docker host (stop
the SQL Server container, which runs outside the cluster). PatientZero has
neither, on purpose: a test application that can delete pods is a risk on
any cluster it is installed on.

| Type | Scope | Parameters | Does |
| --- | --- | --- | --- |
| `restart` | cluster | `workload` (required): `db-collector`, `otel-collector`, `loki` or `sqlserver` | When its step starts: deletes the workload's pod and waits until its StatefulSet's new pod is Ready, or restarts SQL Server's container and waits until it is healthy. The end of the step changes nothing |
| `outage` | cluster | `workload` (required), as for `restart` | For its step: scales the StatefulSet to 0 and waits until its pod is gone, or stops SQL Server's container; at the end, scales it back to 1 and waits until it is ready, or starts the container and waits until it is healthy |

The workloads are the Heartbeat chart's StatefulSets in the Heartbeat
namespace, one pod each, and the sandbox SQL Server container
(`SQLSERVER_CONTAINER`).

**The host agent.** A run with cluster faults is started with a host agent
attached: `make patientzero-scenario` starts it with `?agent=host` and
becomes that agent, on the host, with the kubeconfig and Docker it already
uses. PatientZero still runs the scenario: its steps and their clock, the
checks, the journal, the traffic it serves and the rows it commits, which
delivery checks compare Loki with. When a step's cluster fault starts or
stops, PatientZero's runner posts a request in the run's status
(`fault_request`: `id`, `action` `start` or `stop`, `type`, `params`) and
waits; the agent, which polls the status every 2 seconds, applies it with
`kubectl delete pod`, `kubectl scale` or `docker stop` and answers on
`POST /admin/run/faults/{id}`, with an `error` if it failed. The request
fails, and the run with it, if the agent has not answered within 5
minutes. Without an agent PatientZero refuses such a scenario (HTTP 422),
and the listing marks it `agent: true`.

The agent applies only these two types to these four workloads, for the
run it started. Interrupted, it stops the run detached (`POST
/admin/run/stop?detach=1`: PatientZero fails the pending and later requests
at once instead of waiting), then ends the outages it started. Killed
outright, it cannot: scale the StatefulSet back to 1 (`kubectl --context
kind-heartbeat -n heartbeat scale statefulset/<name> --replicas=1`) or start
the container (`docker start heartbeat-sqlserver-dev`) by hand.

A host-side runner, the same runner code on the host with the faults
applied directly, was the plan before this. It would have had to read the
traffic and the committed rows from PatientZero and reach Prometheus, Loki
and Alertmanager through port-forwards; with the agent, PatientZero stays
the one place a run is clocked, checked and journaled, and the host side
only does what needs its access.

## Admin endpoints

PatientZero serves the runner on its own port (8080), under `/admin/`.
Inside the cluster only the load Job's pods reach that port; the runner's
callers come through `kubectl port-forward`, which NetworkPolicy does not
filter ([who can reach it](../README.md#who-can-reach-it)). The admin
endpoints are behind a bearer token: `Authorization: Bearer <token>`,
compared in constant time with `PATIENTZERO_ADMIN_TOKEN`, as the
db-collector's admin endpoints do. The chart reads the token from a Secret (`admin.existingSecret`,
default `patientzero-admin`, key `admin-token`); `make patientzero-up`
creates it once with a random token. Without a token every admin request
gets 401. An unauthenticated request gets 401 before routing, so it learns
nothing about the routes or the runner. Scenarios and their queries come
only from the files built into the image; a request can only name one.

| Request | Answers |
| --- | --- |
| `GET /admin/scenarios` | The scenarios (steps, signals, fault types, load, `max_duration_seconds`, whether this runner can run each and why not) and the registered fault types |
| `POST /admin/scenarios/{name}/start` | 202 and the run's status; 404 unknown, 409 while another run is active (one at a time), 422 not runnable here. `?agent=host` attaches a host agent for its cluster faults |
| `GET /admin/run` | The active run's status (state, current step, each check's result so far, the cluster fault request waiting for the host agent), else the last run's, else `{"state": "idle"}` |
| `POST /admin/run/faults/{id}` | The host agent's answer to a cluster fault request, `{"error": ""}` when applied: 204; 404 if no such request is waiting |
| `POST /admin/run/stop` | Stops the active run and answers once it has ended: its fault removed, open checks `skipped`, the journal closed with `run_end` state `stopped`; 409 if none is active. `?detach=1` detaches the host agent first |
| `GET /admin/runs` | The journals on disk, newest first |
| `GET /admin/runs/{run}/journal` | One run's journal (`application/x-ndjson`), everything written so far while it runs |

By hand, through a port-forward:

```bash
kubectl --context kind-heartbeat -n patientzero port-forward svc/patientzero 8080 &
TOKEN=$(kubectl --context kind-heartbeat -n patientzero get secret patientzero-admin -o jsonpath='{.data.admin-token}' | base64 -d)
curl -s -H "Authorization: Bearer $TOKEN" localhost:8080/admin/scenarios | jq '.scenarios[] | {name, runnable}'
curl -s -X POST -H "Authorization: Bearer $TOKEN" localhost:8080/admin/scenarios/checkout-errors/start
curl -s -H "Authorization: Bearer $TOKEN" localhost:8080/admin/run | jq
```

PatientZero reads the signals at `PATIENTZERO_PROMETHEUS_URL`,
`PATIENTZERO_LOKI_URL` and `PATIENTZERO_ALERTMANAGER_URL`, which `make
patientzero-up` points at the Heartbeat namespace's services (chart values
`signals.*`). A scenario whose signals need a source with no URL is not
runnable.

## Run journal

One JSONL file per run: append-only, one event per line, never rewritten.
Its run id is the UTC start time, the scenario and four random hex digits,
`20261022T101500Z-checkout-errors-3f9a`, so the files sort by age.

**Where.** PatientZero writes it to `PATIENTZERO_JOURNAL_DIR`
(`/var/lib/patientzero/journal`, an `emptyDir` of at most 128 MiB in the
chart) and serves it on the admin endpoint; `make patientzero-scenario`
saves a copy under `.tmp/patientzero-scenarios/`. The file is written as
the run goes, so a stopped or crashed run still leaves what happened up to
that point. The `emptyDir` keeps the newest 20 journals across container
restarts, not across pods: the copy the caller saves is the lasting one. An
endpoint, not only a volume, because the image is distroless (no `kubectl
cp`) and a test or CI job needs the journal through the same port-forward
it already uses.

**Envelope.** Every line:

```json
{"v": 1, "seq": 7, "time": "2026-10-22T10:16:30.12Z", "run": "20261022T101500Z-checkout-errors-3f9a", "event": "fault_start", "data": {...}}
```

| Field | Meaning |
| --- | --- |
| `v` | Journal format version, 1. A change that renames or removes a field, or changes its meaning, makes it 2; new fields and events keep it |
| `seq` | 1, 2, 3, ... with no gap: a gap means a lost line |
| `time` | When the event happened, UTC, RFC 3339 with nanoseconds |
| `run` | The run id |
| `event` | One of the events below |
| `data` | The event's fields |

**Events**, in the order a run writes them (traffic and check lines
interleave with the steps):

| Event | When | `data` |
| --- | --- | --- |
| `run_start` | First line | `scenario`, `description`, `scenario_sha256` (of the definition below), `definition` (the scenario as run, defaults filled in), `load`, `host` (the pod), `sources` (`prometheus`, `loki`, `alertmanager` URLs), `summary_interval`, `poll_interval`, `planned_end` |
| `fault_start` | A step's fault is on | `step`, `step_index`, `type`, `scope`, `params` |
| `fault_end` | The fault is off | As `fault_start`, plus `reason` (`completed`, `stopped`, or `error` with `error`). A fault that failed to start has a `fault_end` with `error` and no `fault_start`, and ends the run |
| `traffic` | Every 15 s, then once more at the end with `scope` `interval`; then one with `scope` `run` | `scope`, `from`, `to`, `requests`, `failed` (requests that wrote an error row, refused orders included), `injected` (failed by a fault), `slow_sql` (slow-query rows written), `shed` (requests turned away with 503 at the in-flight limit: they wrote no log rows and are not among the `requests`), `rows` (log rows committed, per log type), `users` and `sessions` (distinct, anonymous callers not counted as users), `users_capped` / `sessions_capped` when the 50,000-per-interval or 200,000-per-run cap was reached, and `entry_points`: per entry point `requests`, `failed`, `injected`, `slow_sql`, `shed`, `anonymous`, `duration_ms_sum`, `duration_ms_max` |
| `check` | A signal has its result | `signal`, `kind` (`promql`, `logql`, `alert`, `delivery`), `query` (the expression, the alert name, or what a delivery check counts), `condition` or `state` and `labels`, `step`, `from`, `mode`, `within`, `window_start`, `window_end` (as the window actually opened), `result` (`pass`, `fail`, `skipped` when the run stopped before the window closed), `evaluated_at` (the deciding evaluation), `attempts`, `values` (that evaluation's values, sorted, at most 20; a delivery check's Loki counts in its log types' order), `series` (how many it returned), `error` (that evaluation's error, if any); for a delivery check also `rows_from` and `rows_to` (the rows' window as the steps ran) and `delivery`: per log type `log_type`, `expected`, `loki`, `written`, `lost` |
| `run_end` | Last line | `scenario`, `state` (`passed`, `failed` when a check failed, `stopped`, `error`), `error`, `seconds`, `checks` (`pass`, `fail`, `skipped` counts), `requests`, `failed`, `injected`, `slow_sql`, `shed`, `users`, `sessions` (the whole run) |

Traffic is counted as PatientZero serves it, from the request's start to
its response, by the same handlers that write the log rows: the known
answer for what Heartbeat should report. It is counted only while a run is
active.

```bash
jq -c 'select(.event == "check") | .data | {signal, result, values}' .tmp/patientzero-scenarios/<run>.jsonl
jq -c 'select(.event == "traffic" and .data.scope == "interval") | .data | {to, requests, injected, users}' .tmp/patientzero-scenarios/<run>.jsonl
```

## Adding a scenario

1. Write `<name>.yaml` here, with the schema line at the top.
2. Use a fault type from the registry, or add one: an in-process fault in
   [`internal/inject`](../internal/inject/inject.go) (registered by
   `inject.Register`), a cluster fault in
   [`internal/cluster`](../internal/cluster/cluster.go) and the host agent
   (`cluster_fault` in [`scripts/patientzero.sh`](../scripts/patientzero.sh)),
   with its parameters in the schema's `fault` section.
3. Read the expected signals from metrics and logs Heartbeat already
   produces; leave room for the collector's lag (about a minute; screen rows
   another minute) and the query's own range. A query's range also reaches
   back before the run: two runs back to back can see each other's rows, so
   keep ranges short (`[1m]`, `[2m]`).
4. `go -C tools/patientzero test ./...` validates the file and checks that
   the schema still matches the registry; then run it on kind.
