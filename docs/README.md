# Heartbeat Documentation

Start with the group for what you want to do. Every topic has one home; other
pages link to it instead of repeating it.

These pages are also published as an HTML site at
<https://gdpais.github.io/Heartbeat/>, rebuilt from `master` whenever its pages change. The Markdown
is the source; `make docs-site` builds the site locally into `docs/site/`,
which is not committed.

## By audience

**Start here (what Heartbeat is and where it stands)**

1. [Product overview](product/overview.md): what Heartbeat is, who it is for,
   its scope and current status
2. [Roadmap](product/roadmap.md): the phases, what each lets someone do, and
   what is done
3. [How we work](product/operating-model.md): phase outcomes, who decides
   what, scope, reversals and the measures the scorecard watches

**Run it (developers)**

- [Local development](guides/local-development.md): **how to run the app**
  (one command sequence), the SQL Server sandbox, the API, tests, image
  builds, troubleshooting
- [Walkthrough](guides/kind-walkthrough.md) (optional): a guided first run of
  the same commands, from installing the tools to breaking the database on
  purpose
- [Live demo](guides/live-demo.md) (optional): the collector and the
  telemetry path on a running stack, stage by stage, from the collector's SQL
  to Grafana and Alertmanager, with what to judge
- [Kubernetes delivery](guides/kubernetes-local.md): the Helm chart, values,
  network access, images and the kind cluster
- [Contributing](../CONTRIBUTING.md): branches, commits, checks, pull requests
  and releases; [changelog](../CHANGELOG.md)
- Test application: [PatientZero](../tools/patientzero/README.md), the
  synthetic OutSystems application Heartbeat is tested against, and its
  [fault scenarios and run journal](../tools/patientzero/scenarios/README.md)
- [TODO.md](../TODO.md): implementation task list, the source of truth for
  delivery order and status

**Operate it (operators and SREs)**

- [Installing a release](guides/install-from-release.md): install, upgrade
  and remove the published chart from GHCR, without a source checkout
- [Onboarding SQL Server targets](guides/database-targets.md): the
  collector's login, certificates, and adding a target through the API or by
  hand
- [Onboarding by pull request](guides/onboarding-by-pull-request.md): add,
  change or retire an application, its source and its database targets in
  the environment's inventory file

**Architecture (how it is built and why)**

1. [Architecture overview](architecture/overview.md): diagrams of the current
   runtime and the target platform, data ownership and the guardrails every
   subsystem follows
2. [Operator workflows](architecture/workflows.md): ingestion, session
   investigation, alerting, reporting and audit, end to end
3. [Data model](architecture/data-model.md): PostgreSQL schema and ownership
   rules
4. [Database observability](architecture/database-observability.md): SQL
   Server signal coverage, probe safety, and the
   [probe review checklist](architecture/database-observability.md#probe-review-checklist)
   to follow before adding a probe or approving a `query_template` override
5. [Application observability](architecture/application-observability.md):
   OutSystems 11 log extraction, derived metrics, SLOs, dashboards and request
   traces
6. [DB collector internals](../services/db-collector/README.md): runtime
   flow, failure isolation, the probe model and how rows become metrics

**Reference (lookup tables)**

- [Configuration reference](reference/configuration.md): `integrations.yaml`,
  the inventory file and environment variables
- [Metrics and endpoints reference](reference/metrics-and-endpoints.md):
  ports, health endpoints, the API, metric names, known gaps

**Architecture decisions (ADRs)**

- [Decision index](architecture/decisions/README.md): why things are the way
  they are, one record per decision; a replaced record is marked superseded,
  never rewritten

## By topic

| Folder | Contents |
| --- | --- |
| [`product/`](product/) | [Overview](product/overview.md), [roadmap](product/roadmap.md), [how we work](product/operating-model.md) |
| [`guides/`](guides/) | Task-oriented how-tos: running it ([local development](guides/local-development.md), [walkthrough](guides/kind-walkthrough.md), [live demo](guides/live-demo.md), [Kubernetes delivery](guides/kubernetes-local.md)) and operating it ([installing a release](guides/install-from-release.md), [SQL Server targets](guides/database-targets.md), [onboarding by pull request](guides/onboarding-by-pull-request.md)) |
| [`architecture/`](architecture/) | [Overview](architecture/overview.md), [operator workflows](architecture/workflows.md), [data model](architecture/data-model.md), [database observability](architecture/database-observability.md), [application observability](architecture/application-observability.md) |
| [`architecture/decisions/`](architecture/decisions/README.md) | Architecture decision records (ADRs), numbered, with their index |
| [`reference/`](reference/) | Lookup tables for [configuration](reference/configuration.md) and [metrics and endpoints](reference/metrics-and-endpoints.md) |
| [`archive/`](archive/README.md) | Superseded planning documents, kept for history |

Service internals live beside their code:
[`services/db-collector`](../services/db-collector/README.md) and the
API's contract, [`apps/api/openapi.yaml`](../apps/api/openapi.yaml).

Dated working documents (`docs/plans/`, `docs/reviews/`) are for decision
review and are not tracked in git. Their lasting outcomes are recorded as
[architecture decisions](architecture/decisions/README.md).

## Glossary

| Term | Meaning |
| --- | --- |
| **Collector** | A configured unit of collection in `integrations.yaml` (`collectors[]`), identified by a stable `id`, with a kind (`sqlserver` or `outsystems_logs`), scrape interval, probes and targets. The DB collector *service* runs one poller per enabled collector. |
| **Target** | One monitored database endpoint (host, port, database, credential reference) inside a collector. Becomes the `target` metric label. |
| **Probe** | A read-only SQL query from the built-in catalog (`waits`, `blocking`, …) whose result rows become Prometheus metrics. |
| **Probe assignment** | A probe assigned to a database target through the API or the inventory file. Approved assignments are rendered into the chart's `managedCollectors`, which the collector runs ([ADR 0014](architecture/decisions/0014-probe-assignments-to-runtime-config.md)). |
| **Credential reference** (`credential_ref`) | A pointer such as `env/finance-prod` that is resolved to a secret at runtime. Secret values are never stored in config or PostgreSQL. |
| **Config version** | Hash of the active `integrations.yaml`, exposed by health and admin endpoints. |
| **Environment** | The top ownership boundary (production, staging, …). Everything monitored belongs to exactly one environment. |
| **Application** / **component** | A business system operators care about, and its parts (modules, screens, workers). Application-owned records derive their environment from the application. |
| **Inventory file** | One YAML file per environment that declares its applications, components, telemetry sources, database targets and probe assignments. It is reviewed as a pull request and applied by the API, which reconciles PostgreSQL to it ([ADR 0021](architecture/decisions/0021-inventory-as-code-and-no-web-ui.md); [format](reference/configuration.md#inventory-file)). |
| **Telemetry source** | A configured feed of logs or metrics for one application (for example an OutSystems log export). |
| **Investigation** | A request to correlate evidence for a subject (user, IP, session, request) in an application over a time window. |
| **Evidence link** | A URL into Grafana, Loki or Prometheus that supports an investigation result; raw evidence is never copied. |
| **Adaptive baseline** | A computed, versioned threshold (for example rolling median + MAD) rendered into Prometheus rules. |
| **OTel gateway** | A Heartbeat normalization service that never got a caller, removed by [ADR 0009](architecture/decisions/0009-remove-the-otel-gateway.md). Not the stock **OpenTelemetry Collector**, which does OTLP ingest and routing. |
| **Plane** | One of Heartbeat's four areas (telemetry, analysis jobs, control plane, presentation); see [core systems](architecture/overview.md#core-systems). |
| **Control plane** | The Go API (`apps/api`) plus PostgreSQL, Redis and the YAML config and inventory files, which manage inventory, probe assignments, and later policies, schedules and investigations. |
| **Phase** | One step of the [roadmap](product/roadmap.md), defined by what it lets someone do. TODO.md calls them delivery *stages*, with the same numbers; its section numbers differ. |
| **ADR** | Architecture decision record, under [`architecture/decisions/`](architecture/decisions/README.md). |
| **PatientZero** | The synthetic OutSystems 11 application Heartbeat is tested and demonstrated against ([ADR 0010](architecture/decisions/0010-patientzero-test-application.md)). |
