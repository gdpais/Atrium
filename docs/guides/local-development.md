# Local Development

How to run Heartbeat: the Heartbeat stack on a local kind cluster, the DB
collector pointed at a throwaway SQL Server, and the tests. This is the only
page you need to run the app. Two optional guides build on it:

| Guide | For |
| --- | --- |
| [Walkthrough](kind-walkthrough.md) | Your first run: installs the tools and shows what each step should look like |
| [Live demo](live-demo.md) | Reviewing what each feature does on a running stack, stage by stage |

Prerequisites: Docker, Go 1.27+, and kind, kubectl and Helm at the pinned
versions (kind v0.33, kubectl within one minor of Kubernetes 1.36, Helm 4.2+).
`make tools-check` verifies all of them and says how to fix any mismatch. Run
`make help` for the full target list.

## Run everything

From the repository root, the whole stack collecting from a local SQL Server:

```bash
make tools-check        # every line should start with ok
make chart-deps         # once, and after Chart.lock changes
make kind-up            # cluster, images, chart
make sqlserver-dev-init # once: random SA and collector passwords in .env.sqlserver-dev
make sqlserver-dev-up   # SQL Server container, collector login, redeploy against it
make health
```

Grafana is then at http://localhost:3001 (`admin`/`admin`). The sections
below explain each step; [Troubleshooting](#troubleshooting) covers the usual
failures. To stop: `make sqlserver-dev-down` and `make kind-down`.

## Start the platform

```bash
make chart-deps    # once, and after Chart.lock changes: fetch the engine charts
make kind-up       # create the cluster, build and load images, deploy
make health        # check the collector, Prometheus, Alertmanager and Grafana
make kind-down     # delete the cluster
```

`make kind-up` deploys the same Helm chart CI and production use
([ADR 0003](../architecture/decisions/0003-helm-on-kind-and-production.md)), with
[`kind.yaml`](../../infra/helm/values/kind.yaml) values. Two profiles:

| Profile | Deploys |
| --- | --- |
| `full` (default) | DB collector, the API with its PostgreSQL and Valkey (Redis), Prometheus, Alertmanager, Grafana, Loki, Tempo, OpenTelemetry Collector |
| `minimal` | DB collector, the API with its PostgreSQL and Valkey, Prometheus, Alertmanager, Grafana |

Choose one with `PROFILE=minimal make kind-up` (or `make kind-deploy`).
The API migrates its database before it starts; see [run the API](#run-the-api).

Services keep the ports Compose used, on 127.0.0.1 only: Grafana
http://localhost:3001 (`admin`/`admin`), Prometheus :9090, Alertmanager :9093,
collector :8082, OTLP :4317/:4318. The
[metrics and endpoints reference](../reference/metrics-and-endpoints.md#local-ports-kind)
lists them.

The default config has **no SQL Server targets**, so the collector runs but
collects nothing. Use the sandbox below, or
[configure a real target](database-targets.md).

How the chart, images and cluster fit together, and how to debug them:
[Kubernetes delivery](kubernetes-local.md).

## Daily loop

| You changed | Run |
| --- | --- |
| Go code in a service, or a migration | `make kind-images kind-deploy`; the API's pod applies new migrations before it starts |
| Chart templates or values | `make kind-deploy` |
| Prometheus rules or dashboards (`infra/helm/heartbeat/files/`) | `make kind-deploy` (rules reload in place; run `make rules-check` first) |
| The API's contract (`apps/api/openapi.yaml`) | `make generate`, and commit the regenerated code with it ([below](#change-the-api-contract)) |
| An API query (`apps/api/internal/store/queries/`) or a migration (`db/migrations/`) | `make generate`, and commit the regenerated code with it ([below](#change-a-query-or-a-migration)) |

Images are tagged by content (`dev-<image id>`), so a code change always rolls
out a new pod and a stale image cannot hide behind a reused tag. Every deploy
rebuilds first when the Go sources changed since the last build, for example
after a pull ([Images](kubernetes-local.md#images)).

The loop has a memory budget: the full profile holds at most 2.5 GiB of RAM
in the kind node. To check it, watch `docker stats heartbeat-control-plane`
(`<cluster>-control-plane` for another `KIND_CLUSTER`), take the peak over
30 minutes of `make patientzero-load`, and subtract PatientZero and its k6
Job, which run in the same node (`docker exec heartbeat-control-plane crictl
stats --label io.kubernetes.pod.namespace=patientzero`). A second cluster,
such as `make kind-e2e`, fits next to a sandbox that has run for hours only
while the profile stays near today's 1.85 GiB (2026-11-02, with Tempo
([what the telemetry costs](../reference/metrics-and-endpoints.md#what-the-telemetry-costs))
and the API, PostgreSQL and Valkey); at the budget it does not fit in a
7.6 GiB Docker host. Run it in CI, or
`docker restart heartbeat-sqlserver-dev` first: that frees the sandbox's
memory and keeps its databases. A change that would exceed the budget is
trimmed first, or asks for a new one
([ADR 0003](../architecture/decisions/0003-helm-on-kind-and-production.md)).

## SQL Server sandbox

A throwaway SQL Server runs as a Docker container on the kind network,
outside the cluster, and the collector is pointed at it.

```bash
make sqlserver-dev-init     # create the ignored .env.sqlserver-dev (random SA and collector passwords)
make sqlserver-dev-up       # start SQL Server, create the collector login and its Secret, deploy
make health
curl http://localhost:8082/metrics
make sqlserver-dev-down     # remove the container and its data; redeploy without it
```

| Piece | File | Tracked |
| --- | --- | --- |
| Env template | `.env.sqlserver-dev.example` | yes |
| Env file (random SA and collector passwords, mode 600) | `.env.sqlserver-dev` | no |
| Script that creates the collector login | `scripts/sqlserver-login.sh` | yes |
| Values pointing the collector at the container | `infra/helm/values/sqlserver-dev.yaml` | yes |

The container is named `heartbeat-sqlserver-dev`, uses `MSSQL_PID=Developer`,
publishes 1433 only on `127.0.0.1:11433`, runs the AMD64 image (emulated on
ARM hosts) and must pass a SQL health check before deploying. Its memory is
capped at 2048 MB (`SQLSERVER_MEMORY_LIMIT_MB`, at least 2048, SQL Server's
minimum); uncapped, SQL Server sizes itself to 80% of the Docker VM, and on an
8 GB Docker host a PatientZero load flood or a second kind cluster got it
killed. A container keeps the cap it was created with: `make sqlserver-dev-up`
warns when it differs, and `docker rm -f heartbeat-sqlserver-dev` followed by
`make sqlserver-dev-up` recreates it with the new one, keeping its databases,
which are on the `heartbeat-sqlserver-dev-data` volume. `make test-sqlserver`
caps its disposable container the same way. Once started,
later `make kind-deploy` runs keep the target until `make sqlserver-dev-down`.
The collector logs in as `heartbeat_collector`, which `make sqlserver-dev-up`
creates with only the
[documented grants](database-targets.md#collector-login-permissions); `sa` is
used only to set the container up. An `.env.sqlserver-dev` from before this
login existed has an `sa` collector credential, which `make sqlserver-dev-up`
refuses: run `make sqlserver-dev-init` (it adds the login and keeps the SA
password) and then `make sqlserver-dev-up` again. The collector trusts the
container's self-signed certificate
(`dbCollector.sqlserver.trustServerCertificate`), which is acceptable only
because the container is throwaway; production targets need a trusted
certificate.

### PatientZero

[PatientZero](../../tools/patientzero/README.md), the synthetic OutSystems
application Heartbeat is tested against, runs next to the chart and writes
into its own two databases on the sandbox; the collector reads its logging
database.

```bash
make patientzero-up         # needs make sqlserver-dev-up; adds PatientZero's login to .env.sqlserver-dev
make patientzero-scenario NAME=checkout-errors   # a fault scenario with its load; checks the signals
make patientzero-down       # remove it, its databases and its login
```

## Run tests

| Command | What it runs | Needs |
| --- | --- | --- |
| `make test` | First `make generate-check`: fails when the API's generated code differs from what its OpenAPI spec and its SQL queries generate. Then all Go unit and repository contract tests, including the docs generator's and PatientZero's modules | Go, a C compiler (sqlc's parser), network access the first time (the generators are fetched by `go run`) |
| `make test-race` | The same suite with the race detector | Go |
| `make vet` | Go static checks | Go |
| `make vulncheck` | `govulncheck` on all three Go modules; fails on a known vulnerability the code can reach, standard library included | Go, network access |
| `make rules-check` | promtool rule validation and unit tests | Docker |
| `make test-integration` | PostgreSQL migration and index tests; then the API against PostgreSQL and Valkey: golang-migrate applies `db/migrations` (again, concurrently, over a newer schema), the readiness check reads the schema version, credentials rotate, and `/readyz` follows each dependency going away and coming back (`-tags=integration`) | Docker (`postgres:18`, `valkey/valkey`) |
| `make test-sqlserver` | Every built-in probe against a disposable SQL Server, as a login with only the documented grants; fails on duplicate series or padded label values; checks the session settings and the sysadmin-equivalence flag for that login, `sa` and a `CONTROL SERVER` login. Every OutSystems log query against its fixture tables, views and rows, as a login with only `SELECT` on them, returning every row of the current and the previous week; and the extraction of `oslog_Error` to a fake OTel Collector across cycles, a restart and a weekly log table rotation, each row exactly once (`-tags=sqlserver`). Then PatientZero's live tests (`TestLive*`) as `sa`: its entry points write `oslog_*` rows that the collector's own queries read back, and a small production-shaped log (`make patientzero-scale`'s generator) loads, matches its known answers and is measured | Docker (SQL Server 2022) |
| `make chart-check` | `helm lint`, then renders every values profile and checks it (below); then lints and renders PatientZero's chart and checks that its NetworkPolicies admit port 8080 only from the load Job and its Service stays ClusterIP | Helm, `make chart-deps` |
| `make kind-e2e` | Acceptance checks on a temporary kind cluster (below) | Docker, kind, kubectl, Helm, jq |

CI (`.github/workflows/ci.yml`) runs all of them, and repeats `chart-check`
with Helm 3; on a pull request the slower ones run only when the change can
affect them ([CONTRIBUTING.md](../../CONTRIBUTING.md#ci)).

`make chart-check` renders the chart for each profile (defaults, kind,
minimal, SQL Server sandbox, production example) and fails if rendering is not
deterministic, an object is cluster-scoped or a Helm hook, the rendered
`integrations.yaml` fails the Go loader, the collector is not a single replica
with a writable state volume at `HEARTBEAT_STATE_DIR` and a read-only root
filesystem, a one-replica StatefulSet is not `Parallel`, a misspelt top-level
value or a misspelt key inside a component (`dbCollector.enabeld`,
`podSecurityContext.runAsNonroot`) renders, the notes `helm install` prints
miss a required Secret, or Prometheus, Alertmanager or Grafana points at a
Service the profile does not deploy.

`make kind-e2e` creates its own cluster (`heartbeat-e2e-<id>`, private
kubeconfig, no host ports) and SQL Server container, so it runs next to your
dev cluster without touching it, and removes only what it created. It checks
the ADR 0003 acceptance criteria: every workload ready; the running collector
is the image just built; the SQL target's metrics are fresh in Prometheus and
visible through Grafana, collected by a login that is not `sysadmin`; a failed
target is isolated; the Watchdog alert reaches Alertmanager; an
`outsystems_logs` collector, added by a hot reload, extracts a fixture
`oslog_Error` table to Loki and counts the rows in Prometheus; a valid ConfigMap
change hot-reloads without a restart; an invalid one (a misspelled probe) is
rejected while collection continues, shows in the reload metric and alert, and
clears with the next valid one; a SQL Server outage shows as a failed target without
restarting the collector; a collector update never runs two
collectors (it prints the rollout time, collection gap and SQL Server session
count); a deploy replaces a collector pod that never became Ready, and moves a
StatefulSet created with `OrderedReady` to `Parallel` without restarting a pod
whose template it does not change; and Prometheus data and Alertmanager
silences survive pod replacement.
It takes about 10 minutes.

### How integration tests stay isolated

Integration tests never touch your developer stack:

- Each test in `tests/` uses `infra/docker-compose.test.yml` (a disposable
  PostgreSQL fixture, not the platform) under a random `heartbeat-test-<id>`
  project, with no published ports and tmpfs storage.
- Only `db/migrations` is bind-mounted, read-only.
- `COMPOSE_*` overrides and dotenv files are ignored; your selected Docker
  daemon is kept.
- The API's tests (`apps/api`) connect from the test process, so each starts
  its own containers with `docker run` instead: a random
  `heartbeat-api-test-<id>` name, a random password passed through the
  environment, tmpfs storage, and one port published on 127.0.0.1 at a
  random port. PostgreSQL is the fixture's image, so both kinds of test run
  the same major.
- Cleanup is registered before startup and removes only that test's project
  or containers. Tests run concurrently, and separate test processes are
  isolated too.

If a test process is killed, find the leftover project with
`docker compose ls` and remove only that `heartbeat-test-<id>` project using the
test Compose file; find leftover API test containers with
`docker ps -a --filter name=heartbeat-api-test-` and remove them with
`docker rm -fv`.

## Build images

`make kind-images` builds the collector image for the kind node's own
architecture and loads it. To build by hand: the Dockerfile compiles for
Docker's selected target platform; the build stage runs natively and cross-compiles Go, so no CPU
emulation is needed.

```bash
docker build --platform linux/arm64 -t heartbeat/db-collector:dev -f services/db-collector/Dockerfile .
docker build --platform linux/amd64 -t heartbeat/db-collector:amd64 -f services/db-collector/Dockerfile .
```

BuildKit supplies `TARGETOS` and `TARGETARCH`; do not override them
separately from `--platform`. `.dockerignore` keeps everything but Go sources
out of the build context.

Released images are built by CI, not by hand: see
[releases](../../CONTRIBUTING.md#releases).

## Change collector config without restarting

Edit the `integrations` values and run `make kind-deploy`; for probe
assignments approved through the API, run `make kind-assignments`, which
renders them in the API's pod into `.tmp/kind/heartbeat/managed-collectors.yaml`
and deploys with it (later `make kind-deploy` runs keep it; delete the file
to drop them). The collector is not restarted: the ConfigMap reaches the pod within about a minute and the
collector polls the file every 5s. To apply it sooner:

```bash
curl -X POST -H "Authorization: Bearer local-admin-token" http://localhost:8082/admin/config/reload
```

`local-admin-token` is the kind-only value of the `heartbeat-runtime` Secret.
Credentials need no restart either: the collector reads the mounted
credentials Secret on every connection, and the kubelet updates it within
about a minute. See the [configuration reference](../reference/configuration.md#credential-resolution).

## Run the API

`make kind-up` deploys it with the chart's PostgreSQL and Valkey and their
Secrets, which `scripts/kind.sh` generates once per cluster. Its pods run
`api migrate up` in an init container first. It has no NodePort: reach it
through `kubectl port-forward`, which its NetworkPolicy does not filter, and
the database through `kubectl exec`:

```bash
kubectl --context kind-heartbeat -n heartbeat logs deployment/api -c migrate   # the migrations applied
kubectl --context kind-heartbeat -n heartbeat port-forward svc/api 8081 &
curl localhost:8081/readyz                                                      # {"status":"ready"}
kubectl --context kind-heartbeat -n heartbeat exec -it postgresql-0 -- psql -U heartbeat
# The first admin; its audit record reaches Loki as {service_name="heartbeat-api"}
(umask 077; openssl rand -hex 16 >admin-password)
kubectl --context kind-heartbeat -n heartbeat exec -i deployment/api -c api -- \
  /app/api users create-admin --email admin@example.com <admin-password
```

Log in as that admin through the same port-forward, from the origin the kind
values set (`http://localhost:8081`), as
[logging in and sessions](../reference/metrics-and-endpoints.md#logging-in-and-sessions)
shows.

On a new cluster the `migrate` init container fails a few times, about a
minute in all, until PostgreSQL accepts connections; Kubernetes retries it.
To start again from an empty database, delete the `postgresql-data` claim
and `postgresql-0`, then `make kind-deploy`; the same goes for a new
PostgreSQL major, which the chart does not upgrade
([ADR 0015](../architecture/decisions/0015-postgresql-and-redis-delivery.md)).

### Outside the cluster

For a faster loop with `go run`, run it on your machine against a PostgreSQL
and a Valkey container of your own. Host ports 55432 and 56379, on 127.0.0.1
only, stay clear of a local PostgreSQL or Redis:

```bash
export DEV_PASSWORD=$(openssl rand -hex 16)
POSTGRES_PASSWORD=$DEV_PASSWORD docker run -d --name heartbeat-api-dev-postgres \
  -p 127.0.0.1:55432:5432 -e POSTGRES_USER=heartbeat -e POSTGRES_DB=heartbeat -e POSTGRES_PASSWORD postgres:18.6
VALKEY_EXTRA_FLAGS="--requirepass $DEV_PASSWORD" docker run -d --name heartbeat-api-dev-valkey \
  -p 127.0.0.1:56379:6379 -e VALKEY_EXTRA_FLAGS valkey/valkey:9.1.3
until docker exec heartbeat-api-dev-postgres pg_isready -qh 127.0.0.1; do sleep 1; done

export HEARTBEAT_API_POSTGRES_HOST=127.0.0.1 HEARTBEAT_API_POSTGRES_PORT=55432 \
  HEARTBEAT_API_POSTGRES_SSLMODE=disable \
  HEARTBEAT_CREDENTIAL_SECRET_API_POSTGRES="heartbeat:$DEV_PASSWORD" \
  HEARTBEAT_API_REDIS_ADDR=127.0.0.1:56379 HEARTBEAT_API_REDIS_TLS=false \
  HEARTBEAT_CREDENTIAL_SECRET_API_REDIS="default:$DEV_PASSWORD"

go run ./apps/api/cmd/api migrate up   # apply db/migrations
go run ./apps/api/cmd/api              # serve on :8081
curl localhost:8081/readyz             # {"status":"ready"}

# The first admin, with the password on standard input
printf '%s\n' "$DEV_PASSWORD" | go run ./apps/api/cmd/api users create-admin --email admin@example.com
```

Without `HEARTBEAT_API_AUDIT_ENDPOINT` the audit records stay in the outbox,
where `docker exec heartbeat-api-dev-postgres psql -U heartbeat -c 'select record from audit_outbox'`
shows them. To see them delivered, point it at an OTLP/HTTP logs receiver,
such as `kubectl --context kind-heartbeat -n heartbeat port-forward svc/otel-collector 4318` on kind, with
`HEARTBEAT_API_AUDIT_ENDPOINT=http://127.0.0.1:4318`.

The defaults expect TLS on both (`verify-full` and `HEARTBEAT_API_REDIS_TLS=true`),
which local containers don't have, hence the two overrides. The API starts
without either dependency: `/readyz` answers 503 and the log's
`readiness changed` line says which check fails and why, for example
`schema not migrated` before `migrate up`. Every setting is in the
[configuration reference](../reference/configuration.md#api); the endpoints
are in the [endpoints reference](../reference/metrics-and-endpoints.md#api-8081).
Remove the containers with
`docker rm -fv heartbeat-api-dev-postgres heartbeat-api-dev-valkey`.

## Change the API contract

The API's contract is [`apps/api/openapi.yaml`](../../apps/api/openapi.yaml), written by hand
([ADR 0013](../architecture/decisions/0013-api-design-and-subsystem-boundaries.md)).
After editing it, regenerate the server types and commit them with the spec:

```bash
make generate   # writes apps/api/internal/openapi/openapi.gen.go
```

`make test` and CI, through `make generate-check`, fail when the committed
code differs from what the spec generates. The generator, oapi-codegen, is pinned in the Makefile
(`OAPI_CODEGEN_VERSION`) and fetched by `go run`, so there is nothing to
install. The handlers in `apps/api/internal/httpapi` implement the generated
`StrictServerInterface`: a new operation does not compile until it has one,
and a handler can only return the responses the spec declares.

## Change a query or a migration

The API's SQL is in [`apps/api/internal/store/queries/`](../../apps/api/internal/store/queries/schema.sql),
one `-- name: <Method> :one|:many|:exec` comment per query
([ADR 0013](../architecture/decisions/0013-api-design-and-subsystem-boundaries.md)).
sqlc reads the schema from `db/migrations` (the `.up.sql` files) and writes
typed Go for pgx into `apps/api/internal/store/*.gen.go`. Settings are in
[`apps/api/sqlc.yaml`](../../apps/api/sqlc.yaml). After editing a query, or
adding a migration that a query reads:

```bash
make generate   # also rewrites apps/api/internal/store/*.gen.go
```

As with the contract, `make test` fails while the committed code is stale,
and a query that no longer matches the schema fails `make generate`, not the
API at run time. sqlc is pinned in the Makefile (`SQLC_VERSION`) and built
by `go run`; its PostgreSQL parser is C, so it needs a C compiler (the Xcode
Command Line Tools on macOS) and takes about half a minute the first time.

A migration is a `NNNN_name.up.sql` and `NNNN_name.down.sql` pair in
[`db/migrations`](../../db/migrations), numbered next, four digits, and
written expand then contract ([CONTRIBUTING](../../CONTRIBUTING.md)). The API
embeds the files and `api migrate up` applies the new ones with
golang-migrate, recording the version in `schema_migrations`; in a cluster
an init container will run it before the API starts [4.1]. Until the
database has the newest version the API embeds, its readiness check fails,
so a replica of a new release never serves an old schema. An older release
started against a newer schema (a rollback) migrates nothing and is ready.

golang-migrate marks a version dirty while its migration runs. If the
migration fails, the version stays dirty, `migrate up` refuses to run, and
the readiness of a release that needs that version says so; an older
release, whose schema is complete, stays ready. PostgreSQL ran the file as one
implicit transaction, so none of it applied: fix the file, set the version
back with `update schema_migrations set version = <previous>, dirty = false`
(or `delete from schema_migrations` when it was the first), and run
`migrate up` again. `api migrate version` prints the version, with
`(dirty)` when it is.

## Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| `make tools-check` reports a wrong kubectl | Another kubectl (often Docker Desktop's) is earlier in `PATH`; put Homebrew's first |
| `make kind-up` fails with `port is already allocated` | Something else holds one of the ports in the [port list](../reference/metrics-and-endpoints.md#local-ports-kind) (often an old Compose stack: `docker compose ls`; find the holder with `lsof -nP -iTCP:<port> -sTCP:LISTEN`). Grafana uses 3001, so a Grafana on 3000 does not clash. Stop the holder and rerun, or move that port: delete the half-created cluster (`make kind-down`), then, for example, `sed 's/hostPort: 3001,/hostPort: 3002,/' infra/kind/cluster.yaml > .tmp/kind-cluster.yaml` and `KIND_CONFIG=.tmp/kind-cluster.yaml make kind-up`. `make health` still checks 3001, so check a moved Grafana with `curl localhost:3002/api/health` |
| kind node fails with a cgroup v1 error | kubelet 1.36 needs cgroup v2. Docker Desktop has it; on Linux, use a distribution with cgroup v2 |
| Pods stay `ErrImageNeverPull` | The images were not loaded into this cluster (for example after recreating it by hand). Run `make kind-images kind-deploy` |
| `make health` or the browser get "connection reset" or `Empty reply from server` right after Docker or the laptop restarts, or right after `make sqlserver-dev-up` | kube-proxy inside the node is still starting, or still routing the host port to the collector pod that was just replaced. Wait a few seconds (up to a minute after a restart) and rerun |
| Image pulls fail with `429 Too Many Requests` | Docker Hub's anonymous rate limit. `docker login`, wait, and rerun |
| SQL Server never becomes healthy on Apple silicon | Rosetta emulation is off in Docker Desktop, or Docker has too little memory |
| `make sqlserver-dev-up` says the credential must be a dedicated login, not `sa` | `.env.sqlserver-dev` predates the least-privilege login. Run `make sqlserver-dev-init` (it adds the login and keeps the SA password), then `make sqlserver-dev-up` again |
| `helm upgrade` fails with `resource StatefulSet/heartbeat/db-collector not ready ... 1 pods have failed` | The collector is crash-looping on the image or config just deployed; `kubectl --context kind-heartbeat -n heartbeat logs db-collector-0` says why. Fix it and rerun `make kind-deploy`: the StatefulSet replaces the broken pod (`podManagementPolicy: Parallel`) |
| `make kind-deploy` prints `recreating StatefulSet db-collector with podManagementPolicy Parallel` | Once, on a cluster deployed before the chart moved to `Parallel`, which cannot change in place: it deletes the StatefulSet with `--cascade=orphan` and the deploy creates it again. The pod keeps running until the new StatefulSet replaces it, which it does only if the deploy changes the pod template (after a `kubectl rollout restart` it does) |
| `helm upgrade` fails with a field-manager conflict | Something was edited by hand with `kubectl`. See [Kubernetes delivery](kubernetes-local.md#working-with-it) |
