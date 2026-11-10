<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/brand/mark-dark.svg">
    <img src="docs/brand/mark.svg" alt="Atrium logo: a stone arch with one terracotta stone" width="112">
  </picture>
</p>

# Atrium

Atrium is an SRE-focused monitoring platform. It covers deep SQL Server
observability, application telemetry (OutSystems first), session-centric
investigation, adaptive alerting and operational reporting, built on Grafana,
Prometheus, Loki and OpenTelemetry. It complements commercial APMs rather than
replacing them.

Collection runs from a separate monitoring environment: **no agents are
installed on monitored database hosts.**

**Documentation: [docs/README.md](docs/README.md)**. Start with the
[product overview](docs/product/overview.md) and the
[architecture overview](docs/architecture/overview.md). The same docs are
published as an HTML site at <https://gdpais.github.io/Atrium/>, rebuilt from `master`
whenever its pages change; `make docs-site` builds it locally into `docs/site/`.

## Status

Phases 0 and 1 are done, phase 2 (OutSystems ingest) waits for its release,
phases 3 (the API) and 4 (inventory as code) passed their validations, and
production delivery (phase 5) has started; nothing runs in production yet.
See the [product overview](docs/product/overview.md#current-status), the
[roadmap](docs/product/roadmap.md) and [TODO.md](TODO.md).

| Component | Path | Status |
| --- | --- | --- |
| DB collector (SQL Server, OutSystems 11 logs) | `services/db-collector` | SQL Server probes, hot reload, failure isolation, readiness, secured endpoints, least-privilege login, counters and self-metrics; OutSystems 11 log extraction to Loki with derived metrics |
| Kubernetes delivery | `infra/helm`, `infra/kind` | One Helm chart for kind, CI and production: the DB collector and the API plus pinned Prometheus, Alertmanager, Grafana, Loki, Tempo and OTel Collector; kind workflow and acceptance tests; production (EKS, Argo CD) not built |
| Metadata schema | `db/migrations` | Migrated and tested; written by the API |
| Shared contracts | `packages/` | JSON schemas for config and telemetry payloads |
| API | `apps/api` | Login and roles, the inventory, telemetry sources, database targets and probe assignments rendered to the collector's config, Grafana deep links, audit records; applies the environment's inventory file; deployed by the chart with its PostgreSQL and Valkey |
| Session analyzer, reporting | `services/session-analyzer`, `services/reporting` | Not started |

## Quick start

Prerequisites: Docker, Go 1.27+, and kind, kubectl and Helm at the pinned
versions (`make tools-check` verifies them).

```bash
make chart-deps # fetch the pinned engine charts (once)
make kind-up    # create a local kind cluster and deploy the Helm chart
make health     # check the collector, Prometheus, Alertmanager and Grafana
make test       # Docker-free Go tests
```

Grafana runs at http://localhost:3001 and Prometheus at http://localhost:9090.
The default config monitors no databases. To collect real SQL Server metrics
locally, use the sandbox:

```bash
make sqlserver-dev-init
make sqlserver-dev-up
curl http://localhost:8082/metrics
```

First time? The [walkthrough](docs/guides/kind-walkthrough.md) takes you
through a full live test step by step. More in the
[local development guide](docs/guides/local-development.md):
profiles, tests, the kind acceptance checks and reloads; and in
[Kubernetes delivery](docs/guides/kubernetes-local.md). To monitor a real
database, see [onboarding SQL Server targets](docs/guides/database-targets.md).

## Architecture at a glance

Atrium has four planes: **telemetry** (collectors, Prometheus, Loki,
Alertmanager), **analysis jobs** (session analyzer, reporting), the **control
plane** (Go API, PostgreSQL, Redis, YAML config, the inventory as code) and
**presentation** (Grafana). The [core systems](docs/architecture/overview.md#core-systems) table
shows each plane's components and status.

Key boundaries: PostgreSQL stores product metadata only
([ADR 0001](docs/architecture/decisions/0001-postgresql-metadata-only.md));
collectors and integrations are configured in YAML/Kubernetes with hot reload
([ADR 0002](docs/architecture/decisions/0002-runtime-config-in-yaml.md)).

## Contributing

Work happens on short-lived `feature/*` branches merged into `master` by pull
request; releases are `vX.Y.Z` tags. See [CONTRIBUTING.md](CONTRIBUTING.md)
and the [changelog](CHANGELOG.md).

## Repository layout

```text
apps/
  api/                 control-plane API: OpenAPI spec, HTTP service, sqlc queries
services/
  db-collector/        SQL Server and OutSystems 11 log collector
  session-analyzer/    investigation and baseline worker (scaffold)
  reporting/           report worker (scaffold)
internal/config/       shared integrations config manager
internal/credentials/  shared credential lookup (ATRIUM_CREDENTIAL_<REF>)
packages/
  config-schema/       JSON schemas: integrations, inventory file, telemetry sources
  telemetry-contracts/ shared payload contracts
db/migrations/         PostgreSQL schema migrations, embedded in the API
config/                integrations.yaml for running a service outside Kubernetes;
                       inventory.example.yaml, an example inventory file
infra/helm/atrium/  Helm chart: workloads, engine config, rules, dashboards
infra/helm/values/     values per environment (kind, minimal, SQL Server sandbox,
                       production example, external platform example)
infra/kind/            local kind cluster config
infra/docker-compose.test.yml  disposable PostgreSQL fixture for integration tests
tests/                 repository, migration and integration tests
tools/docsite/         Markdown-to-HTML docs site generator (separate Go module)
tools/patientzero/     synthetic OutSystems 11 test application (separate Go module)
docs/                  documentation (index: docs/README.md)
```

Go services follow `cmd/<service>/main.go` plus `internal/<domain>/`. Empty
scaffold directories hold `.gitkeep` files until their service is implemented.

## CI

GitHub Actions runs the checks on every pull request and push to `master`;
`ci-ok` is the one required check ([CONTRIBUTING.md](CONTRIBUTING.md#ci)).
`docs-pages.yml` publishes the HTML docs, and `release-please.yml` and
`release.yml` publish tagged releases to GitHub Container Registry
([CONTRIBUTING.md](CONTRIBUTING.md#releases)).
