# Architecture Decisions

Short records of decisions that shape Heartbeat: the context, what was
decided, and the consequences. Add a new record instead of rewriting an old
one; mark the old record *Superseded by ADR NNNN*.

| ADR | Decision | Status |
| --- | --- | --- |
| [0001](0001-postgresql-metadata-only.md) | PostgreSQL stores durable product metadata only, never telemetry | Accepted |
| [0002](0002-runtime-config-in-yaml.md) | Integrations and collector desired state come from YAML/Kubernetes, with hot reload | Accepted |
| [0003](0003-helm-on-kind-and-production.md) | Deliver the platform with Helm on kind (local, CI) and production Kubernetes | Accepted; implemented for kind and CI |
| [0004](0004-core-technology-choices.md) | Core stack: Go, React, Prometheus, Loki, Grafana, Alertmanager, OTel Collector, PostgreSQL, Redis | Accepted; React left the stack with the Web UI (ADR 0021) |
| [0005](0005-production-delivery-and-operations-defaults.md) | Production on AWS EKS via Argo CD GitOps; ECR; AWS Secrets Manager + ESO; pinned latest-stable versions; Discord → Slack/Teams/WhatsApp alerts; healthchecks.io dead-man's switch; Grafana admin auth | Accepted; chart side implemented (version pins, Watchdog route, production example values); production not built |
| [0006](0006-trunk-based-development-and-tagged-releases.md) | Trunk-based development on `master`; one SemVer version per release; `vX.Y.Z` tags from release-please publish multi-arch images and the chart to GHCR; changelog from Conventional Commits; no moving tags | Accepted; in effect from 2026-10-05; workflows built, release app and first release pending; required checks replaced by ADR 0007 |
| [0007](0007-change-based-ci-with-one-required-check.md) | One CI workflow; slow jobs run only when a change can affect them, everything runs on `master`; `ci-ok` is the only required check and fails unless each job ran or was skipped as intended | Accepted; in effect from 2026-10-10 |
| [0008](0008-outsystems-11-log-extraction.md) | OutSystems 11 self-managed logs are extracted from the logging database by a new db-collector kind, `outsystems_logs`: one reviewed `.sql` file per log type, watermarked reads, OTLP to the OTel Collector and Loki, derived metrics on the collector's `/metrics` | Accepted; extraction built, derived metrics pending |
| [0009](0009-remove-the-otel-gateway.md) | Remove the OTel gateway service; Alertmanager's default receiver has no integrations until an environment configures one; push sources go to the OTel Collector over OTLP, mapped with OTTL | Accepted |
| [0010](0010-patientzero-test-application.md) | Heartbeat is tested against PatientZero, a synthetic OutSystems 11 application on the sandbox SQL Server: real SQL and OutSystems log rows from the same requests, k6 load, named fault scenarios with expected signals, a run journal; a replica of a real logging database is only read, for its schema and traffic profile | Accepted; first slice being built |
| [0011](0011-platform-agnostic-telemetry-boundary.md) | Heartbeat's services emit only OTLP and Prometheus exposition; only the OTel Collector and the chart name a platform; the integrations config is neutral (`platform`, `dashboards`, `logs`, `alerting`); content is per platform. The Grafana stack stays bundled | Accepted; the ELK review is closed by ADR 0019 |
| [0012](0012-personal-data-in-telemetry.md) | User IDs, session IDs, request keys and client IPs stay raw in Loki's structured metadata, never labels; only the components that use Loki reach it and Grafana needs a login; the extractor can hash them later behind a setting; production needs compliance sign-off first | Accepted; compliance sign-off before production |
| [0013](0013-api-design-and-subsystem-boundaries.md) | API: stdlib `net/http` routing; spec-first OpenAPI 3.1 with oapi-codegen; `pgx` + `sqlc`; `db/migrations` applied by golang-migrate from an init container; local users with argon2id and cookie sessions in Redis; versioned job envelopes on Redis Streams, with the PostgreSQL row as the job. Freezes subsystem boundaries and ownership rules | Accepted; routing, the OpenAPI spec with generated server types, `pgx` + `sqlc` and golang-migrate (`api migrate up`) built; the init container and the rest pending |
| [0014](0014-probe-assignments-to-runtime-config.md) | The API never writes cluster state: approved probe assignments render to one generated values file (`managedCollectors`), proposed as a pull request to `heartbeat-deploy` and synced by Argo CD; on kind the same file goes into `make kind-deploy` | Accepted, amended 2026-11-04 (one interval per target; kind renders in the API's pod), and by ADR 0021: assignments in the inventory render from the file in its own pull request, so the API's pull request and GitHub App are needed only for assignments the API manages; the `none` output is built |
| [0015](0015-postgresql-and-redis-delivery.md) | PostgreSQL and Redis: small chart templates on the projects' images for kind and CI, off in production, which uses RDS and ElastiCache; Redis runs as Valkey; no Bitnami charts | Accepted |
| [0016](0016-audit-records-through-an-outbox.md) | Audit records are written to an `audit_outbox` in the change's transaction and drained as OTLP log records through the OTel Collector to Loki; no JSONL files; the outbox is a queue, not a store | Accepted |
| [0017](0017-outsystems-sources-map-application-identity.md) | An OutSystems 11 telemetry source maps an application to its `service.name` (`application_name`); the `outsystems_logs` collector per installation stays in YAML, not rendered by the API | Accepted |
| [0018](0018-heartbeats-own-telemetry.md) | Heartbeat's own telemetry: metrics stay on `/metrics`; logs go over OTLP as well as stderr, on their own Collector receiver and pipeline, never the OutSystems delivery path; traces use the OTel SDK and Tempo; opt-in loopback pprof | Accepted |
| [0019](0019-keep-grafana-and-share-telemetry-as-otlp.md) | The Grafana stack is the platform; the ELK review is closed. Other applications receive Heartbeat's metrics, logs and traces as OTLP from optional Collector exporters; the API serves Heartbeat's own resources, not raw telemetry | Accepted; sharing not built [TODO 9.5] |
| [0020](0020-web-ui-served-by-the-api.md) | Web UI: a Vite + React + TypeScript single-page app embedded in the API binary and served at `/`, same origin; client generated from `openapi.yaml` (openapi-typescript, openapi-fetch); TanStack Query, React Router, Mantine; a `web` CI job and Playwright in `kind-e2e` | Superseded by 0021; its screens were built [TODO 5.1–5.2] and removed |
| [0021](0021-inventory-as-code-and-no-web-ui.md) | No Web UI. The inventory (environments, applications, components, sources, targets, probes and assignments) is one YAML file per environment in `heartbeat-deploy`, reviewed as a pull request, validated in CI and applied by the API, which reconciles PostgreSQL to it; one writer per row (`config` or `api`); assignments render from the file without a database; Grafana shows the inventory from the API's info metrics; the API keeps its login | Accepted; the UI is removed, the inventory file not built [TODO 5] |

Template:

```markdown
# NNNN. Title

- Status: Proposed | Accepted | Superseded by NNNN
- Date: YYYY-MM-DD

## Context
## Decision
## Consequences
```

Working documents such as dated plans (`docs/plans/`) and reviews
(`docs/reviews/`) stay out of git. When one of them leads to a decision, record
the outcome here.
