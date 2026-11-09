# 0004. Core technology choices

- Status: Accepted. Updated by [ADR 0021](0021-inventory-as-code-and-no-web-ui.md):
  there is no Web UI, so the TypeScript + React row no longer applies.
- Date: 2026-05-30 (recorded 2026-09-29)

## Context

Heartbeat is built for use inside a large company. The constraints: few
runtimes and moving parts, free for enterprise use, Grafana-native, and
compatible with OpenTelemetry. It complements existing APMs (Dynatrace,
Datadog) rather than replacing them.

## Decision

| Concern | Choice | Why | Alternatives deferred |
| --- | --- | --- | --- |
| API and collectors | Go | One backend language: static binaries, strong concurrency for polling, good telemetry libraries | NestJS (more runtime layers) |
| Web UI | TypeScript + React | Practical for an internal operator UI; no SSR unless needed | — |
| Metadata | PostgreSQL | Reliable, free, well understood ([ADR 0001](0001-postgresql-metadata-only.md)) | — |
| Metrics | Prometheus | Smallest operational surface; Grafana-native | Mimir only if retention, HA, multi-tenancy or cross-region scale outgrow Prometheus; VictoriaMetrics, Thanos |
| Logs | Loki | Grafana-native, far fewer moving parts | OpenSearch/Elastic only if a validated requirement needs document search Loki cannot serve without harmful cardinality |
| Dashboards | Grafana, provisioned as code | Existing operator familiarity; no custom dashboard engine | — |
| Alert routing | Alertmanager | Grouping, dedupe, silencing, delivery | — |
| Telemetry ingress | OpenTelemetry Collector | Standard OTLP; configuration before custom code | — |
| Async work | Redis | Alerting, reporting and investigations are asynchronous enough to justify a queue/cache | — |
| Report artifacts | S3-compatible object storage, only if persistence is required | Otherwise generate on demand | — |
| Deployment | Kubernetes with Helm ([ADR 0003](0003-helm-on-kind-and-production.md)) | Reproducible cloud delivery | — |
| Auth | Local RBAC first | Minimal MVP | OIDC/SSO when corporate requirements demand it |

Collection runs from a separate monitoring environment. No agent or collector
is installed on monitored database hosts.

## Consequences

- MVP stack: Grafana + Prometheus + Loki + PostgreSQL + Redis.
- Scale-out (Mimir) and richer search (OpenSearch) are explicit later decisions,
  not defaults.
- These engines are the bundled platform, not a dependency of Heartbeat's
  services: [ADR 0011](0011-platform-agnostic-telemetry-boundary.md) keeps the
  platform replaceable, and [ADR 0019](0019-keep-grafana-and-share-telemetry-as-otlp.md)
  keeps the Grafana stack after the review against the organization's ELK
  stack.
- Redis runs as Valkey, and how PostgreSQL and Redis are deployed is
  [ADR 0015](0015-postgresql-and-redis-delivery.md).
- A late-roadmap evaluation compares the custom SQL Server collector with
  Grafana Alloy; see
  [database observability](../database-observability.md#custom-collector-and-alloy-comparison-late-roadmap).
