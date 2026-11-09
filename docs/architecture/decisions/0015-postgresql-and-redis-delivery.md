# 0015. PostgreSQL and Redis: small chart templates for kind and CI, managed services in production; Redis runs as Valkey

- Status: Accepted
- Date: 2026-10-21

## Context

The API (phase 3) needs PostgreSQL and Redis on kind and in CI
([TODO 4.1](../../../TODO.md)). Neither is deployed today: ADR 0003 removed
them until a service uses them, and the only PostgreSQL is the disposable
`postgres:17` integration-test fixture in `infra/docker-compose.test.yml`.
Constraints:

- What Atrium deploys goes through the chart; what it tests against stays
  a Docker fixture. Third-party engines are namespaced, with no CRDs, and each
  can be turned off ([ADR 0003](0003-helm-on-kind-and-production.md)).
- Argo CD renders the chart: no `randAlphaNum` or `lookup`, and Secrets are
  created outside it. Production uses RDS for PostgreSQL, Secrets Manager
  through External Secrets, and the latest stable versions production
  supports, pinned exactly ([ADR 0005](0005-production-delivery-and-operations-defaults.md)).
- Free for enterprise use ([ADR 0004](0004-core-technology-choices.md)).
- Nothing in Redis is a record: losing it logs users out and delays jobs,
  which PostgreSQL re-enqueues ([ADR 0013](0013-api-design-and-subsystem-boundaries.md)).

What was checked on 2026-10-21:

- **Bitnami.** Broadcom's announcement on
  [bitnami/containers#83267](https://github.com/bitnami/containers/issues/83267):
  since 28 August 2025 no new Bitnami images or charts are published to
  Docker Hub. Existing images moved to `docker.io/bitnamilegacy`, which gets
  no updates. A small set of hardened images stays free for development on
  the `latest` tag only, and production users are pointed to the paid
  Bitnami Secure Images. The packaged charts at `docker.io/bitnamicharts`
  are no longer updated, and their images may not deploy without overrides.
  The chart source stays Apache 2.0.
- **Redis licensing.** Per Redis's
  [licenses page](https://redis.io/legal/licenses/), Redis 8 and later are
  under a choice of RSALv2, SSPLv1 or AGPLv3; 7.4 to 7.8 under RSALv2 or
  SSPLv1; 7.2.x and earlier under BSD-3-Clause.
- **Valkey.** The Linux Foundation fork of Redis 7.2, under BSD-3-Clause;
  it speaks the same protocol. The project publishes `valkey/valkey` on
  Docker Hub (tags up to 9.2 today).
- **ElastiCache.** Per the
  [engine versions page](https://docs.aws.amazon.com/AmazonElastiCache/latest/dg/engine-versions.html),
  it supports Redis OSS up to 7.1, and Valkey from 7.2. A newer engine on
  ElastiCache is Valkey.
- **PostgreSQL.** The Docker Official Image `postgres` publishes 18.x as
  stable (19 is in release candidate).

## Decision

1. **Two small templates in the Atrium chart**, `postgresql` and `valkey`,
   on the projects' own images (`postgres`, `valkey/valkey`). Each is a
   one-replica StatefulSet with a ClusterIP Service, a readiness probe
   (`pg_isready`, `valkey-cli ping`), resources, a non-root security
   context, and a NetworkPolicy that admits only the API and the workers.
   Turned on with `postgresql.enabled` and `valkey.enabled`, and only once
   the API uses them.
2. **Storage.** PostgreSQL keeps its data on a PVC from the cluster's default
   StorageClass, so a pod restart keeps it. Valkey has no PVC by default
   (an `emptyDir`): nothing in it is a record, and a restart costs a login.
   A PVC can be turned on.
3. **Credentials from existing Secrets.** The templates read the PostgreSQL
   password and the Valkey password from `existingSecret` names. On kind,
   `scripts/kind.sh` generates them, as it does the collector's admin
   token; in production they come from Secrets Manager.
4. **Production uses managed services.** `production.example.yaml` sets both
   `enabled: false` and points the API at RDS for PostgreSQL and ElastiCache
   for Valkey, with TLS, by endpoint and Secret name. The templates are for
   kind and CI only: no replicas, no backups, no major-version upgrades
   (on kind, delete the PVC and migrate again).
5. **Redis runs as Valkey.** ADR 0004's "Redis" is the role and the
   protocol; the server is Valkey: BSD-3-Clause, so free for enterprise use
   without a license review, and what ElastiCache runs for any version past
   Redis OSS 7.1, so kind tests what production runs. Docs keep saying
   Redis for the role; the API's client libraries don't change.
6. **One version per engine, everywhere.** Each engine runs the newest major
   its AWS managed service supports, pinned to an exact tag. The integration
   test fixture moves to the same PostgreSQL major in the same change, so
   tests, kind and production agree. Renovate proposes updates, and the kind
   CI job must pass (ADR 0005).

Considered and not chosen:

- **Bitnami charts.** No longer updated, and their default images are in an
  unmaintained legacy repository (above).
- **An operator (CloudNativePG, a Redis operator).** Adds cluster-scoped CRDs
  and a controller, which ADR 0003 rules out, for HA and backups that
  production gets from RDS anyway.
- **Another community chart.** Another upstream to track and audit for two
  StatefulSets of about a hundred lines each, which the chart's own tests
  cover.
- **The `redis` image.** Redis 8's licenses need a legal review in a large
  company, and ElastiCache wouldn't run the same server.
- **PostgreSQL and Valkey as Docker containers beside kind**, like the SQL
  Server sandbox. They are part of what Atrium deploys, not something it
  tests against (ADR 0003).

## Consequences

- The chart gains two templates, values, schema entries and render tests,
  and `make kind-e2e` checks that the API reaches both (build slice 2.2).
- The full kind profile grows by one PostgreSQL and one Valkey pod. With the
  API they must fit the local loop's 2.5 GiB budget
  ([ADR 0003](0003-helm-on-kind-and-production.md)), measured under load
  when they land. They do: measured on 2026-11-02, figures in ADR 0003.
- Production needs RDS and ElastiCache in the infrastructure code and their
  Secrets in Secrets Manager [18].
- If the integration fixture's PostgreSQL major moves, the migration tests
  run against the new major in the same pull request.
- Valkey is protocol-compatible with Redis 7.2; Redis-8-only commands or
  modules are not available, and the API must not depend on them.

Update 2026-11-02: the templates landed with the API's chart pull request
[4.1]. Amazon RDS has supported PostgreSQL 18 since 14 November 2025 (its
[release calendar](https://docs.aws.amazon.com/AmazonRDS/latest/PostgreSQLReleaseNotes/postgresql-release-calendar.html)
lists 18.6 from 25 August 2026), but the integration fixture was still on
17.11, against point 6. The chart and the fixture moved to `postgres:18.6`
together, and the migration tests ran on it. Valkey runs 9.1.3, the version
of the API's integration tests; `tests/chart_engines_test.go` fails when the
chart and the tests disagree on either engine.
