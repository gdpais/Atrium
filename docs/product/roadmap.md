# Heartbeat Roadmap

The single phase plan for Heartbeat. It summarizes [TODO.md](../../TODO.md),
which is the source of truth for delivery order and task status (TODO section
numbers in brackets). The core order is **DB collector → OutSystems ingest →
API/control plane → Web UI**; each phase is tested as it is delivered, using the
shared validation inventory [16] throughout rather than as a final stage.

| Phase | Theme | Status |
| --- | --- | --- |
| 0 | Foundations, including kind + Helm delivery | Done |
| 1 | DB collector (SQL Server) | Done |
| 2 | OutSystems ingest (OutSystems 11 log extraction) | At its exit: everything it builds is done ([ADR 0008](../architecture/decisions/0008-outsystems-11-log-extraction.md)), and the stage validation passed on 2026-11-02: the nightly run of every scenario has been green since 2026-10-28, and the 48 hour backfill matched its known answer in that workflow on 2026-11-02. Left: the outcome demo, and the release, which waits for the owner to create the release app [0.5] |
| 3 | API / control plane | In progress: the service bootstrap is done [4.1]: the chart deploys the API with its PostgreSQL and Valkey, migrating in an init container, its logs, metrics and traces wired like the collector's; audit records reach Loki through an outbox [4.10]; login, sessions, users and roles [4.2]; the inventory of environments, applications and components [4.3]; telemetry sources, with whether the collector observes them [4.4]; database targets, probe definitions and probe assignments, approved into `managedCollectors` and applied on kind [4.5]; next Grafana deep links [4.9], then the stage's validation. Planned into pull-request-sized slices [4]; decisions recorded in ADRs [0013](../architecture/decisions/0013-api-design-and-subsystem-boundaries.md)–[0017](../architecture/decisions/0017-outsystems-sources-map-application-identity.md); runs without waiting for phase 2's release |
| 4 | Web UI | Not started |
| 5 | Production delivery (EKS + Argo CD) | Not started; decisions recorded |
| 6 | Complete application and database product workflows | Not started |
| 7 | Investigations, alerting and reporting | Not started |
| 8 | Advanced reliability, HA and security | Not started |
| 9 | Fallback file ingestion | Deferred |
| 10 | Oracle collector, gated by an Alloy coverage check | Late roadmap |
| 11 | Custom collector vs Grafana Alloy comparison | Last planned phase |

## Phase outcomes

What each phase lets someone do. A phase is done when its outcome is shown
in a demo on real-shaped data, not when its checklist is ticked
([how we work](operating-model.md#outcomes-over-features)).

| Phase | Who | Can now |
| --- | --- | --- |
| 0 | Developer | Run the whole stack locally and in CI with one chart and one command sequence |
| 1 | DBA, SRE | See a SQL Server's waits, blocking, sessions, memory and storage in Grafana, with no agent on the database host |
| 2 | SRE | See an OutSystems application's health, errors and latency, with its logs, in Grafana, and be alerted when its objective burns |
| 3 | Operator | Onboard an application, a SQL Server target and its probes through the API, without editing YAML, with every change audited |
| 4 | Operator | Do the same in a browser |
| 5 | SRE, on-call | Run Heartbeat on EKS against production databases, with alerts reaching on-call |
| 6 | Application owner | Read an application's and its databases' health together, end to end |
| 7 | SRE | Ask what happened to a user, session or request in a time window and get correlated evidence; get low-noise alerts and a weekly report |
| 8 | SRE | Lose a collector or a node without a data gap beyond the agreed objective |
| 9 | Operator | Load exported logs where Heartbeat cannot reach the database |
| 10 | DBA | See Oracle as they see SQL Server |
| 11 | Owner | Decide, on evidence, whether to keep the custom collector or adopt Alloy |

## Test application: PatientZero

Heartbeat only reads, so the phases are tested against **PatientZero**, a
synthetic application that behaves like an OutSystems 11 application
([ADR 0010](../architecture/decisions/0010-patientzero-test-application.md)) [16.5].
Each request runs real SQL against its business database on the sandbox SQL
Server and writes the log rows the platform would into its own logging
database, so database load and application logs share a cause. Its logging
schema and traffic profile come from a survey of a real OutSystems 11
logging database, which is only read.

- k6 load with virtual users of stable identity, in steady, peak, soak and
  day profiles
- Named fault scenarios, each with the signals it must produce, run by
  `make patientzero-scenario`: weekly rotation, log backlog and growth, late
  commits, restarts and outages, a bad deploy and slow SQL now; blocking, a
  slow partner, late timers and a bad front end with phase 7
- A compressed calendar for the extractor: rotation on demand, backfill,
  late commits and time zones, without touching the collector's clock
- A journal of every run: the known answer for investigations and reports

It grows with the phases: logs and derived metrics in phase 2, onboarding in
phases 3 and 4, investigations, alerts and reports in phase 7.

## Phase 0 — Foundations (done)

- Monorepo scaffolding, local platform, CI [0.2]
- Shared JSON-schema contracts for config, telemetry, investigations, alerts and
  reports [0.3]
- PostgreSQL schema migrations and tests ([data model](../architecture/data-model.md)) [1]
- Prometheus, Loki, Grafana, Alertmanager and OTel Collector provisioning;
  Prometheus sends alerts to Alertmanager with a Watchdog route [9]
- Shared config manager with validation, versioning and hot reload; reload
  results exported as metrics and a rejected reload alerted on [8]
- One Helm chart for kind, CI and production; acceptance checks run on kind in
  CI; platform Compose and the Kustomize bundle retired
  ([ADR 0003](../architecture/decisions/0003-helm-on-kind-and-production.md)) [0.4]
- Trunk-based development and tagged releases: `v0.1.0` is public on GHCR
  and installs from the [install guide](../guides/install-from-release.md)
  ([ADR 0006](../architecture/decisions/0006-trunk-based-development-and-tagged-releases.md)) [0.5]

Open housekeeping: the release GitHub App and the tag-creation ruleset wait for
the end of phase 2
([ADR 0006](../architecture/decisions/0006-trunk-based-development-and-tagged-releases.md)) [0.5].

## Phase 1 — DB collector (done)

Runtime collection is configured in YAML and needs no API or UI.

- Probe catalog: waits, blocking, sessions, memory, storage, throughput, CPU,
  page life expectancy and buffer cache hit ratio, file I/O; per-target
  connection pooling and timeouts [2.1–2.3]
- Endpoint security: admin token on diagnostics and reload (constant-time
  check), status-only `/readyz`, full redaction and credential-free URLs,
  NetworkPolicy, `TrustServerCertificate` warning [2.7]
- Least privilege and query safety: every probe runs with `LOCK_TIMEOUT` and
  low deadlock priority; a sysadmin-equivalent login is logged, exported and
  alerted on; tests and the sandbox run as a login with only the documented
  grants; probe review checklist [2.2]; a `query_template` must be one
  read-only `SELECT`, checked when the configuration loads [2.7]
- Metrics: cumulative values as counters in base units, per-probe durations
  and error counters, Go runtime and process metrics [2.4, 2.6]
- Reliability: failure isolation, bounded backoff, freshness metrics,
  stale-series cleanup, readiness that reflects collector state, reload
  rollback, a hard deadline so a probe the driver cannot cancel never
  stalls the collector, and a row cap per probe (`max_rows`) [2.6]
- SQL Server overview dashboard and local SQL Server sandbox; every probe runs
  against a real SQL Server in CI, and `make kind-e2e` covers live SQL Server →
  collector → Prometheus → Grafana on kind, reloads and target outages with a
  least-privilege login

Not in this phase: API-driven probe assignments [2.5] join in phase 3, probe
versioning with them [4.5], and evidence publication in phase 7 [12.4]. A
failing probe fails only itself since 2026-10-27: its target stays up and
keeps collecting while the probe backs off on its own [2.6].

## Phase 2 — OutSystems ingest

OutSystems 11 self-managed first. Its logs are read from the logging database
by a new db-collector kind
([ADR 0008](../architecture/decisions/0008-outsystems-11-log-extraction.md);
design in [application observability](../architecture/application-observability.md)).

- PatientZero's first slice: the logging-database survey, its schema and
  traffic profile, the business and logging
  databases on the sandbox, k6 load, and the scenario runner with the ingest
  and SLO scenarios (rotation, backlog, late commits, log growth, restarts and
  outages, a bad deploy, slow SQL), which are part of this phase's exit
  checks; each ingest scenario compares Loki with the rows PatientZero
  committed. The other scenarios come with phase 7 [16.5]
- A persistent, ordered export queue in the OTel Collector: a Collector
  restart or a Loki outage during a backlog loses and reorders nothing; the
  db-collector waits for the Collector without failing its target [3.3]
- One reviewed `.sql` query per log type (error, screen, integration, ...),
  watermarked incremental reads, OTLP to the OTel Collector and Loki with
  OutSystems field names kept [3.3]
- User, session and request IDs and client IP stay searchable as Loki
  structured metadata, never labels [9.2]
- Derived SRE, infrastructure and business metrics on the collector's
  `/metrics`; recording rules and SLO burn-rate alerts [3.3]
- OutSystems overview, errors, latency and request-timeline dashboards [10.4, 3.5]
- Use configured application/environment identities until API inventory exists
- Later: request traces, Tempo with synthesized spans from `Request_Key`,
  built together once something consumes them: the first OTLP trace source or
  phase 7's investigation timeline [3.5, 3.6]; MuleSoft and
  generic sources [3.4]; OutSystems Cloud and ODC through their OTLP streams,
  straight into the OTel Collector (the OTel gateway was removed:
  [ADR 0009](../architecture/decisions/0009-remove-the-otel-gateway.md))

Exit: every query tested against a SQL Server fixture with the `oslog_*`
tables; logging database → collector → Loki and Prometheus → Grafana on kind,
including restarts and outages without gaps, shown by the PatientZero
scenarios in a scheduled CI run; the SLO scenarios fire the burn-rate alerts;
a 48-hour backfill matches its known answer in Loki. The phase ends with its
release: the release GitHub App is created then, and release-please cuts the
release from the commits since `v0.1.0` [0.5].

Done for the exit: the signal gaps the outage scenarios found (an alert when
the log export is blocked, `HeartbeatServiceDown` for a missing db-collector
or Loki, a late-row counter for rows committed past the `lag`, and the log
delay metric described as extraction freshness) [3.3, 9.1], and a CI workflow
that runs every scenario on kind nightly, by hand and on the release pull
request; the scenarios take over two hours, so they stay out of the per-PR
`make kind-e2e` [16.5].

Validated on 2026-11-02, with the nightly runs and `master`'s CI as evidence
([TODO 3](../../TODO.md#3-core-system-outsystems-ingest)): every log-type query
against SQL Server, extraction → Collector → Loki and derived metrics →
Prometheus on kind, restarts and outages without gaps, and the SLO scenarios
firing the burn-rate alerts. The first green nightly run was on 2026-10-28.
The 48 hour backfill matched its known answer in Loki on the current export
path in the scenarios workflow on 2026-11-02
([run](https://github.com/gdpais/Heartbeat/actions/runs/41538460130): all 48 hours in full, users, sessions and error fingerprints
exact), and runs after the scenarios from now on [16.5].

Left before the release, in order:

1. The outcome demo: an SRE sees a broken deploy's errors, latency and logs in
   Grafana and the burn-rate alert page. The nightly run checks signals in
   Prometheus, Loki and Alertmanager, not what Grafana shows, and the
   [live demo](../guides/live-demo.md) runs the scenarios without walking
   through the dashboards or the logs [16.5]
2. The release. **Owner:** create the release GitHub App, needed since
   2026-10-05 [0.5]

Not in the exit, and done alongside phase 3 before the first production
deploy: a series budget [2.6], collector
egress and exposure [2.7], unknown keys in component values [0.4],
Alertmanager's rollout policy [9.4] and PatientZero's hardening [16.5].

## Phase 3 — API / control plane

Decisions: API design and the frozen subsystem boundaries
([ADR 0013](../architecture/decisions/0013-api-design-and-subsystem-boundaries.md)), approved
probe assignments as pull requests to the config repository
([ADR 0014](../architecture/decisions/0014-probe-assignments-to-runtime-config.md)), PostgreSQL
and Redis delivery ([ADR 0015](../architecture/decisions/0015-postgresql-and-redis-delivery.md)),
audit records through an outbox to Loki
([ADR 0016](../architecture/decisions/0016-audit-records-through-an-outbox.md)),
OutSystems sources as application identity mappings
([ADR 0017](../architecture/decisions/0017-outsystems-sources-map-application-identity.md)).
Its prerequisites are done, so it starts while phase 2's release waits for
the release app.

In order, one pull request each:

- Bootstrap, PostgreSQL and Redis connectivity, migrations, and the API,
  PostgreSQL and Valkey in the chart [4.1] (done)
- Audit output, first, so every later change is audited [4.10] (done)
- Local auth with minimal roles: login, sessions, users and roles [4.2] (done)
- Environments, applications, components, telemetry sources, database targets,
  probe definitions and assignments; approved assignments become
  YAML/Kubernetes runtime config through the `managedCollectors` chart key,
  applied on kind by `make kind-assignments`; the collector reads credentials
  from mounted Secret files, so a new target needs no restart [4.3–4.5, 2.5]
  (done)
- Grafana deep links from the integrations templates [4.9] (done): the API
  reads the collector's `heartbeat-integrations` ConfigMap and renders
  dashboard and deep-link templates for applications and database targets;
  no log-search endpoint until investigations need the query adapters

The pull requests to `heartbeat-deploy` (ADR 0014) wait for that repository
in phase 5. Investigation, alerting and reporting APIs and their job
contracts move to phase 7 and are built with their workers [4.6–4.8]: nothing
consumes them before, and their shape depends on the correlation model and
the evidence design.

Exit: auth/ownership, CRUD persistence, secret refs and audit redaction;
PatientZero registered through the API as an application with its telemetry
source and database target, whose approved probes the collector runs on kind.

## Phase 4 — Web UI

- Bootstrap, auth, and inventory/onboarding screens on the phase-3 APIs [5]
- Investigation, alert and report screens come in phase 7, with their APIs
  and workers

Exit: API-backed login, roles, onboarding and Grafana/Loki deep links;
PatientZero onboarded from the UI.

## Phase 5 — Production delivery (EKS + Argo CD)

The chart already runs on kind and in CI; this phase takes it to production with
the defaults in
[ADR 0005](../architecture/decisions/0005-production-delivery-and-operations-defaults.md) [18].
It comes after the Web UI so the first production deploy carries a usable
product; pull it forward if a production SQL Server must be monitored sooner.

- The observability platform is chosen: the Grafana stack stays, and other
  applications receive Heartbeat's telemetry as OTLP from the Collector
  ([ADR 0019](../architecture/decisions/0019-keep-grafana-and-share-telemetry-as-otlp.md)) [18]
- `heartbeat-deploy` repository with Argo CD Applications and per-environment
  values; optional Argo CD rehearsal on kind
- CI publishes multi-arch images and the chart to ECR through GitHub OIDC
- EKS infrastructure as code; External Secrets; healthchecks.io dead-man's
  switch; Discord, then Slack, Teams and WhatsApp receivers
- Where the monitored SQL Servers sit relative to the EKS VPC

Exit: production networking, identity, TLS, storage, retention and restore
validated in the target environment.

## Phase 6 — Complete application and database product workflows

- OutSystems and SQL Server onboarding, the remaining signal coverage,
  dashboards and drill-down [10, 11]; CPU, cache and I/O arrive in phase 1
  ([SQL Server signal coverage](../architecture/database-observability.md#sql-server-signal-coverage))

Exit: onboarding in the UI → ingest/collection → Loki/Prometheus → Grafana, with
field/metric semantics, cardinality and database load checked.

## Phase 7 — Investigations, alerting and reporting

- Job/result contracts on Redis Streams, versioned before the first producer
  and consumer ([ADR 0013](../architecture/decisions/0013-api-design-and-subsystem-boundaries.md)) [4]
- Collector evidence: content, destination and publication, retrievable
  before the analyzer uses it [12.4]
- Session analyzer, the investigations API and the investigation UI [4.6, 6, 12]
- Alerting API, alert policy rendering to Prometheus and Alertmanager;
  adaptive baselines [4.7, 13]
- Reporting API, report templates, schedules, generation and email delivery
  [4.8, 7, 14]
- The rest of PatientZero: timers, extension calls, a fake partner, and the
  blocking, slow-partner, timer-backlog, bad front-end and daylight saving
  scenarios [16.5]; request traces if they haven't come earlier [3.5, 3.6]
- See [operator workflows](../architecture/workflows.md)

Exit: deterministic correlation/baseline/report tests; alert
trigger/route/dedupe/resolve and scheduled/on-demand reports end to end;
investigations and reports on PatientZero runs match their journals.

## Phase 8 — Advanced reliability, HA and security

- Collector replica ownership and takeover with fencing; recovery-time and
  data-gap objectives; outage testing [19]
  ([collector HA design](../architecture/database-observability.md#collector-recovery-and-high-availability-planned))
- Authn/authz hardening, query budgets, rate limits, worker idempotency and
  backoff [15.2]
- Audit rotation and shipping; operational runbooks [15.3]

Query safety and collector endpoint security are phase 1 work, not deferred here.

## Phase 9 — Fallback file ingestion (deferred)

- Ingest uploaded/exported files only when direct database connectivity is
  impossible, with validation, lineage and freshness warnings [20]
- Always lower priority than live collection

## Phase 10 — Oracle collector (late roadmap)

- **Gate first:** a time-boxed check of what Grafana Alloy and established Oracle
  exporters cover, recorded as an ADR. It decides whether to build a custom
  Oracle collector, adopt Alloy for Oracle, or run the phase-11 comparison first,
  so a custom collector is not built only to be replaced [21]
- Then: connectivity, probes, evidence, config, API/Web onboarding, dashboards,
  rules, investigation/report integration, an isolated test target and a runbook

Exit: supported versions, least privilege, metric semantics and query load
validated, plus failure/reload/ownership tests.

## Phase 11 — Custom collector vs Grafana Alloy comparison (last)

- A parallel, evidence-based comparison on a dedicated branch, for SQL Server
  and Oracle separately; the outcome is open [22]
- See the
  [comparison design](../architecture/database-observability.md#custom-collector-and-alloy-comparison-late-roadmap)

## Only if scope expands

Asset and topology inventory, collector fleet history in PostgreSQL, OIDC/SSO,
Mimir scale-out, engines beyond SQL Server and Oracle, DB-backed integration
CRUD and audit search, custom dashboards, autonomous RCA [17].
