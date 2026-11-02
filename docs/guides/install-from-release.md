# Installing a Release

How to install, upgrade and remove Heartbeat from a published
[GitHub release](https://github.com/gdpais/Heartbeat/releases), with Helm and
no source checkout. Each release publishes:

| Artifact | Where |
| --- | --- |
| Helm chart | `oci://ghcr.io/gdpais/charts/heartbeat`, version `X.Y.Z` |
| DB collector image | `ghcr.io/gdpais/heartbeat/db-collector:X.Y.Z`, for `linux/amd64` and `linux/arm64` |
| API image | `ghcr.io/gdpais/heartbeat/api:X.Y.Z`, for `linux/amd64` and `linux/arm64` |

All are public: no registry login is needed. The release notes list each
artifact's digest. The chart also installs pinned Prometheus, Alertmanager,
Grafana, Loki, Tempo and OpenTelemetry Collector from their own registries,
and the API's PostgreSQL and Valkey from their official images
([what the chart deploys](kubernetes-local.md#what-the-chart-deploys)). Those
two are for trying Heartbeat out: one replica each and no backups. Production
points the API at managed services instead
([production example](../../infra/helm/values/production.example.yaml),
[ADR 0015](../architecture/decisions/0015-postgresql-and-redis-delivery.md)).

To work on Heartbeat itself, use [local development](local-development.md)
instead. Production deploys the same chart from ECR through Argo CD
([ADR 0005](../architecture/decisions/0005-production-delivery-and-operations-defaults.md)).

## What you need

- A Kubernetes cluster. CI tests Kubernetes 1.36 on kind.
- Helm 4.2 and kubectl. The chart also renders with Helm 3, but installs are
  tested with Helm 4.
- A default StorageClass. The release asks for seven volumes, 23.25 GiB in
  all (listed under [Uninstall](#uninstall)).
- Memory: the pods request about 1.0 GiB, and their limits add up to about
  4.2 GiB.
- A CNI that enforces NetworkPolicy (kindnet, Calico, Cilium, Amazon VPC CNI
  with network policy on). Without one the chart's policies are accepted and
  drop nothing. To limit where the collector connects, set
  `dbCollector.networkPolicy.egress` with your databases' subnets, and keep
  its Service `ClusterIP`
  ([network access](kubernetes-local.md#network-access)).

The commands below use the namespace `heartbeat` and the release name
`heartbeat`. Any names work, but install one release per namespace: the
object names are fixed.

```bash
VERSION=X.Y.Z   # the release to install, without the v
```

## 1. Create the namespace and Secrets

The chart never creates Secrets
([ADR 0005](../architecture/decisions/0005-production-delivery-and-operations-defaults.md)),
so create these first. Without them the collector, API, PostgreSQL, Valkey and
Grafana pods wait in `CreateContainerConfigError` or `ContainerCreating`.

```bash
kubectl create namespace heartbeat
# Bearer token for the collector's admin endpoints.
kubectl -n heartbeat create secret generic heartbeat-runtime \
  --from-env-file=<(printf 'admin-token=%s\n' "$(openssl rand -hex 32)")
# Grafana's admin login.
kubectl -n heartbeat create secret generic grafana-admin \
  --from-env-file=<(printf 'admin-user=admin\nadmin-password=%s\n' "$(openssl rand -base64 24)")
# The bundled PostgreSQL's and Valkey's passwords (letters and digits), and
# the API's logins to both, as username:password.
POSTGRES_PASSWORD=$(openssl rand -hex 24) VALKEY_PASSWORD=$(openssl rand -hex 24)
kubectl -n heartbeat create secret generic heartbeat-postgresql \
  --from-env-file=<(printf 'password=%s\n' "$POSTGRES_PASSWORD")
kubectl -n heartbeat create secret generic heartbeat-valkey \
  --from-env-file=<(printf 'password=%s\n' "$VALKEY_PASSWORD")
kubectl -n heartbeat create secret generic heartbeat-api-credentials \
  --from-env-file=<(printf 'HEARTBEAT_CREDENTIAL_SECRET_API_POSTGRES=heartbeat:%s\nHEARTBEAT_CREDENTIAL_SECRET_API_REDIS=default:%s\n' "$POSTGRES_PASSWORD" "$VALKEY_PASSWORD")
unset POSTGRES_PASSWORD VALKEY_PASSWORD
```

PostgreSQL reads its password once, when it creates the database on its
volume; changing it later takes an `ALTER ROLE` as well as both Secrets.

The values reach `kubectl` through a pipe, not its command line. To read one
back: `kubectl -n heartbeat get secret grafana-admin -o jsonpath='{.data.admin-password}' | base64 -d`.
[Kubernetes delivery](kubernetes-local.md#secrets) lists every Secret the
chart reads and where kind and production get them.

## 2. Pin the release

Put your values in a file and keep it: every `helm upgrade` passes it again.
Start with the image digest from the release notes' *Published artifacts*
table:

```yaml
# heartbeat.yaml
dbCollector:
  image:
    # The sha256 after the @ in the db-collector row of the release notes.
    digest: sha256:<digest>
api:
  image:
    # The sha256 in the api row.
    digest: sha256:<digest>
```

Without them the chart runs `ghcr.io/gdpais/heartbeat/db-collector:X.Y.Z` and
`ghcr.io/gdpais/heartbeat/api:X.Y.Z`, the images of its own version. A digest
names exactly the build the release lists; a digest wins over a tag.

## 3. Install

```bash
helm install heartbeat oci://ghcr.io/gdpais/charts/heartbeat --version "$VERSION" \
  --namespace heartbeat -f heartbeat.yaml --wait --timeout 10m
```

Helm prints `Pulled: ghcr.io/gdpais/charts/heartbeat:X.Y.Z` and a
`Digest: sha256:...`; check it against the chart row of the release notes.
It then prints the chart's notes: the Secrets it needs, how to check the
install and how to reach Grafana. `helm show readme oci://ghcr.io/gdpais/charts/heartbeat --version "$VERSION"`
prints the chart's README.

If a value was wrong, fix it in `heartbeat.yaml` and upgrade with it:

```bash
helm upgrade heartbeat oci://ghcr.io/gdpais/charts/heartbeat --version "$VERSION" \
  --namespace heartbeat -f heartbeat.yaml --wait --timeout 10m
```

A collector pod that never became Ready is replaced by that upgrade; there is
no pod to delete by hand.

## 4. Check that it works

```bash
kubectl -n heartbeat get pods   # every pod Running, every container ready
kubectl -n heartbeat port-forward svc/db-collector 8082:8082 &
curl -s localhost:8082/readyz   # {"status":"ready"}
HEARTBEAT_ADMIN_TOKEN=$(kubectl -n heartbeat get secret heartbeat-runtime -o jsonpath='{.data.admin-token}' | base64 -d)
# The header goes to curl on stdin, so the token stays off its command line.
printf 'Authorization: Bearer %s\n' "$HEARTBEAT_ADMIN_TOKEN" | curl -s -H @- localhost:8082/admin/config |
  jq '{version, status: .readiness.status, warnings}'
kubectl -n heartbeat port-forward svc/grafana 3000:3000 &
# later: kill %1 %2 stops both port-forwards
kubectl -n heartbeat logs deployment/api -c migrate   # "migrate: schema up to date"
kubectl -n heartbeat rollout status deployment/api    # Ready: PostgreSQL migrated, Valkey answers
```

The admin call prints `version`, the SHA-256 of the active config, the
readiness `status`, and `warnings`, empty unless an insecure setting is on:

```json
{
  "version": "<64 hex characters>",
  "status": "ready",
  "warnings": []
}
```

Use free local ports. If something on your machine already holds
`127.0.0.1:8082` or `:3000`, `kubectl port-forward` listens on `[::1]` only,
without an error, and `localhost` may reach the other program. Then forward
another port, for example `18082:8082`, and use it in the URLs.

Then open http://localhost:3000 and log in with the `grafana-admin` Secret's
user and password. The *Heartbeat* folder holds the dashboards. They show
data once a SQL Server target is added (next step).

The API has no default user. Create the first admin in an API pod, with the
password on standard input, so it never reaches a command line or the shell
history ([`api users create-admin`](../reference/configuration.md#api)):

```bash
read -rs PASSWORD && printf '%s\n' "$PASSWORD" | kubectl -n heartbeat exec -i deployment/api -c api -- \
  /app/api users create-admin --email admin@example.com; unset PASSWORD
```

It logs the user's ID and its audit record's ID. The record reaches Loki
through the OTel Collector within seconds; in Grafana's Explore:
`{service_name="heartbeat-api"} | event_name="heartbeat.audit"`.

`/readyz` says whether the collector itself works, not the databases: it
stays ready while every SQL Server target fails
([endpoints](../reference/metrics-and-endpoints.md#db-collector-8082)).

| Symptom | Cause and fix |
| --- | --- |
| `CreateContainerConfigError` on `db-collector-0`, `valkey-0` or the Grafana pod | A Secret from step 1 is missing or lacks a key; `kubectl -n heartbeat describe pod <pod>` names it. Create it; the pod then starts by itself |
| The API pod in `Init:Error` or `Init:CrashLoopBackOff` | Its `migrate` init container could not apply the migrations. On a first install it fails a few times, about a minute in all, until PostgreSQL accepts connections, then succeeds by itself. Otherwise `kubectl -n heartbeat logs deployment/api -c migrate` says why, for example a password that does not match `heartbeat-postgresql` |
| The API pod `Running` but not ready | `kubectl -n heartbeat logs deployment/api -c api` has a `readiness changed` line naming the check that fails: PostgreSQL, the schema version, or Redis (Valkey) |
| `db-collector-0` stays `ContainerCreating` with a `FailedMount` event | The Secret named in `dbCollector.credentials.existingSecret` is missing. Create it ([step 5](#5-add-a-sql-server-target)); the pod then starts by itself |
| `postgresql-0` stays `ContainerCreating`, or the API pod `Init:0/1`, with a `FailedMount` event | `heartbeat-postgresql` or `heartbeat-api-credentials` from step 1 is missing: both are mounted as files. Create it; the pod then starts by itself |
| `ImagePullBackOff` or `ErrImagePull` | A digest in `heartbeat.yaml` is wrong, or the cluster cannot reach `ghcr.io` (or Docker Hub, `quay.io` for the engines) |
| Pods `Pending` with an unbound claim | No default StorageClass. Make one the default, or name a class for each claim in the values |
| `values don't meet the specifications of the schema` | A value is misspelt or out of range; the message gives its path. Unknown keys are rejected too, at the top level and inside `dbCollector`, the security contexts and the chart's other own components |

## 5. Add a SQL Server target

[Onboarding SQL Server targets](database-targets.md) is the reference: the
login and its grants, with the T-SQL to create it (step 1), the target entry
(step 2) and the credential (step 3). With a release install:

```bash
SQL_LOGIN=heartbeat_collector   # the login the DBAs created
# Its password, not echoed. The Secret key comes from credential_ref: env/finance-prod.
read -rs SQL_PASSWORD
kubectl -n heartbeat create secret generic heartbeat-sqlserver-credentials \
  --from-env-file=<(printf 'HEARTBEAT_CREDENTIAL_ENV_FINANCE_PROD=%s:%s\n' "$SQL_LOGIN" "$SQL_PASSWORD")
unset SQL_PASSWORD
```

In a second values file, `targets.yaml`, name that Secret and write the whole
`integrations.collectors` list as in
[step 2](database-targets.md#2-add-the-target), with the target under
`targets`. Lists replace rather than merge, so the list must be complete.

```yaml
# targets.yaml
dbCollector:
  credentials:
    existingSecret: heartbeat-sqlserver-credentials
integrations:
  collectors:
    - id: sqlserver-default
      # ... as in step 2 of the onboarding guide
```

Apply it with every values file, in the same order each time:

```bash
helm upgrade heartbeat oci://ghcr.io/gdpais/charts/heartbeat --version "$VERSION" \
  --namespace heartbeat -f heartbeat.yaml -f targets.yaml --wait --timeout 10m
```

Naming the Secret for the first time restarts the collector to mount it,
which ends the step 4 port-forward (`error: lost connection to pod`): start it
again. Later changes need no restart: the target list hot-reloads, and a key
added to the Secret, or a changed password, reaches the collector within about
a minute, because it reads the mounted files on every connection. Then check
the target as in [step 5](database-targets.md#5-verify).

The collector verifies the server's TLS certificate. SQL Server's default
self-signed certificate fails with `x509: certificate is not valid for any
names`, and a certificate from a private CA cannot be trusted yet
([certificates](database-targets.md#certificates)).

## Upgrade

Read the release notes first, and set the new digest in `heartbeat.yaml`.
Check that your values pass the new chart's schema, which touches nothing in
the cluster, then upgrade:

```bash
VERSION=X.Y.Z
helm template heartbeat oci://ghcr.io/gdpais/charts/heartbeat --version "$VERSION" \
  --namespace heartbeat -f heartbeat.yaml -f targets.yaml >/dev/null
helm upgrade heartbeat oci://ghcr.io/gdpais/charts/heartbeat --version "$VERSION" \
  --namespace heartbeat -f heartbeat.yaml -f targets.yaml --wait --timeout 10m --rollback-on-failure
```

Pass every values file you have, in the same order each time (`targets.yaml`
exists once you have done step 5), rather than `--reuse-values`, so the new
chart's defaults apply to everything you did not set.

`--rollback-on-failure` (`--atomic` in Helm 3) rolls a failed upgrade back to
the last revision that worked: Helm applies that revision's objects again and
deletes the ones the failed upgrade added. Without it, a failed upgrade is
left half done. Helm applies every object it can, keeps the objects the new
version removes, and marks the revision `failed`. Fix the cause and upgrade
again, or go back with `helm rollback heartbeat`.

Grafana keeps a provisioned dashboard after a release removes its file, as a
safeguard. The releases since 0.1.0 replace *Heartbeat Overview* with *Heartbeat
Service*; delete the old one by hand if you had it.

**From 0.1.0.** Do these once, before the first upgrade, in this order:

1. Update your values files. The release notes list the values renamed or
   removed since 0.1.0. Remove `otelGateway`: the OTel gateway is gone
   ([ADR 0009](../architecture/decisions/0009-remove-the-otel-gateway.md)),
   and unknown top-level values now fail. The `helm template` check above
   must pass before the next step, so that the upgrade cannot fail on the
   values after it. If you repoint `otlp_http/logs` at another platform, repoint
   `otlp_http/self-logs` too
   ([example](../../infra/helm/values/external-platform.example.yaml)), or the
   services' own logs go to a Loki that is not there.
2. Delete the collector's StatefulSet, leaving its pod running:

   ```bash
   kubectl -n heartbeat delete statefulset db-collector --cascade=orphan
   ```

   It now uses `podManagementPolicy: Parallel`, so it can replace a pod that
   never became Ready, and Kubernetes cannot change that field on an existing
   StatefulSet.
3. Run the upgrade above. It creates the StatefulSet again; the new
   StatefulSet adopts the running pod and replaces it with the new version.
   If it fails and rolls back, the rollback brings back the 0.1.0
   StatefulSet: repeat step 2 before the next attempt.

An upgrade attempted without step 2 fails with
`StatefulSet.apps "db-collector" is invalid: spec: Forbidden`. With
`--rollback-on-failure`, Helm then returns to 0.1.0; do steps 2 and 3. Without
it, Helm has applied everything else and removed nothing:

- the ConfigMaps are the new ones, `integrations.yaml` included;
- Grafana and Loki run the new version, and their pods restarted;
- the new NetworkPolicies, the `db-collector-state` and `otel-collector-queue`
  claims and the new OTel Collector StatefulSet exist, beside the old OTel
  Collector Deployment and the OTel gateway, which still run.

The 0.1.0 collector rejects the new config (`invalid candidate config` in
its log) and keeps collecting with its old one. Do not restart it: it reads
the new file at startup and would exit. Do steps 2 and 3. The second upgrade
completes the change and removes the old objects.

## Uninstall

```bash
helm uninstall heartbeat --namespace heartbeat
```

This deletes every object the release created, but not the claims Kubernetes
created for the StatefulSets' volume templates, nor what you created
yourself:

| Left behind | Size | Holds |
| --- | --- | --- |
| `PersistentVolumeClaim/storage-loki-0` | 10 GiB | The logs in Loki |
| `PersistentVolumeClaim/storage-tempo-0` | 2 GiB | The traces in Tempo |
| `PersistentVolumeClaim/storage-alertmanager-0` | 1 GiB | Alertmanager's silences and notification log |
| The step 1 Secrets and your credentials Secret | | Step 1 and step 5 |
| `Namespace/heartbeat` | | |

The other claims are deleted with the release, and their data with them:
`prometheus` (8 GiB, the metric history), `postgresql-data` (1 GiB, the API's
database), `db-collector-state` (256 MiB, the `outsystems_logs` watermarks)
and `otel-collector-queue` (1 GiB). A new install in the same namespace
reuses the three claims left behind, so Loki, Tempo and Alertmanager start
with their old data.

To remove everything:

```bash
kubectl -n heartbeat delete pvc storage-loki-0 storage-tempo-0 storage-alertmanager-0
kubectl delete namespace heartbeat   # the Secrets and anything else left in it
```

With the usual `Delete` reclaim policy, deleting a claim also deletes its
volume and data.
