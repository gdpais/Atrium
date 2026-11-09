# Live Demo: Every Implemented Feature, Stage by Stage

A hands-on tour of what Atrium does today, for reviewing the project's
state before starting the next phase. It follows one SQL Server sample from
the query the collector sends (**extract**), through the probe catalog's
descriptors (**transform**), to `/metrics`, Prometheus, rules, Grafana and
Alertmanager (**load**). Then it breaks things on purpose and shows the
parts that are only scaffolds. About 60 minutes.

This page does not start anything: it inspects a stack started as in
[local development](local-development.md#run-everything). New to the stack?
Do the [walkthrough](kind-walkthrough.md) first. Every step names the
decision it exercises, so a surprise points at the ADR or TODO item to
revisit.
[What to judge](#what-to-judge) collects them at the end.

## 0. Load the helpers

Start the stack and the SQL Server sandbox as in
[Run everything](local-development.md#run-everything); if a step fails, see
[troubleshooting](local-development.md#troubleshooting). If Grafana runs on
another port there, use that port wherever this page says 3001.

In every terminal you use for the demo, from the repository root:

```bash
. scripts/demo-helpers.sh
```

[`scripts/demo-helpers.sh`](../../scripts/demo-helpers.sh) defines short
functions and changes nothing by itself. `hb_sql` and `hb_probe` run SQL as
the collector's own login, so you see exactly what the collector sees.

| Helper | Does |
| --- | --- |
| `hb_sql` / `hb_sql_sa` | Run SQL from stdin as the collector login / as `sa` (setup and workload only) |
| `hb_probe NAME` | Run probe `NAME` as the collector sends it, as its login |
| `hb_metrics REGEX` | Collector `/metrics` samples matching `REGEX` |
| `hb_prom 'PROMQL'` | Prometheus instant query, one line per series |
| `hb_admin [JQ ARGS]` | `GET /admin/config` through jq; default: target states |
| `hb_reload` | `POST /admin/config/reload` |
| `hb_logs [N]` | Last N collector log lines |
| `hb_k ARGS` | `kubectl` in the kind cluster's `atrium` namespace |

```bash
hb_admin
```

You should see `"status": "ready"` and the target `sqlserver-dev` in state
`ok`.

## 1. Extract: what the collector asks SQL Server

The probe catalog
([`catalog.go`](../../services/db-collector/internal/probes/sqlserver/catalog.go))
is the extractor: each probe is one SQL query plus metric descriptors. List
it:

```bash
go run ./services/db-collector/cmd/probe-catalog
```

```text
PROBE            METRIC                                        TYPE     COLUMN                  SCALE  LABELS
blocking         atrium_sqlserver_blocked_requests          gauge    blocked_count           1      blocking_session_id
...
storage          atrium_sqlserver_database_file_size_bytes  gauge    size_pages              8192   database_name,file_name,file_type
waits            atrium_sqlserver_wait_seconds_total        counter  wait_time_ms            0.001  wait_type
```

Print one probe's batch as it goes over the wire, then run it as the
collector:

```bash
go run ./services/db-collector/cmd/probe-catalog -sql sessions
hb_probe sessions
```

```text
SET LOCK_TIMEOUT 1000; SET DEADLOCK_PRIORITY LOW;
SELECT status, COUNT(*) AS session_count FROM sys.dm_exec_sessions GROUP BY status

status|session_count
------|-------------
running|1
sleeping|68
```

Every batch starts with the session settings: a probe gives up on a lock
after 1s and is always the deadlock victim. Run the other eight
(`waits blocking memory_pressure storage throughput cpu buffer_cache
file_io`) the same way. `blocking` returns no rows while nothing is blocked,
and `cpu` returns `NULL` for `other_process_percent` on Linux. Both matter in
the next step.

**Least privilege**
([database targets](database-targets.md#collector-login-permissions)). The
login sees server state but no application data:

```bash
echo "SELECT SUSER_SNAME() AS login, IS_SRVROLEMEMBER('sysadmin') AS sysadmin" | hb_sql
echo "SELECT name FROM sys.databases" | hb_sql
```

The first shows `atrium_collector|0`. Step 4 creates a database and shows
the login cannot read it.

## 2. Transform: rows into samples

The runner decodes each row with the probe's descriptors: `ValueColumn`
becomes the value, multiplied by `Scale`; `LabelColumns` become labels, plus
`environment` and `target`; a NULL or missing value makes no sample. Compare
the raw rows with what the collector exports:

```bash
hb_probe storage | head -4
hb_metrics 'file_size_bytes\{database_name="master"'
```

```text
master|master|ROWS|544
master|mastlog|LOG|256
atrium_sqlserver_database_file_size_bytes{database_name="master",...,file_name="master",file_type="ROWS",...} 4.456448e+06
```

544 pages × 8192 = 4,456,448 bytes. Check the other conversions the same
way:

| Raw (`hb_probe`) | Exported (`hb_metrics`) | Rule |
| --- | --- | --- |
| `memory_pressure`: `total_server_memory_kb` | `atrium_sqlserver_total_server_memory_bytes` | × 1024 |
| `waits`: `wait_time_ms` per `wait_type` | `atrium_sqlserver_wait_seconds_total` | × 0.001, counter |
| `cpu`: `sql_process_percent`, `other_process_percent` = `NULL` | only `atrium_sqlserver_cpu_sql_process_ratio` | × 0.01; NULL → no series |
| `file_io`: `num_of_reads` for `master`/`master` | `atrium_sqlserver_database_file_reads_total{database_name="master",file_name="master"}` | as is, counter |
| `throughput`: `batch_requests`, `transactions` (one row) | two counters | one row feeds two descriptors |
| `blocking`: no rows | no `atrium_sqlserver_blocked_requests` series | absent, not 0 |

Gauges move between two reads; counters only go up. Compare values from the
same moment, because the collector reads every 15s.

## 3. Load: from `/metrics` to Grafana and Alertmanager

```bash
hb_metrics '^atrium_sqlserver_' | wc -l                     # probe series: about 160 on a fresh sandbox
hb_metrics '^atrium_collector_target'                        # up, failures, last success, login_sysadmin
hb_prom 'count by (__name__) ({__name__=~"atrium_sqlserver_.*"})'
hb_prom 'time() - atrium_collector_target_last_success_timestamp_seconds'   # data age: under 30s (collection + scrape interval)
hb_prom 'histogram_quantile(0.95, sum by (probe, le) (rate(atrium_collector_probe_duration_seconds_bucket[5m])))'
```

Recording rules and alerts:

```bash
hb_prom 'atrium:sqlserver_sessions:sum'
hb_prom 'atrium:sqlserver_blocked_requests:sum'   # 0, not empty: the blocking probe is up and nothing is blocked
hb_prom 'topk(5, atrium:sqlserver_wait_seconds:rate5m)'
curl -s localhost:9090/api/v1/alerts | jq -c '.data.alerts[] | {alertname: .labels.alertname, state}'
```

Only `Watchdog` should be firing. Right after the collector pod is replaced,
`AtriumServiceDown` can show `pending` for the old pod's IP for about a
minute, until Prometheus's DNS discovery drops it, or, while no pod runs, for
the missing `db-collector` job. A replacement is back well within its 2
minutes, so it never reaches `firing`.

Grafana (`admin`/`admin`): open
<http://localhost:3001/d/sqlserver-overview?var-target=sqlserver-dev>.
All 14 panels should have data except Blocked Requests, which reads 0. The
same data through Grafana's API:

```bash
G=http://admin:admin@localhost:3001
curl -s "$G/api/search?type=dash-db" | jq -c '.[] | {uid, title}'
curl -sG "$G/api/datasources/proxy/uid/prometheus/api/v1/query" --data-urlencode 'query=atrium:sqlserver_sessions:sum' | jq -c '.data.result'
```

## 4. Make the database do something

Create a small application database as `sa`. The collector never writes, so
this is the only way to create load:

```bash
hb_sql_sa <<'SQL'
SET NOCOUNT ON;
IF DB_ID(N'atrium_demo') IS NULL CREATE DATABASE atrium_demo;
GO
USE atrium_demo;
IF OBJECT_ID(N'dbo.orders') IS NULL
  CREATE TABLE dbo.orders (id int IDENTITY PRIMARY KEY, customer int NOT NULL, amount decimal(10,2) NOT NULL, note char(500) NOT NULL DEFAULT 'x');
INSERT dbo.orders (customer, amount)
SELECT TOP (50000) ABS(CHECKSUM(NEWID())) % 1000, ABS(CHECKSUM(NEWID())) % 10000 / 100.0
FROM sys.all_objects a CROSS JOIN sys.all_objects b;
GO
SQL
echo "SELECT TOP 1 * FROM atrium_demo.dbo.orders" | hb_sql   # Msg 916: the collector cannot read it
```

Within 15s, `hb_metrics 'database_name="atrium_demo"'` shows the new files
(size and six I/O counters each). The Throughput, File I/O and Database File
Size panels move.

**Blocking.** One session holds a lock for 90s while three others wait for
it:

```bash
printf "USE atrium_demo; BEGIN TRAN; UPDATE dbo.orders SET amount = amount + 1 WHERE id <= 10; WAITFOR DELAY '00:01:30'; ROLLBACK;\n" | hb_sql_sa >/dev/null &
sleep 2
for i in 1 2 3; do printf "USE atrium_demo; SELECT COUNT(*) FROM dbo.orders WHERE id <= 10;\n" | hb_sql_sa >/dev/null & done
sleep 30
hb_probe blocking
hb_metrics blocked_requests
hb_prom 'atrium:sqlserver_blocked_requests:sum'
```

You should see a series per blocking session, `blocking_session_id` as a
label, and a total of 3 in the recording rule and in the Blocked Requests
panel. While it runs, look at the collector from the server side:

```bash
echo "SELECT session_id, login_name, lock_timeout, deadlock_priority FROM sys.dm_exec_sessions WHERE login_name = N'atrium_collector'" | hb_sql_sa
```

`lock_timeout` 1000 and `deadlock_priority` -5 confirm the session settings
from step 1. When the blocker rolls back (`wait` for the jobs), the series
disappear and the rule falls back to 0. The lock waits show up in
`atrium_sqlserver_wait_seconds_total{wait_type="LCK_M_S"}` only then:
SQL Server adds a wait to `sys.dm_os_wait_stats` when it ends.

```bash
wait
hb_prom 'sum by (wait_type) (rate(atrium_sqlserver_wait_seconds_total{wait_type=~"LCK.*"}[5m]))'
```

The collector produced **evidence** for the `blocking` and `sessions`
probes in every cycle, and the default sink discarded it: nothing records
which session blocked whom or with what statement. That is by design until
TODO §12.4.

## 5. Break things

Each scenario ends with the collector still `ready` (`curl -s
localhost:8082/readyz`) and never restarted (`hb_k get pod db-collector-0`
keeps its age and restart count).

### SQL Server stops

```bash
docker stop atrium-sqlserver-dev
sleep 25; hb_admin
hb_prom 'sum by (reason) (increase(atrium_collector_probe_errors_total[1m])) > 0'
docker start atrium-sqlserver-dev
```

The target becomes `failed` with the driver's error (`lookup
atrium-sqlserver-dev ... no such host`, since a stopped container leaves
the kind network's DNS). Errors count under `reason="error"` and the probe
series are cleared, not frozen. It returns to `ok` about 30s after the
start.

### SQL Server freezes

A paused container accepts nothing and answers nothing, the case the
[probe hard deadline](../../services/db-collector/README.md#failure-isolation-and-readiness)
exists for.

```bash
docker pause atrium-sqlserver-dev
sleep 50; hb_admin
hb_prom 'sum by (reason) (atrium_collector_probe_errors_total{target="sqlserver-dev"}) > 0'
hb_logs 30 | grep -E 'abandon|backoff'
docker unpause atrium-sqlserver-dev
```

The first probe times out after 7.5s (half the 15s interval) and is
abandoned 2s later. The rest of the target's probes are `not_started` while
that call is still pending, and the target backs off (17s, 33s, 71s, ...,
capped at 5m). Within about a minute the log shows `abandoned probe returned;
result discarded`, ended by the driver's 30s socket timeout (`i/o timeout`).
After `unpause`, the target recovers at its next attempt.

### A config that must not load

Helm's values schema rejects a probe timeout above 25s before anything is
deployed:

```bash
sed 's/^          - name: waits$/          - name: waits\n            timeout_ms: 30000/' infra/helm/values/sqlserver-dev.yaml > .tmp/demo-bad-timeout.yaml
EXTRA_VALUES=.tmp/demo-bad-timeout.yaml make kind-deploy   # fails: maximum: got 30,000, want 25,000
```

The collector's own loader is the second line of defence. Write a broken
config straight into the ConfigMap, bypassing Helm:

```bash
hb_k get configmap atrium-integrations -o json > .tmp/integrations-good.json
jq '.data["integrations.yaml"] = "platform: unsupported\ncollectors: [{id: broken, kind: sqlserver, enabled: true}]\n"' .tmp/integrations-good.json | hb_k replace -f -
until [ -n "$(hb_admin -r .last_reload_err)" ]; do sleep 3; done   # kubelet syncs the file in up to ~60s
hb_admin '{version, last_reload_err, status: .readiness.status, target: .readiness.collectors[0].targets[0].state}'
```

`last_reload_err` says `platform "unsupported" is not supported (supported:
grafana, generic)`; the version, the
readiness and the target are unchanged. Monitoring sees the rejection too:

```bash
hb_metrics '^atrium_collector_config_last_reload_successful'   # 0 until a reload succeeds
curl -s localhost:9090/api/v1/alerts | jq -c '.data.alerts[] | select(.labels.alertname == "AtriumCollectorConfigReloadFailed") | .state'   # pending, firing after 2m
```

Restore it as Helm's field manager,
so the next `make kind-deploy` does not conflict:

```bash
jq 'del(.metadata.resourceVersion, .metadata.uid, .metadata.creationTimestamp)' .tmp/integrations-good.json |
  hb_k apply --server-side --field-manager=helm --force-conflicts -f -
until [ -z "$(hb_admin -r .last_reload_err)" ]; do sleep 3; done
```

A valid change reloads without a restart: see the
[walkthrough](kind-walkthrough.md#5-change-the-config-without-a-restart).

### One probe fails, only that probe

A `query_template` override that divides by zero breaks only `blocking`:

```bash
sed 's/^          - name: blocking$/          - name: blocking\n            query_template: "SELECT 1 AS blocking_session_id, 1\/0 AS blocked_count"/' infra/helm/values/sqlserver-dev.yaml > .tmp/demo-broken-probe.yaml
EXTRA_VALUES=.tmp/demo-broken-probe.yaml make kind-deploy
sleep 90
for i in 1 2 3 4 5 6; do
  printf '%s %s exported=%s live=%s\n' "$(date +%T)" \
    "$(hb_admin -c '.readiness.collectors[0].targets[0] | {state, probes: [.probes[]? | {name, state, consecutive_failures}]}')" \
    "$(hb_metrics batch_requests_total | awk '{print $2}')" "$(hb_probe throughput | sed -n 3p | cut -d'|' -f1)"
  sleep 20
done
```

The target is `degraded`: `atrium_collector_target_up` stays 1 and the
eight working probes keep collecting, so `exported` follows `live` in every
cycle. Only `blocking` fails: `atrium_collector_probe_up{probe="blocking"}`
is 0, its series are gone, and `/admin/config` lists it under the target's
`probes`, `failed` on the cycles it runs and `backoff` while it waits out its
own backoff, with its consecutive failures growing.
`atrium:sqlserver_blocked_requests:sum` and the Blocked Requests panel go
empty (unknown), not 0. Restore with `make kind-deploy` and `hb_reload`.

### The collector login gains sysadmin

```bash
echo "ALTER SERVER ROLE sysadmin ADD MEMBER atrium_collector" | hb_sql_sa
hb_k rollout restart statefulset/db-collector && hb_k rollout status statefulset/db-collector
sleep 30
hb_metrics login_sysadmin                       # 1
hb_logs 30 | grep sysadmin                      # WARN ... sysadmin-equivalent
curl -s localhost:9090/api/v1/alerts | jq -c '.data.alerts[] | select(.labels.alertname == "AtriumCollectorLoginElevated") | .state'   # pending, firing after 15m
echo "ALTER SERVER ROLE sysadmin DROP MEMBER atrium_collector" | hb_sql_sa
hb_k rollout restart statefulset/db-collector
```

Without the restart, the check repeats about every 10 minutes.

### Admin endpoints

```bash
curl -s -o /dev/null -w '%{http_code}\n' localhost:8082/admin/config                                  # 401
curl -s -o /dev/null -w '%{http_code}\n' -H 'Authorization: Bearer wrong' localhost:8082/admin/config # 401
hb_admin '.config.CredentialRefs'                                                                     # every value <redacted>
```

### An application fails, on purpose

With [PatientZero](../../tools/patientzero/README.md) running
(`make patientzero-up`), a scenario breaks the application instead of the
database and checks that Atrium sees it:

```bash
make patientzero-scenario NAME=checkout-errors   # about 6 minutes
```

Half of the checkout's `PlaceOrder` calls fail for three minutes, each with
an error row in PatientZero's logging database. The command prints each
check as it settles: the `PlaceOrder` error ratio from the collector's
derived metrics rises above 0.3, the injected errors appear in Loki, and
the ratio falls back once the fault ends. Watch it in Grafana's *OutSystems
Errors* dashboard meanwhile. The run's journal, what PatientZero injected
and served every 15 seconds, is in `.tmp/patientzero-scenarios/`
([format](../../tools/patientzero/scenarios/README.md#run-journal)).

Two longer scenarios page in Alertmanager, through the chart's SLO
burn-rate alerts as shipped:

```bash
make patientzero-scenario NAME=bad-deploy   # about 19 minutes
make patientzero-scenario NAME=slow-sql     # about 25 minutes
```

`bad-deploy` publishes a broken version of the checkout module, and every
`PlaceOrder` fails with a new exception until it is rolled back:
`OutSystemsErrorBudgetBurnFast` fires within about 5 minutes and resolves
after the rollback, and `OutSystemsErrorBudgetBurnSlow` fires later. In
`slow-sql` the order list's query runs about 3 s longer in SQL Server: slow
SQL in the *OutSystems Overview* dashboard, more running sessions in *SQL
Server Overview*, and the latency alerts. Open Alertmanager on
<http://localhost:9093> meanwhile. Each waits for a slow burn that resolves
only 40 to 50 minutes after its fault started, so give a second run of the
same scenario that long
([SLO scenarios](../../tools/patientzero/scenarios/README.md#slo-scenarios)).

### Follow a broken deploy to its request

The scenario checks pass without anyone looking. This walk is what an SRE
does with the same fault: from the page to the failing request. Start
`bad-deploy` and open each dashboard with **Application** set to
`PatientZero` once the command prints `step broken`. The first checks
settle within about 4 minutes.

1. **OutSystems Overview** (`outsystems-overview`). *Failed requests* and
   *Error budget burn (1h)* climb, *Failed requests by application* shows
   only PatientZero, and the errors in *SLO burn rates* pass 14.4, the fast
   burn's threshold (the demo's run reached about 20).
   Latency does not move: this is an error fault, not a slow one.
2. **OutSystems Errors** (`outsystems-errors`). *Errors by module* shows
   only `PatientZero_App`. *Top errors* has one new row: `PlaceOrder` from
   the `Checkout` screen, with a fingerprint (the demo's run had
   `2b2acd8d67226a9c`). *Users affected* counts the shoppers who hit it.
   Click the fingerprint to filter the whole dashboard by it.
3. **Loki**. The same exception, with its request key:

   ```bash
   G=http://admin:admin@localhost:3001
   curl -sG "$G/api/datasources/proxy/uid/loki/loki/api/v1/query_range" \
     --data-urlencode 'query={service_name="PatientZero", outsystems_log_type="error"} |= "Object reference not set"' \
     --data-urlencode "start=$(($(date +%s)-300))000000000" --data-urlencode limit=1 |
     jq -c '.data.result[] | {Request_Key: .stream.Request_Key, error_fingerprint: .stream.error_fingerprint, Username: .stream.Username, line: .values[0][1]}'
   ```

   The fingerprint matches *Top errors*. In Grafana, *Explore* → Loki runs
   the same query, and *Recent errors* on the Errors dashboard lists the
   same rows.
4. **OutSystems Request Timeline** (`outsystems-request`). Paste the
   `Request_Key` into the **Request_Key** box. *Timeline of the request*
   shows the `mobile_request` row for `PlaceOrder` and its `error` row,
   joined by the error's ID. *Messages of the request* shows the log line
   with its stack (`at PatientZero_App.PlaceOrder()`).
5. **Alertmanager** (<http://localhost:9093>). `OutSystemsErrorBudgetBurnFast`
   fires during the fault and resolves after the rollback, and
   `OutSystemsErrorBudgetBurnSlow` fires later.

A slow screen goes the other way, through the latency dashboard:

```bash
make patientzero-scenario NAME=orderlist-latency   # about 7 minutes
```

For three minutes every `OrderList` screen takes 3 s longer. In
**OutSystems Latency** (`outsystems-latency`), *p95 by log type* and *p95
by module* show `screen` and `PatientZero_Web` above the 2 s line, and
*Duration distribution* shows a new band between 2 and 5 s. *Requests
slower than 2 s* lists each slow `OrderList` request with its user and
`Request_Key`, which opens it in the Request Timeline like step 4. The
p50, p95 and p99 tiles at the top cover every log type, so with the
default **Log type** `All` the integration calls keep them under 100 ms. Set
**Log type** to `screen` to see the slowdown there too.

Last walked on 2026-11-08, on kind at `13312e0`. `bad-deploy` passed its 11
checks (8,586 requests, 968 failed, 965 of them injected), and
`orderlist-latency` passed its 2.

## 6. Alert delivery

Alertmanager routes Watchdog to `deadmans-switch` and every other alert to
`default`. Neither has an integration locally
([ADR 0009](../architecture/decisions/0009-remove-the-otel-gateway.md)), so
alerts are visible in Alertmanager and Grafana but notify no one. Send a
synthetic alert and find it:

```bash
curl -s -X POST localhost:9093/api/v2/alerts -H 'Content-Type: application/json' \
  -d '[{"labels":{"alertname":"AtriumDemoAlert","severity":"info"},"annotations":{"summary":"live demo"}}]'
curl -s 'localhost:9093/api/v2/alerts?filter=alertname="AtriumDemoAlert"' | jq '.[] | {receivers: [.receivers[].name], state: .status.state}'   # default, active
```

## 7. What is deployed but not used yet

These checks come back empty or trivial today, and that is expected:

| Check | Result | Planned in |
| --- | --- | --- |
| Evidence from `blocking` and `sessions` | discarded by the default sink | Phase 7 (TODO §12.4) |
| `hb_k exec postgresql-0 -- psql -U atrium -Atc 'SELECT count(*) FROM investigations'` | `0`: the investigation, alerting and reporting tables wait for their workers | Phase 7 (TODO §4.6–4.8) |
| `hb_k get configmap atrium-prometheus-rules -o jsonpath='{.data.atrium-rendered\.rules\.yml}'` | `groups: []`: no rules are rendered from alert policies yet | Phase 7 (TODO §13) |

The API, its inventory and probe assignments are deployed and used, but this
page does not walk through them: see
[run the API](local-development.md#run-the-api) and
[onboarding SQL Server targets](database-targets.md#through-the-api).

## What to judge

| You saw | Decision it exercises | Revisit if |
| --- | --- | --- |
| Config changes applied with no restart; bad config rejected twice (Helm schema, then the loader) | [ADR 0002](../architecture/decisions/0002-runtime-config-in-yaml.md): runtime config in YAML, hot reload | Reload latency (30 to 75s here, mostly kubelet sync) is too slow for operators |
| One chart, same commands as CI | [ADR 0003](../architecture/decisions/0003-helm-on-kind-and-production.md) | The local loop is too heavy for your machine |
| Raw rows match exported samples; units and counter types as documented | Probe catalog and descriptors (TODO §2.3, §2.4) | A signal you need cannot be expressed as one query plus descriptors |
| `lock_timeout` 1000, `deadlock_priority` -5, no access to application data, sysadmin alert | Least privilege and query safety (TODO §2.2) | The grants are not acceptable to DBAs |
| Frozen server: collector stays ready, bounded stuck call | Hard deadline (TODO §2.6) | Recovery takes too long |
| One broken probe fails only itself; the target stays up and its other probes keep collecting | Per-probe failure isolation (TODO §2.6) | A broken probe goes unnoticed because its target stays up |
| `blocking_session_id` as a label | Probe label choice | Session IDs churn and create new series; the detail may belong in evidence instead |
| Collector evidence and the investigation, alerting and reporting tables unused | Phase 7 | Something you need now depends on them |

## Clean up

```bash
echo "DROP DATABASE IF EXISTS atrium_demo" | hb_sql_sa
make sqlserver-dev-down   # removes the container and its data
make kind-down
```
