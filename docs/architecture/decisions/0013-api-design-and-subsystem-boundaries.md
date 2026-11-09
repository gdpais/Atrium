# 0013. API design: stdlib HTTP, spec-first OpenAPI, sqlc, Redis sessions and job streams; frozen boundaries

- Status: Accepted; built except the job envelopes on Redis Streams (phase 7).
  Updated by [ADR 0021](0021-inventory-as-code-and-no-web-ui.md): there is
  no Web UI, so the boundaries' Web UI row and the UI client of the OpenAPI
  contract no longer apply.
- Date: 2026-10-21

## Context

Phase 3 builds the API (`apps/api`), the first service that reads and writes
PostgreSQL. Today `apps/api/cmd/api` and `apps/api/internal` are empty
directories. What already constrains it:

- One backend language, few runtimes and dependencies, free for enterprise
  use; local RBAC first, OIDC later ([ADR 0004](0004-core-technology-choices.md)).
  The module is on Go 1.27.
- The schema is [`db/migrations/0001_foundations.up.sql`](../../../db/migrations/0001_foundations.up.sql)
  with a matching `.down.sql`. Nothing records which migrations a database
  has: the integration tests apply the files with `psql`. `users` has no
  password column.
- PostgreSQL holds product metadata only ([ADR 0001](0001-postgresql-metadata-only.md));
  collector desired state comes from YAML, never from the API's tables
  ([ADR 0002](0002-runtime-config-in-yaml.md)); services emit only OTLP and
  Prometheus exposition and read telemetry through platform adapters
  ([ADR 0011](0011-platform-agnostic-telemetry-boundary.md)).
- Durable job state already has tables (`investigation_jobs`, `report_runs`),
  and the architecture guardrails say Redis is never the system of record and
  workers are idempotent.
- The Web UI (phase 4) needs a typed client, and production deploys through
  Argo CD, which renders the chart with `helm template`: no Helm hooks unless
  Argo's hook mapping fits ([ADR 0005](0005-production-delivery-and-operations-defaults.md)).

The subsystem boundaries and ownership rules have been open in
[TODO 0.1](../../../TODO.md) since phase 0. Their content is already in ADRs
0001, 0002 and 0011; the API is where they become interfaces, so this record
freezes them.

## Decision

### Boundaries and ownership (frozen)

1. **Who owns what.**

   | Subsystem | Owns | Talks to others only through |
   | --- | --- | --- |
   | API (`apps/api`) | Every PostgreSQL table except job results (below); users, sessions, approvals; rendering approved probe assignments ([ADR 0014](0014-probe-assignments-to-runtime-config.md)) | Its OpenAPI contract (UI, scripts); job envelopes on Redis; telemetry adapters (ADR 0011); the collector's admin API, read-only |
   | DB collector | Reading monitored databases; collector runtime state | `integrations.yaml`, `/metrics`, `/healthz`, `/readyz`, `/admin/config`, OTLP out |
   | Workers (session analyzer, reporting) | Running jobs; the rows of the jobs they claimed, and their results (`investigation_results`, `evidence_links`, `report_runs` status) | Job envelopes on Redis; PostgreSQL; telemetry adapters |
   | Web UI | Nothing durable | The API only, same origin |
   | OTel Collector and the observability platform | Telemetry: metrics, logs, rules, alert routing | OTLP and Prometheus scrapes (ADR 0011) |

   The API never queries a monitored database and never writes cluster state.

2. **Ownership rules.**
   - Application-owned records derive their environment through
     `applications.environment_id`; there is one ownership path (ADR 0001).
   - PostgreSQL stores control-plane metadata only (ADR 0001).
   - YAML/Kubernetes stores integration and collector desired runtime config
     (ADR 0002). Approvals in PostgreSQL are requests; the config file in git
     is the desired state ([ADR 0014](0014-probe-assignments-to-runtime-config.md)).
   - The observability platform (Prometheus and Loki when bundled) stores
     telemetry (ADR 0011).
   - Redis stores transient state only: job delivery, locks, idempotency
     keys, rate limits, short-lived caches and login sessions. Losing it
     logs users out and delays jobs; it loses no record.

### HTTP and contract

3. **Routing: the standard library.** `net/http`'s `ServeMux` with method and
   wildcard patterns (`GET /api/v1/applications/{id}`), middleware as
   `func(http.Handler) http.Handler`. No web framework. The resource API is
   under `/api/v1`; `/healthz`, `/readyz` and `/metrics` stay outside it.
   Errors are `application/problem+json` (RFC 9457).
4. **Spec-first OpenAPI 3.1, server types generated with oapi-codegen.** The
   contract is `apps/api/openapi.yaml`, written by hand and reviewed like
   code. [oapi-codegen](https://github.com/oapi-codegen/oapi-codegen)
   generates the models and a strict, typed server interface for the
   standard library's `ServeMux` (its `std-http-server` target); handlers
   implement that interface. Generated code is committed, and CI regenerates
   it and fails on a difference. v2.8.0 added what its release notes call
   "initial OpenAPI 3.1 support" (it needs Go 1.25 or later), so the spec
   keeps to plain constructs (`type: [T, "null"]`, `oneOf` enums); one that
   doesn't generate is written another way, not a reason to change tools.
   The UI's client is generated from the same file (phase 4).
   Within `v1`, changes are additive; a breaking change is `v2`.

### Data

5. **`pgx` with `sqlc`.** `pgx` v5 (`pgxpool`) directly, not `database/sql`.
   Queries are `.sql` files under `apps/api/internal/store/queries/`;
   [sqlc](https://sqlc.dev/) reads the schema from `db/migrations` (it parses
   `.up.sql` and skips `.down.sql`) and generates typed Go. No ORM: SQL stays
   reviewable, and a query that no longer matches the schema fails at
   generation, not at run time. Generated code is committed and checked in CI
   like the OpenAPI code. A request that writes several rows, and its audit
   record [4.10], runs in one transaction.
6. **Migrations stay in `db/migrations`, applied by golang-migrate.** The
   existing `NNNN_name.up.sql` / `.down.sql` pairs are the format
   [golang-migrate](https://github.com/golang-migrate/migrate) reads, so
   nothing is renamed, and zero-padded numbers keep its numeric order and
   sqlc's lexical order the same. A small Go package in `db/migrations`
   embeds the files; the API image runs `api migrate up` as an init container
   before the API starts. golang-migrate records the version in
   `schema_migrations` and takes a PostgreSQL advisory lock, so replicas
   starting together apply each migration once. No Helm or Argo hook.
   Changes are expand, then contract ([CONTRIBUTING](../../../CONTRIBUTING.md));
   `.down.sql` files are for tests and local use, never run in production.

### Identity

7. **Local users, argon2id.** A migration adds a password hash to `users`.
   Hashes are argon2id (`golang.org/x/crypto/argon2`) in the PHC string
   format, which carries its parameters, so they can be raised later and old
   hashes upgraded at the next login. Start with RFC 9106's second
   recommended setting (t=3, p=4, 64 MiB), and cap concurrent hashing so a
   burst of logins can't run the pod out of memory. Failed logins are
   rate-limited per account and per client address in Redis. There is no
   default password: the first admin is created with `api users create-admin`,
   which reads the password from standard input.
8. **Cookie sessions held in Redis.** A random 256-bit session ID in a
   `__Host-` cookie (`HttpOnly`, `Secure`, `SameSite=Lax`, `Path=/`); Redis
   keeps the ID's SHA-256 hash with the user, created and last-seen times,
   with an idle and an absolute expiry (configurable; proposed 8 hours and 7
   days). Unsafe methods also need an `Origin` header matching the configured
   site. Logout deletes the session; disabling a user or changing their
   password deletes all of theirs. Roles come from `roles` / `user_roles`
   (MVP: `admin`, `operator`, `viewer` for wallboards [4.2]); each operation
   declares the least role it needs, and the middleware checks it.

### Jobs

9. **Versioned job envelopes on Redis Streams, with consumer groups.** The
   PostgreSQL row (`investigation_jobs`, `report_runs`) is the job; the
   stream message only delivers it.
   - One stream per job type (`heartbeat:jobs:<type>`), one consumer group per
     worker service, trimmed by length.
   - The envelope carries `type`, `version`, `job_id`, `enqueued_at` and a
     typed `payload`, defined as JSON Schema beside the existing payload
     contracts in `packages/telemetry-contracts`. A breaking payload change
     bumps `version`; a consumer that doesn't know a version dead-letters the
     message instead of guessing.
   - Workers are idempotent by `job_id`: a claim is a conditional update
     (`queued` → `running`), and a message is acknowledged (`XACK`) only after
     the result is committed. Messages idle too long are reclaimed
     (`XAUTOCLAIM`); after a set number of deliveries they go to a dead-letter
     stream and the job is marked `failed`.
   - The API re-enqueues jobs that stay `queued` past a timeout, which covers
     a lost message or a Redis restart. A duplicate is harmless.

   Contracts are versioned before the first producer or consumer exists, and
   each workflow is integration-tested when its worker lands [4].

Considered and not chosen:

- **A router or framework (chi, Echo, Gin).** Go's `ServeMux` has method and
  wildcard routing since 1.22, and oapi-codegen generates for it, so a
  dependency would add little.
- **ogen** for the OpenAPI code. Fuller OpenAPI 3.1 support, but it brings
  its own router and runtime instead of the standard library's.
- **Code-first specs** (annotations that generate the spec). The spec drifts
  from what reviewers read, and the UI client would depend on Go comments.
- **An ORM (GORM, ent).** Hides the SQL that reviewers and the probe review
  culture expect to see, for a schema that is small and stable.
- **Sessions in PostgreSQL.** Survives a Redis restart and keeps Redis off
  the request path, but writes a row on every request to refresh last-seen,
  and needs a cleanup job. Redis expires keys itself, and a Redis outage
  already stops jobs.
- **PostgreSQL as the queue** (`FOR UPDATE SKIP LOCKED`). One store fewer and
  no re-enqueue sweep, and enough for the volumes expected. Not chosen
  because Redis is already in the stack (ADR 0004) for sessions and caches,
  consumer groups give per-service delivery, pending tracking and reclaiming
  without writing them, and polling stays off the small production database.
  It remains the fallback if Redis Streams prove more work than they save.
- **OIDC from the start.** ADR 0004 defers it until corporate requirements
  demand it [17].

## Consequences

- The API's dependencies are few and conventional: `pgx`, a Redis client,
  golang-migrate, `golang.org/x/crypto`, and the oapi-codegen runtime. Code
  generation needs two pinned tools (oapi-codegen, sqlc); CI checks that
  their output is current.
- Every API request needs both PostgreSQL and Redis: if Redis is down, nobody
  can log in or stay logged in. Readiness reports both.
- `schema_migrations` appears in the database; the integration tests keep
  applying the files with `psql` and gain a test that golang-migrate applies
  them too.
- Job delivery is at least once; correctness rests on idempotent workers and
  conditional state changes, tested per workflow.
- The 0.1 freeze items are closed by this record; changing a boundary above
  now needs a new ADR.
- How assignments reach the collector is [ADR 0014](0014-probe-assignments-to-runtime-config.md);
  how PostgreSQL and Redis are deployed is [ADR 0015](0015-postgresql-and-redis-delivery.md);
  how the audit record is written in the transaction and delivered is
  [ADR 0016](0016-audit-records-through-an-outbox.md).
