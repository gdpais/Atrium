# Heartbeat Documentation

Start with the page for your role. Every topic has one home; other pages link
to it instead of repeating it.

These pages are also published as an HTML site at
<https://gdpais.github.io/Heartbeat/>, rebuilt from `master` on every push. The Markdown
is the source; `make docs-site` builds the site locally into `docs/site/`,
which is not committed.

## By audience

**New to Heartbeat (clients, stakeholders, new team members)**

1. [Product overview](product/overview.md): what Heartbeat is, who it is for,
   MVP scope, current status
2. [Roadmap](product/roadmap.md): phases and what is done
3. [How we work](product/operating-model.md): phase outcomes, who decides
   what, scope, reversals and the measures the scorecard watches
4. [Architecture overview](architecture/overview.md): diagrams of the current
   runtime and the target platform

**Developers**

- [Local development](guides/local-development.md): **how to run the app**
  (one command sequence), the SQL Server sandbox, tests, image builds,
  troubleshooting
- [Walkthrough](guides/kind-walkthrough.md) (optional): a guided first run of
  the same commands, from installing the tools to breaking the database on
  purpose
- [Live demo](guides/live-demo.md) (optional): every implemented feature on a
  running stack, stage by stage, from the collector's SQL to Grafana and
  Alertmanager, with what to judge
- [Kubernetes delivery](guides/kubernetes-local.md): the Helm chart, values,
  images and the kind cluster
- [Data model](architecture/data-model.md): PostgreSQL schema and ownership rules
- [Probe review checklist](architecture/database-observability.md#probe-review-checklist):
  before adding a SQL Server probe or approving a `query_template` override
- [Architecture decisions](architecture/decisions/README.md): why things are the
  way they are
- [Contributing](../CONTRIBUTING.md): branches, commits, checks, pull requests
  and releases; [changelog](../CHANGELOG.md)
- Service internals: [DB collector](../services/db-collector/README.md)
- Test application: [PatientZero](../tools/patientzero/README.md), the
  synthetic OutSystems application Heartbeat is tested against, and its
  [fault scenarios and run journal](../tools/patientzero/scenarios/README.md)
- [TODO.md](../TODO.md): implementation task list

**Operators and SREs**

- [Installing a release](guides/install-from-release.md): install, upgrade
  and remove the published chart from GHCR, without a source checkout
- [Onboarding SQL Server targets](guides/database-targets.md)
- [Configuration reference](reference/configuration.md): `integrations.yaml` and
  environment variables
- [Metrics and endpoints reference](reference/metrics-and-endpoints.md): ports,
  health endpoints, metric names, known gaps

## By topic

| Folder | Contents |
| --- | --- |
| [`product/`](product/) | [Overview](product/overview.md), [roadmap](product/roadmap.md), [how we work](product/operating-model.md) |
| [`architecture/`](architecture/) | [Overview](architecture/overview.md), [data model](architecture/data-model.md), [database observability](architecture/database-observability.md), [application observability](architecture/application-observability.md), [operator workflows](architecture/workflows.md), [decisions](architecture/decisions/README.md) |
| [`guides/`](guides/) | Task-oriented how-tos |
| [`reference/`](reference/) | Lookup tables for configuration, metrics and endpoints |
| [`archive/`](archive/README.md) | Superseded planning documents, kept for history |

Dated working documents (`docs/plans/`, `docs/reviews/`) are for decision
review and are not tracked in git. Their lasting outcomes are recorded as
[architecture decisions](architecture/decisions/README.md).

## Glossary

| Term | Meaning |
| --- | --- |
| **Collector** | A configured unit of collection in `integrations.yaml` (`collectors[]`), identified by a stable `id`, with a kind (`sqlserver`), scrape interval, probes and targets. The DB collector *service* runs one poller per enabled collector. |
| **Target** | One monitored database endpoint (host, port, database, credential reference) inside a collector. Becomes the `target` metric label. |
| **Probe** | A read-only SQL query from the built-in catalog (`waits`, `blocking`, …) whose result rows become Prometheus metrics. |
| **Credential reference** (`credential_ref`) | A pointer such as `env/finance-prod` that is resolved to a secret at runtime. Secret values are never stored in config or PostgreSQL. |
| **Config version** | Hash of the active `integrations.yaml`, exposed by health and admin endpoints. |
| **Environment** | The top ownership boundary (production, staging, …). Everything monitored belongs to exactly one environment. |
| **Application** / **component** | A business system operators care about, and its parts (modules, screens, workers). Application-owned records derive their environment from the application. |
| **Telemetry source** | A configured feed of logs or metrics for one application (for example an OutSystems log export). |
| **Investigation** | A request to correlate evidence for a subject (user, IP, session, request) in an application over a time window. |
| **Evidence link** | A URL into Grafana, Loki or Prometheus that supports an investigation result; raw evidence is never copied. |
| **Adaptive baseline** | A computed, versioned threshold (for example rolling median + MAD) rendered into Prometheus rules. |
| **OTel gateway** | A Heartbeat normalization service that never got a caller, removed by [ADR 0009](architecture/decisions/0009-remove-the-otel-gateway.md). Not the stock **OpenTelemetry Collector**, which does OTLP ingest and routing. |
| **Plane** | One of Heartbeat's four areas (telemetry, analysis jobs, control plane, presentation); see [core systems](architecture/overview.md#core-systems). |
| **Control plane** | The planned Go API plus PostgreSQL, Redis and YAML config, which manage inventory, policies, schedules and investigations. |
| **ADR** | Architecture decision record, under [`architecture/decisions/`](architecture/decisions/README.md). |
| **PatientZero** | The synthetic OutSystems 11 application Heartbeat is tested and demonstrated against ([ADR 0010](architecture/decisions/0010-patientzero-test-application.md)). |
