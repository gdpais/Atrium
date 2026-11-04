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

## Who it is for

| Audience | Uses Heartbeat to |
| --- | --- |
| SREs and on-call engineers | Investigate incidents across application logs, metrics and database evidence; receive low-noise alerts |
| Platform and DBA teams | Watch SQL Server health without installing agents on database hosts |
| Application owners | Read application health, error and latency trends; receive scheduled reports |
| Heartbeat operators | Manage environments, applications, monitored databases, alert policies and report schedules |

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

## MVP scope

| Capability | MVP outcome |
| --- | --- |
| Application observability | OutSystems first (Traditional and Reactive): normalized logs in Loki, derived metrics in Prometheus, health/errors/latency dashboards, compatibility with default OutSystems log fields |
| Database observability | SQL Server: waits, blocking, sessions, memory, storage, throughput, CPU, buffer cache and file I/O metrics, collector self-observability, dashboards ([signal coverage](../architecture/database-observability.md#sql-server-signal-coverage)) |
| Telemetry ingestion | OTLP metrics, logs and traces through the OpenTelemetry Collector; traces kept in a trace store, Grafana Tempo expected ([TODO 3.6](../../TODO.md)) |
| Session investigation | Query by application + user/IP/session/request + time range; timeline, anomaly windows and evidence links into Grafana/Loki |
| Alerting | Static rules plus adaptive baselines, rendered to Prometheus and routed by Alertmanager |
| Reporting | Scheduled (weekly first) and on-demand reports, delivered by email |
| Operator backoffice | Inventory, targets and probes as code, reviewed in pull requests and applied by the API; the API for policies and schedules; audit logging ([ADR 0021](../architecture/decisions/0021-inventory-as-code-and-no-web-ui.md)) |

### MVP exit criteria

- One OutSystems application/environment can be onboarded end to end.
- An SRE can see application health, errors, latency and correlated Loki logs
  in Grafana.
- Session analysis returns useful correlated evidence for a real incident
  sample.
- The SQL Server collector runs against real targets with safe probes.
- Static and adaptive alerting both work and are auditable.
- A weekly report runs automatically and is delivered.

### Non-goals for the MVP

- Parity across database engines. SQL Server first, Oracle next; PostgreSQL is
  Heartbeat's metadata store, not a monitored engine.
- A code-level APM tracing agent ecosystem.
- Autonomous root-cause analysis.
- A custom dashboard engine.
- File-based ingestion, except later as a last-resort fallback when direct
  database connectivity is impossible.

## Current status

Heartbeat is in early MVP construction. See the [roadmap](roadmap.md) for
phase-by-phase status and [TODO.md](../../TODO.md) for the task list.

| Area | Status |
| --- | --- |
| Shared contracts, PostgreSQL schema | Done |
| Kubernetes delivery: one Helm chart for kind, CI and production; kind workflow and acceptance tests | Done (production values and Argo CD pending) |
| SQL Server DB collector (probes, hot reload, failure isolation, readiness) | Done for phase 1: secured endpoints, least-privilege login, counters and self-metrics, CPU, buffer cache and file I/O signals |
| OTel Collector pipeline, Prometheus/Loki/Grafana/Alertmanager provisioning | Configured; alerts reach Alertmanager (with a Watchdog), chat receivers only in the production example |
| OutSystems 11 log extraction (`outsystems_logs` collector kind) | In progress: extraction to Loki, derived metrics and dashboards built; real-database survey done; query review pending |
| API | Started: service bootstrap with health, readiness, metrics and the OpenAPI spec; no database or deployment yet |
| Inventory as code (replaces the Web UI), session analyzer, reporting | Not started |

## Risks

- OutSystems telemetry quality and export paths vary by version and deployment.
- Session correlation is only as good as the identifiers present in source logs.
- High-cardinality labels (user, session, IP) can damage Loki; they stay out of
  labels.
- Poorly designed remote SQL polling can load production databases; every
  probe is reviewed and budgeted.
- Opaque adaptive alerting erodes trust, so baselines must be explainable.

## Open questions

- Which user, session and request identifiers are reliably present in each
  OutSystems log type? (The first release targets OutSystems 11 self-managed,
  read from its logging database:
  [ADR 0008](../architecture/decisions/0008-outsystems-11-log-extraction.md).)
- Is in-app report download enough, or is email delivery required from day one?
- Auth model: local RBAC, SSO/OIDC, or both? Is multi-tenancy needed?
- Expected scale: environments, applications, database instances, log volume,
  scrape interval and retention?
- Preferred notification channels and on-call tooling?
- Compliance constraints: PII handling, retention and audit? (Identifiers in
  Loki are kept raw pending sign-off before production:
  [ADR 0012](../architecture/decisions/0012-personal-data-in-telemetry.md).)
