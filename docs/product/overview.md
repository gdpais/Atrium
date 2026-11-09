# Heartbeat Product Overview

Heartbeat is an SRE-focused monitoring platform. It covers deep database
observability, application telemetry, session-centric investigation, adaptive
alerting and operational reporting, on top of Grafana, Prometheus and Loki.

It **complements** commercial APMs such as Dynatrace and Datadog rather than
replacing them. The focus is on gaps those tools leave:

- **Deep database observability** beyond generic APM plugins: waits, locks and
  blocking, memory pressure, session and connection saturation, storage pressure.
- **Cross-system telemetry** that brings application and platform logs and
  metrics into one place.
- **Session-centric troubleshooting**: "what happened to this user (or IP,
  session, request) in this application between 10:00 and 10:15?"
- **Flexible, standards-based integrations** (OpenTelemetry, Prometheus, Loki,
  Grafana) instead of vendor lock-in.

How it is built is in the [architecture overview](../architecture/overview.md).

## Who it is for

| Audience | Uses Heartbeat to |
| --- | --- |
| SREs and on-call engineers | Investigate incidents across application logs, metrics and database evidence; receive low-noise alerts |
| Platform and DBA teams | Watch SQL Server health without installing agents on database hosts |
| Application owners | Read application health, error and latency trends; receive scheduled reports |
| Heartbeat operators | Manage environments, applications and monitored databases as code, reviewed in pull requests; later alert policies and report schedules |

## Principles

- **No agents on monitored databases.** Collection runs from a separate
  monitoring environment using remote, read-only, non-blocking queries with
  least-privilege logins and query budgets.
- **Alert quality over alert count.** Static guardrails plus explainable
  adaptive thresholds; grouping and deduplication to reduce false positives.
- **Reuse Grafana and Loki** for dashboards and log exploration; no custom
  dashboard or search engine.
- **Application-first.** Investigations, alerts and reports belong to an
  application; the environment is derived from it.
- **Fail closed.** Invalid configuration never replaces a working one.

## Scope

What the product covers, and the outcome for each capability. The
[roadmap](roadmap.md) orders the work into phases; the last column says which
phase delivers each capability.

| Capability | Outcome | Phase |
| --- | --- | --- |
| Application observability | OutSystems first (Traditional and Reactive): normalized logs in Loki, derived metrics in Prometheus, health/errors/latency dashboards, compatibility with default OutSystems log fields | 2 |
| Database observability | SQL Server: waits, blocking, sessions, memory, storage, throughput, CPU, buffer cache and file I/O metrics, collector self-observability, dashboards ([signal coverage](../architecture/database-observability.md#sql-server-signal-coverage)) | 1; the remaining signals in 6 |
| Telemetry ingestion | OTLP metrics, logs and traces through the OpenTelemetry Collector; traces in Grafana Tempo | 2; Tempo with Heartbeat's own telemetry ([TODO 3.6](../../TODO.md#36-otlp-trace-ingest)) |
| Session investigation | Query by application + user/IP/session/request + time range; timeline, anomaly windows and evidence links into Grafana/Loki | 7 |
| Alerting | Static rules plus adaptive baselines, rendered to Prometheus and routed by Alertmanager | Static rules and SLO alerts in 2; adaptive baselines in 7 |
| Reporting | Scheduled (weekly first) and on-demand reports; download or email delivery is an [open question](#open-questions) | 7 |
| Inventory and administration | Inventory, targets and probes as code, reviewed in pull requests and applied by the API; users and roles, and later policies and schedules, through the API; audit logging ([ADR 0021](../architecture/decisions/0021-inventory-as-code-and-no-web-ui.md)) | 3 and 4 |

### Done when

- One OutSystems application/environment can be onboarded end to end.
- An SRE can see application health, errors, latency and correlated Loki logs
  in Grafana.
- Session analysis returns useful correlated evidence for a real incident
  sample.
- The SQL Server collector runs against real targets with safe probes.
- Static and adaptive alerting both work and are auditable.
- A weekly report runs automatically and is delivered.

### Non-goals

- Parity across database engines. SQL Server first, Oracle next; PostgreSQL is
  Heartbeat's metadata store, not a monitored engine.
- A code-level APM tracing agent ecosystem.
- Autonomous root-cause analysis.
- A custom dashboard engine, or a Web UI of Heartbeat's own: people read
  Grafana, and operators change the inventory as code
  ([ADR 0021](../architecture/decisions/0021-inventory-as-code-and-no-web-ui.md)).
- File-based ingestion, except later as a last-resort fallback when direct
  database connectivity is impossible.

## Current status

Phases 0 and 1 are done. Phase 2 (OutSystems ingest) passed its validation on
2026-11-02 and its outcome demo on 2026-11-08, and waits for its release.
Phases 3 (the API) and 4 (inventory as code) passed their stage validations
on kind on 2026-11-08 and 2026-11-07. Phase 5 (production delivery) has
started: the `heartbeat-deploy` config repository and the collector's TLS
are built, the AWS infrastructure is not. Nothing runs in production yet. See the
[roadmap](roadmap.md) for each phase and [TODO.md](../../TODO.md) for the task
list.

| Area | Status |
| --- | --- |
| Shared contracts, PostgreSQL schema | Done |
| Kubernetes delivery: one Helm chart for kind, CI and production; kind workflow and acceptance tests | Done for kind and CI; `v0.1.0` is published on GHCR and the next release waits for the release app [0.5]; production (EKS, Argo CD) is phase 5 |
| SQL Server DB collector (probes, hot reload, failure isolation, readiness) | Done: secured endpoints, least-privilege login, counters and self-metrics, CPU, buffer cache and file I/O signals, per-probe failure isolation |
| OTel Collector, Prometheus, Loki, Tempo, Grafana, Alertmanager | Provisioned by the chart; alerts reach Alertmanager (with a Watchdog) but notify no one until an environment configures a receiver; chat receivers only in the production example |
| OutSystems 11 log extraction (`outsystems_logs` collector kind) | Built and validated: extraction to Loki, derived metrics, SLO burn-rate alerts and dashboards, each query reviewed against a real logging database's schema; left: the release |
| Heartbeat's own telemetry | Done: metrics, logs over OTLP and traces in Tempo, with the *Heartbeat Service* dashboard and its SLOs ([ADR 0018](../architecture/decisions/0018-heartbeats-own-telemetry.md)) |
| API | Deployed by the chart with PostgreSQL and Valkey: login, users and roles; environments, applications and components; telemetry sources; database targets, probe definitions and assignments rendered to `managedCollectors`; Grafana deep links; audit records in Loki. Validated on kind on 2026-11-08 |
| Inventory as code (replaces the Web UI) | Done and validated on kind on 2026-11-07: the file's schema and `api inventory check`, one writer per row, the API applies the mounted file, kind declares PatientZero and its database target in it, `api inventory render` renders its probe assignments offline, and Grafana lists the inventory beside its health. Deferred: custom probe definitions in the file [17]; tokens for machine clients when the first one needs them |
| Session analyzer, reporting | Not started (phase 7) |

## Risks

- OutSystems telemetry quality and export paths vary by version and deployment.
- Session correlation is only as good as the identifiers present in source logs.
- High-cardinality labels (user, session, IP) can damage Loki; they stay out of
  labels.
- Poorly designed remote SQL polling can load production databases; every
  probe is reviewed and budgeted.
- Opaque adaptive alerting erodes trust, so baselines must be explainable.

## Open questions

Answered elsewhere, so not listed here: how identifiers are kept in Loki
([ADR 0012](../architecture/decisions/0012-personal-data-in-telemetry.md)),
audit records ([ADR 0016](../architecture/decisions/0016-audit-records-through-an-outbox.md)),
each store's retention ([data retention](../reference/configuration.md#data-retention)),
chat receivers and the dead-man's switch
([ADR 0005](../architecture/decisions/0005-production-delivery-and-operations-defaults.md)),
and the Grafana stack ([ADR 0019](../architecture/decisions/0019-keep-grafana-and-share-telemetry-as-otlp.md)).

- **Compliance sign-off** before the first production deploy: someone
  accountable for data protection confirms raw identifiers in Loki, Loki's
  30-day retention (audit records included) and who has Grafana access
  ([ADR 0012](../architecture/decisions/0012-personal-data-in-telemetry.md)
  point 4; **Owner**, [TODO 9.2](../../TODO.md#92-loki)).
- How often are user IDs, session IDs and client IPs filled in each
  OutSystems log type in real data? `Request_Key` is on all eight; the
  schema survey did not measure the others
  ([ADR 0008](../architecture/decisions/0008-outsystems-11-log-extraction.md)).
- Is a report download enough, or is email delivery required from the first
  reports?
- Is multi-tenancy needed? OIDC/SSO is deferred until a deployment requires
  it ([ADR 0021](../architecture/decisions/0021-inventory-as-code-and-no-web-ui.md)).
- Expected scale: environments, applications, database instances and log
  volume.
- Is a paging tool needed for escalation beyond the chat receivers? ADR 0005
  leaves it a separate decision.
