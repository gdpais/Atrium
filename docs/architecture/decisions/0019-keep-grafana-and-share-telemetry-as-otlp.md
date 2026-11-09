# 0019. Keep the Grafana stack; share telemetry as OTLP from the Collector

- Status: Accepted; sharing telemetry as OTLP is not built yet [TODO 9.5]
- Date: 2026-11-02

## Context

[ADR 0011](0011-platform-agnostic-telemetry-boundary.md) kept the Grafana
stack bundled and left a review of the organization's ELK stack (Elastic
Cloud) for before production [TODO 18]. Every Grafana dashboard and
Prometheus rule built until then would be rewritten if ELK won.

The owner decided on 2026-11-02 to keep the Grafana stack. Other
applications in the organization that consume OpenTelemetry must still be
able to receive Atrium's metrics, logs and traces.

Two places could translate Atrium's data to OpenTelemetry for them:

- **The API** ([ADR 0013](0013-api-design-and-subsystem-boundaries.md)). It
  is the control plane: inventory, policies, investigations and audit. It
  does not carry telemetry. Routing telemetry through it would tie the data
  path's volume and availability to the backoffice, and rebuild the
  batching, retry and on-disk queue the Collector already has. OTLP is also a
  push protocol with no query side, so an API endpoint could not serve
  "OTLP" to a caller that asks.
- **The OpenTelemetry Collector.** It already receives every OTLP signal
  Atrium emits, has a pipeline per signal (`metrics`, `logs`,
  `logs/self`; `traces` comes with Tempo [TODO 3.6]) with role-named log
  exporters, and is the only place besides the chart that names a platform
  (ADR 0011). In the bundled stack Prometheus scrapes the db-collector's
  `/metrics` directly, and the Collector's own `prometheus` exporter
  (`otel-collector:8889`), so the db-collector's series do not pass through
  the Collector today; the
  [external-platform example](../../../infra/helm/values/external-platform.example.yaml)
  already has the Collector scrape them instead.

## Decision

1. **The Grafana stack is Atrium's observability platform.** Prometheus,
   Loki, Grafana and Alertmanager stay bundled, Tempo joins them
   ([ADR 0018](0018-atriums-own-telemetry.md), [TODO 3.6]), and
   dashboards, rules and query adapters are written for them only. The ELK
   review in TODO 18 is
   closed; ADR 0011's boundary stays, so another platform is still a values
   file away.
2. **The Collector is the transformation layer.** A consumer that wants
   Atrium's telemetry gets it pushed as OTLP from the Collector: an
   optional, role-named exporter per signal (`otlp_http/share-metrics`,
   `/share-logs`, `/share-traces`) added next to the bundled exporters, set in
   the chart's values with the endpoint and credentials in a Secret. When
   metrics are shared, the Collector scrapes the db-collector's `/metrics`
   with the same job as Prometheus, so both see the same series, in a
   pipeline of its own that exports only to the shared exporter: in the
   `metrics` pipeline they would also reach the Collector's `prometheus`
   exporter, and Prometheus would store them twice.
3. **The API serves Atrium's own resources, not raw telemetry.**
   Inventory, investigations, evidence links and, later, query results go
   through the API as JSON, and its reads of logs and metrics go through the
   ADR 0011 adapters, with the Grafana stack's (Loki, Prometheus, and Tempo
   once it is bundled) the only implementation.

## Consequences

- Grafana dashboards and Prometheus rules are no longer at risk of a platform
  change before production.
- Sharing with a consumer is configuration plus tests, not service code
  [TODO 9.5]. The shared OTLP carries the raw series and log records;
  Prometheus recording rules and alert state stay in Prometheus, and a
  consumer that needs them reads Prometheus' API.
- A shared exporter sends personal data in logs (user, session and request
  IDs, client IPs; [ADR 0012](0012-personal-data-in-telemetry.md)) to another
  system: each consumer needs the same compliance sign-off as Loki, and the
  exporter needs TLS and authentication [TODO 18].
- The Collector carries more traffic when sharing is on: its export queue
  and memory limits are sized for the extra exporters.
