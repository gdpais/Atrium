# Walkthrough: Run and Test Atrium on kind

A guided first run of the full stack on your machine, about 30 minutes.
Each step says what to run and what you should see. The
[local development guide](local-development.md) is the reference for the same
commands; [Kubernetes delivery](kubernetes-local.md) explains what the chart
deploys.

## 1. Prepare your machine

| Need | Version | macOS (Homebrew) |
| --- | --- | --- |
| Docker Desktop (or Docker Engine on Linux) | any current release; cgroup v2 | `brew install --cask docker` |
| kind | v0.33.x | `brew install kind` |
| kubectl | 1.35–1.37 | `brew install kubectl` |
| Helm | 4.2 or later | `brew install helm` |
| Go | 1.27+ | `brew install go` |
| jq, openssl, curl | any | `brew install jq` (openssl and curl ship with macOS) |

Docker settings:

- Give Docker at least **4 CPUs and 6 GB of memory**. The full profile uses
  about 2 GB, SQL Server about 1 GB, and image builds need headroom.
- On Apple silicon, enable **Use Rosetta for x86_64/amd64 emulation**. SQL
  Server only ships an AMD64 image.
- These ports must be free on 127.0.0.1: 3001, 4317, 4318, 8082, 9090, 9093
  and 11433.

Then, from the repository root:

```bash
make tools-check
```

Every line should start with `ok`. Any `FAIL` line says what to install or
change.

## 2. Start the platform

```bash
make chart-deps   # downloads the pinned Prometheus, Grafana, Loki and OTel charts
make kind-up      # creates the "atrium" cluster, builds and loads images, deploys
```

The first `make kind-up` takes several minutes while Docker pulls the node and
engine images. It ends by listing pods and services; all six pods should be
`1/1 Running`:

```text
alertmanager-0, db-collector-0, grafana-…, loki-0, otel-collector-0, prometheus-…
```

Check the endpoints:

```bash
make health
```

Each check prints a body; the command fails on the first one that does not
answer.

## 3. Look around

| Open | What you should see |
| --- | --- |
| http://localhost:3001 (Grafana, `admin`/`admin`) | Dashboards → **Atrium** folder with *Atrium Inventory*, *Atrium Service*, *SQL Server Overview* and the four OutSystems dashboards (empty until an `outsystems_logs` collector runs, for example with PatientZero); Connections → Data sources lists Prometheus, Loki and Tempo |
| http://localhost:9090/targets (Prometheus) | Seven jobs, all **UP**: alertmanager, api, db-collector, loki, otel-collector, prometheus, tempo |
| http://localhost:9090/alerts | **Watchdog** firing. It always fires; that is how a dead-man's switch knows alerting works |
| http://localhost:9093 (Alertmanager) | The Watchdog alert, received from Prometheus |
| http://localhost:8082/readyz | `{"status":"ready"}`; the details need the admin token (next section) |

So far the collector monitors nothing: the default config has no SQL Server
targets.

## 4. Add a SQL Server target

```bash
make sqlserver-dev-init   # once: writes .env.sqlserver-dev with random SA and collector passwords
make sqlserver-dev-up     # starts SQL Server in Docker, creates the collector login and its Secret, redeploys
```

The first run pulls the SQL Server image (about 1.5 GB). The container,
`atrium-sqlserver-dev`, runs next to the cluster, not in it, and the
collector reaches it by name. Within a minute:

```bash
curl -s -H "Authorization: Bearer local-admin-token" localhost:8082/admin/config |
  jq '{version, status: .readiness.status, targets: [.readiness.collectors[].targets[] | {name, state}]}'
```

shows the target `sqlserver-dev` with `"state": "ok"`. Then:

- In Prometheus, query `atrium_collector_target_up`: value `1` for
  `target="sqlserver-dev"`.
- In Grafana, open *SQL Server Overview* and pick `sqlserver-dev`. The panels
  fill in as samples arrive (15s scrape interval).

## 5. Change the config without a restart

The collector hot-reloads `integrations.yaml`. Note the current version, then
deploy a change (here, a 30s scrape interval):

```bash
curl -s -H "Authorization: Bearer local-admin-token" localhost:8082/admin/config | jq -r .version
mkdir -p .tmp
sed 's/scrape_interval: 15s/scrape_interval: 30s/' infra/helm/values/sqlserver-dev.yaml > .tmp/my-values.yaml
EXTRA_VALUES=.tmp/my-values.yaml make kind-deploy
```

Run the first command again until the version changes. It usually takes 10
to 60 seconds: kubelet updates the mounted ConfigMap, then the collector polls
it every 5s. Confirm the pod was not replaced: its AGE keeps counting.

```bash
kubectl --context kind-atrium -n atrium get pod db-collector-0
```

To apply a config change immediately instead of waiting:

```bash
curl -X POST -H "Authorization: Bearer local-admin-token" localhost:8082/admin/config/reload
```

`make kind-deploy` without `EXTRA_VALUES` puts the 15s interval back.

## 6. Break the database, not the collector

```bash
docker stop atrium-sqlserver-dev
```

Within about 20 seconds `GET /admin/config` shows the target as `"failed"`,
with the driver's error, but the collector itself stays `ready` (`/readyz`
answers 200), and Prometheus shows
`atrium_collector_target_up` at `0`. The collector is not restarted: a
database outage must never take down monitoring.

```bash
docker start atrium-sqlserver-dev
```

The target returns to `"ok"` within about 30 seconds of SQL Server accepting
connections.

## 7. Ship a code change

Change something in `services/db-collector` (even a log message), then:

```bash
make kind-images kind-deploy
kubectl --context kind-atrium -n atrium get pod db-collector-0 \
  -o jsonpath='{.spec.containers[0].image}{"\n"}'
```

The image tag (`atrium/db-collector:dev-<id>`) changes with the binary, and
the collector is replaced by a new pod. The old pod stops before the new one
starts, so the database is never polled twice.

## 8. Try the small profile

```bash
PROFILE=minimal make kind-deploy   # collector, Prometheus, Alertmanager, Grafana only
make kind-deploy                   # back to the full profile
```

## 9. Run the automated acceptance checks

```bash
make kind-e2e
```

This creates a second, temporary cluster (`atrium-e2e-<id>`) with its own
SQL Server, runs every check above and more (invalid config, rollouts,
persistence after pod replacement, an OTel Collector restart during two log
backlogs of 45 minutes and three hours), prints measurements, and deletes
what it created. Your `atrium` cluster is untouched. It takes about 10
minutes and ends with `All kind e2e checks passed`. CI runs the same script on every pull request.

## 10. Clean up

Before you clean up, the [live demo](live-demo.md) goes deeper on the same
cluster: the collector's exact SQL, raw rows against exported samples,
blocking, a frozen server and the parts that are not implemented yet.

```bash
make sqlserver-dev-down   # removes the SQL Server container and its data, redeploys without it
make kind-down            # deletes the cluster
```

## Troubleshooting

See [troubleshooting](local-development.md#troubleshooting) in the local
development guide.
