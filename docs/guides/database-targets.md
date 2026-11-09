# Onboarding SQL Server Targets

How to add, change or remove a monitored SQL Server database.

The collector reads its targets from its YAML config (`integrations.yaml`),
never from PostgreSQL, so onboarding a database is a config change
([ADR 0002](../architecture/decisions/0002-runtime-config-in-yaml.md)): either
the API renders it from approved probe assignments, or you write it by hand
([step 2](#2-add-the-target)). All keys are described in the
[configuration reference](../reference/configuration.md).

SQL Server is the only supported engine. Adding PostgreSQL, MySQL, Oracle or
MongoDB targets requires a new collector kind and connector in code, not just
config.

## 1. Get a login from the DBAs

Production targets use a dedicated observability login, never `sa` or any other
sysadmin login.

### Collector login permissions

The built-in probes need SQL Server 2012 or later (the newest feature they use
is `CONCAT`); CI runs them against SQL Server 2022 on Linux. They are read-only
and need:

| Permission | Needed for |
| --- | --- |
| `VIEW SERVER STATE` | `sys.dm_os_wait_stats`, `sys.dm_exec_requests`, `sys.dm_exec_sessions`, `sys.dm_os_performance_counters`, `sys.dm_os_ring_buffers`, `sys.dm_os_sys_info`, `sys.dm_io_virtual_file_stats`. Every built-in probe was verified as a login holding only `VIEW SERVER STATE` and `VIEW ANY DEFINITION`. SQL Server 2022's narrower `VIEW SERVER PERFORMANCE STATE` has not been verified on its own, so grant `VIEW SERVER STATE`. |
| `VIEW ANY DEFINITION` | `sys.master_files` for the `storage` and `file_io` probes. Without it, file sizes and file I/O are silently missing. |
| `VIEW ANY DATABASE` (granted to `public` by default) | Database names (`DB_NAME()`) in the `storage` and `file_io` `database_name` label. If it was revoked, files of databases the login cannot see are labelled `database_id:<id>` instead. |
| Access to the target's `database_name` | Used as the initial database |

No write, `db_owner` or `sysadmin` rights are required. `make test-sqlserver`
and `make kind-e2e` run every built-in probe as a login holding only these
grants. A `query_template` override runs with the same login; review it against
the [probe review checklist](../architecture/database-observability.md#probe-review-checklist),
and with the DBAs first if it needs more than these permissions. The collector
rejects an override that is not one read-only `SELECT`
([rules](../reference/configuration.md#collectors)), but that text check is
only a safety net: these read-only grants are what stop a probe from changing
anything, so never give the collector's login write rights.

For the DBAs, the T-SQL for such a login (SQL Server authentication; pick
your own name and a strong password):

```sql
USE master;  -- server permissions are granted from master
CREATE LOGIN atrium_collector WITH PASSWORD = N'<password>';
GRANT VIEW SERVER STATE TO atrium_collector;
GRANT VIEW ANY DEFINITION TO atrium_collector;

-- Check: both columns must be 0.
EXECUTE AS LOGIN = N'atrium_collector';
SELECT IS_SRVROLEMEMBER(N'sysadmin') AS is_sysadmin,
       HAS_PERMS_BY_NAME(NULL, NULL, N'CONTROL SERVER') AS has_control_server;
REVERT;

-- Only when the target's database_name is not master: a user there, with no roles.
USE FinanceDB;
CREATE USER atrium_collector FOR LOGIN atrium_collector;
```

[`scripts/sqlserver-login.sh`](../../scripts/sqlserver-login.sh) creates the
same login in the local sandbox, and refuses one that ends up
sysadmin-equivalent.

If the login is sysadmin-equivalent anyway (a member of `sysadmin`, or
holding `CONTROL SERVER`), the collector logs a warning (with the target and
`credential_ref`) and exports `atrium_collector_target_login_sysadmin` as
`1` ([metrics reference](../reference/metrics-and-endpoints.md#collector-self-observability)).
It checks when it first connects to the target and about every 10 minutes
after, so a change to the login's rights shows up within that time. If SQL
Server cannot tell, the collector logs that once per check and exports no
value. The `AtriumCollectorLoginElevated` alert fires when the metric has
been `1` for 15 minutes.

### Certificates

Connections use `encrypt=true` and verify the server's certificate. It must be
issued for the target's `host` and chain to a public CA in the collector's
image (Debian's `ca-certificates`). Otherwise every probe fails:
`x509: certificate is not valid for any names, but wanted to match <host>` for
SQL Server's default self-signed certificate, or
`x509: certificate signed by unknown authority`. Trusting a private CA is not
supported yet: the chart has no setting to add one to the collector.

The chart value `dbCollector.sqlserver.trustServerCertificate: true` (the
variable `ATRIUM_DB_COLLECTOR_SQLSERVER_TRUST_SERVER_CERTIFICATE`) turns
verification off. It is for the local dev container only: it applies to every
target, and the collector logs a warning at startup and lists it in
`GET /admin/config`.

### OutSystems logging database

An `outsystems_logs` collector reads the OutSystems 11 logging database with
its own login, which needs `SELECT` on the `oslog_*` views its log-type
queries read (`oslog_<TYPE>` and `oslog_<TYPE>_Previous`; the views and their
cycle tables share an owner, so no grant on the tables), access to that
database, and nothing else: no server permission. `make test-sqlserver` runs
every log-type query as such a login.
It runs the same login check and session settings as below. Settings are in
the [configuration reference](../reference/configuration.md#outsystems_logs-collectors).

### What the collector runs on the server

- At most 2 sessions per target, with the application name
  `AtriumDBCollector`.
- When it first connects to a target, and about every 10 minutes after, one
  `SELECT IS_SRVROLEMEMBER('sysadmin'), HAS_PERMS_BY_NAME(NULL, NULL,
  'CONTROL SERVER')`. It needs no extra permission.
- Each probe query, in one batch prefixed with
  `SET LOCK_TIMEOUT 1000; SET DEADLOCK_PRIORITY LOW;`. A probe waits at most
  1 second for a lock, then fails with error 1222 for that cycle (logged, the
  target is reported failed and retried on the next cycle). In a deadlock with
  an application session at the default (`NORMAL`) or a higher priority, SQL
  Server picks the collector as the victim; against another `LOW` session it
  picks the one cheaper to roll back, which for a read-only probe is usually
  the collector. `query_template` overrides get the same prefix, so a
  template must be a query, not a bare stored procedure name (see the
  [probe review checklist](../architecture/database-observability.md#probe-review-checklist)).

1 second is well below the default probe timeout (half the scrape interval, at
most 10 seconds), so a lock wait fails fast with a specific error instead of
holding a place in the lock queue, ahead of application requests, until the
probe deadline.

## 2. Add the target

There are three ways. The recommended one is the environment's inventory
file: see [onboarding by pull request](onboarding-by-pull-request.md). The
other two are through the API, or by hand in the values. On kind, the
inventory file in `kind.yaml` declares PatientZero's target, `patientzero`,
on the sandbox SQL Server
([local development](local-development.md#change-collector-config-without-restarting)).

### Through the API

Register the target at
[`/api/v1/database-targets`](../reference/metrics-and-endpoints.md#database-targets),
assign probes to it at
[`/api/v1/probe-assignments`](../reference/metrics-and-endpoints.md#probe-assignments),
and have an admin approve each assignment
([ADR 0014](../architecture/decisions/0014-probe-assignments-to-runtime-config.md)).
The API renders the approved ones into the chart's `managedCollectors`, one
collector per target; all of a target's assignments run at one interval.
On kind, `make kind-assignments` deploys the render, and later
`make kind-deploy` runs keep it. In production the pull request to
`atrium-deploy` that carries it waits for that repository [18]; until
then, add production targets by hand. Provide the credential (step 3) before
the deploy; an assignment shows `applied` once the collector runs it.

```bash
make kind-assignments
curl -sS -b cookies localhost:8081/api/v1/probe-assignments?state=applied | jq '.items[] | {id, collector_id}'
```

Do not add a target to both: the collector refuses a target that two
enabled collectors scrape in one environment, and keeps its last good
config (step 5 shows how to tell).

### By hand

Add an entry under `collectors[].config.targets[]` of the environment's
`integrations` values (the chart renders them into `integrations.yaml`; see
[config by environment](../reference/configuration.md#config-files-by-environment)).
Lists replace rather than merge across values files, so restate the whole
`collectors` list:

```yaml
collectors:
  - id: sqlserver-default
    kind: sqlserver
    enabled: true
    config:
      environment: prod
      scrape_interval: 30s
      probes:
        - name: waits
        - name: blocking
        - name: sessions
        - name: memory_pressure
        - name: storage
        - name: throughput
        - name: cpu
        - name: buffer_cache
        - name: file_io
      targets:
        - name: finance-prod          # becomes the `target` label; unique per collector
          host: finance-sql.internal
          port: 1433
          database_name: FinanceDB
          credential_ref: env/finance-prod
```

To run only some targets of a collector, list them in `config.target_names`.
Collector ids that start with `api-` are reserved for the
[collectors the API renders](../reference/configuration.md#collectors-rendered-by-the-api).

To **change** a target, edit its entry: `host`/`port` for a new server,
`database_name` for another database on the same server, `credential_ref` for a
new login. To **remove** one, delete the entry; the collector stops exporting
its series after the reload.

## 3. Provide the credential

`credential_ref: env/finance-prod` is read from the key
`ATRIUM_CREDENTIAL_ENV_FINANCE_PROD`, with the value `username:password`
([resolution rules](../reference/configuration.md#credential-resolution)).
Keep values out of version control.

Put the key in a Secret and name it in
`dbCollector.credentials.existingSecret`. The chart mounts that Secret into
the collector **container** as files, which the collector reads on every
connection: adding a key for a new target, or changing a password, needs no
restart. The kubelet updates the files within about a minute.

| Where the collector runs | How the Secret is created |
| --- | --- |
| kind | `kubectl -n atrium create secret generic <name> --from-literal=ATRIUM_CREDENTIAL_ENV_FINANCE_PROD=<user>:<password>` |
| kind SQL Server sandbox | Already wired: `make sqlserver-dev-up` creates it from the ignored `.env.sqlserver-dev` |
| Production | An External Secrets Operator `ExternalSecret` from AWS Secrets Manager (ADR 0005) |

## 4. Apply the change

| Where | Steps |
| --- | --- |
| kind | Put the values in a file and run `EXTRA_VALUES=<file> make kind-deploy`; for probe assignments, approved through the API or by the inventory file, `make kind-assignments`. |
| Installed from a release | Put the values in a file and run `helm upgrade` with it and all your other values files ([installing a release](install-from-release.md#5-add-a-sql-server-target)). |
| Production | Commit the values to the `atrium-deploy` repository; Argo CD syncs them. |

Helm rejects values that fail the integrations schema before anything changes.
A config-only change does not restart the collector: the ConfigMap reaches the
pod within about a minute (kubelet sync) and the collector picks it up through
file polling (5s). To apply it sooner, `POST /admin/config/reload` with the
admin token. A change to the credentials Secret needs no restart either: the
files reach the pod the same way, and the next connection uses them. An
invalid file is rejected and the previous config keeps running, so the change
has not taken effect: step 5 shows how to tell.

## 5. Verify

First check that the collector took the change. Helm accepting the values
does not mean the collector did: it also checks host names, credential
references, intervals and probe names.

```bash
curl -s http://localhost:8082/metrics | grep '^atrium_collector_config_'
```

`atrium_collector_config_last_reload_successful` should be `1`, and
`atrium_collector_config_info` should show a new `version`. At `0` the
change was rejected and the last good config still runs, and after 2
minutes Prometheus fires `AtriumCollectorConfigReloadFailed`, so a
rejection shows in alerting even when nobody runs this check
([config reload metrics](../reference/metrics-and-endpoints.md#config-reloads)).
The reason is `last_reload_err` in `GET /admin/config` and in the pod log; fix
the values and apply them again.

Then check the target:

```bash
# The header goes to curl on stdin, so the token stays off its command line.
printf 'Authorization: Bearer %s\n' "$ATRIUM_ADMIN_TOKEN" | curl -s -H @- http://localhost:8082/admin/config |
  jq '.readiness.collectors[].targets[] | select(.name == "finance-prod")'   # state, and the driver error if it fails
curl -s http://localhost:8082/metrics | grep 'atrium_collector_target_up{.*target="finance-prod"'
```

`atrium_collector_target_up` should be `1`, and
`atrium_sqlserver_*` series should appear with `target="finance-prod"`. In
Grafana, open the *SQL Server overview* dashboard and select the target.

If the target stays down, the target's `error` above shows the driver's
message (login, host or certificate); the collector's JSON logs have one line
per probe failure, with collector, target and probe. Repeated failures back off
exponentially up to 5 minutes, so a fix can take that long to show up unless
you reload.
