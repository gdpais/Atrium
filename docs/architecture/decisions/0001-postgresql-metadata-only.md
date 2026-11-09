# 0001. PostgreSQL stores durable product metadata only

- Status: Accepted
- Date: 2026-06-04 (recorded 2026-09-29)

## Context

Atrium handles high-volume telemetry (metrics, logs, session evidence) and a
comparatively small amount of product state: inventory, policies, schedules,
investigation requests and results. Putting both in one relational database
would make PostgreSQL a bottleneck, duplicate data that Prometheus and Loki
already store and query better, and spread "alert truth" across systems.

## Decision

PostgreSQL is the system of record for product metadata only: environments,
applications, components, telemetry sources, database targets and probe
policies, users and roles, investigation requests and results, alert policy
intent and alert history, report templates, schedules and runs.

It does **not** store raw or derived metrics, logs, sessions or session
identity mappings, integration config, audit streams, collector runtime state,
or report binaries. It never evaluates alerts. Executable rules are rendered to
Prometheus and routes to Alertmanager.

Application-owned records derive their environment through
`applications.environment_id`; there are no duplicate ownership paths.

## Consequences

- Telemetry scales independently in Prometheus/Loki; PostgreSQL stays small.
- Investigations recompute correlation from Loki/Prometheus at query time and
  store only bounded summaries and links.
- Some features need two systems (policy intent in PostgreSQL, evaluation in
  Prometheus), so rendering and reconciliation must be designed carefully.
- Tables for audit, integrations, collector fleets or assets can be added later
  if a real UI or compliance workflow needs them. See the
  [data model](../data-model.md#what-postgresql-does-not-store).
