# 0022. Rename the project from Heartbeat to Atrium

- Status: Proposed
- Date: 2026-11-09

## Context

The project is renamed from Heartbeat to Atrium. The name is in more places
than prose: the GitHub repository `gdpais/Heartbeat`, the Go modules
(`module heartbeat`, `heartbeat/tools/patientzero`, `heartbeat/tools/docsite`)
and the `heartbeatconfig` import alias, the GHCR images
`ghcr.io/gdpais/heartbeat/{db-collector,api}` and the chart
`oci://ghcr.io/gdpais/charts/heartbeat`, the chart's directory, name,
template helpers, default Secret and ConfigMap names, and Grafana dashboard
UIDs, every metric (`heartbeat_*`), recording rule (`heartbeat:*`) and alert
(`Heartbeat*`), every environment variable (`HEARTBEAT_*`, including the
`HEARTBEAT_CREDENTIAL_<REF>` credential keys), the session cookie
`__Host-heartbeat_session` and Redis key prefixes, PostgreSQL's default
database and role names, the SQL Server login names in the scripts, the kind
cluster `kind-heartbeat` and its namespace, the release workflow's image
prefix, release-please's `package-name`, the docs site URL
`gdpais.github.io/Heartbeat`, the private `gdpais/heartbeat-deploy`
repository, the rulesets (`Heartbeat-tags`), and the local folder.

Three facts bound the change:

- `v0.1.0` is published as `ghcr.io/gdpais/heartbeat/*` and
  `charts/heartbeat`, and a published version is never rebuilt
  ([ADR 0006](0006-trunk-based-development-and-tagged-releases.md)).
- Nothing runs in production yet (phase 5 has not started). The only
  installs are kind clusters and anyone who tried `v0.1.0`, which shipped
  the collector, rules and dashboards but no API or PostgreSQL.
- GitHub redirects a renamed repository's web and git URLs, but not its
  Pages site, and GHCR package names and Go module paths do not follow a
  repository rename.

## Decision

Rename everything now, in one breaking change, with no compatibility
aliases. Before production, a rename costs one pull request and a reinstall
of a kind cluster; after it, every metric, alert, environment variable and
Secret would need a migration window.

- **Renamed:** the Go modules (`atrium`, `atrium/tools/...`), the GHCR image
  prefix `ghcr.io/gdpais/atrium/*` and chart `oci://ghcr.io/gdpais/charts/atrium`
  (directory `infra/helm/atrium`), metrics, rules, alerts, dashboards,
  environment variables, credential keys, default resource and Secret names,
  the cookie, the database defaults, kind's cluster and namespace,
  release-please's `package-name`, docs, ADRs (the name only; no decision
  changes), README, CONTRIBUTING and TODO. ADR 0018's file becomes
  `0018-atriums-own-telemetry.md`.
- **Kept:** `v0.1.0` stays where it is, under the Heartbeat names; its GHCR
  packages are not deleted, and the next release publishes new packages.
  `CHANGELOG.md`'s `0.1.0` entry, `docs/archive/`, and the sentences that say
  what `v0.1.0` published keep the old names, because they are history.
- **Version:** the rename is a `feat!` commit, so release-please's next
  version is `0.2.0`, the first Atrium release.
- **Upgrade path from 0.1.0:** `helm upgrade` the existing release with the
  new chart reference. Selector labels use the component name, not the chart
  name, so Deployments and StatefulSets are not replaced. Before upgrading,
  create the Secrets under their new names (`atrium-runtime`, or point
  `existingSecret` at the old ones) and rename credential keys from
  `HEARTBEAT_CREDENTIAL_*` to `ATRIUM_CREDENTIAL_*`. Metric history does not
  carry over: dashboards and alerts read the new names from the upgrade on.
  The install guide's *From 0.1.0* steps say this.
- **Order:** (1) the owner renames the repository to `gdpais/Atrium` and the
  tag ruleset to `Atrium-tags` in the GitHub UI; (2) the rename pull request
  merges, so its links point at a repository that exists; (3) the next
  release publishes `atrium/db-collector`, `atrium/api` and
  `charts/atrium`, which the owner makes public, as for `v0.1.0`. The rename
  lands before the first release-please pull request is merged, or after
  its release is tagged, never between: release-please rebuilds its pull
  request from `master` on every push, so the rename becomes part of it.

## Consequences

- Kind clusters are recreated: `kind delete cluster --name heartbeat`, then
  `make kind-up`. Local `.env.sqlserver-dev` files rename their
  `HEARTBEAT_CREDENTIAL_ENV_SQLSERVER_DEV` key.
- The docs site moves to `gdpais.github.io/Atrium/`; the old URL stops
  working, since Pages does not redirect.
- `gdpais/heartbeat-deploy` is renamed `gdpais/atrium-deploy` by the owner,
  and its values, inventory and Argo CD Applications follow in a pull request
  of their own there.
- The `master` ruleset keeps its name. The release GitHub App is named
  Atrium already and the workflow refers to it only by
  `RELEASE_APP_CLIENT_ID` and `RELEASE_APP_PRIVATE_KEY`, so it does not change.
- The local folder `~/projects/LifeLine/Heartbeat` can be renamed once no
  session is working in it; Claude Code's per-project memory is keyed on the
  path and moves with it.
- Users of `v0.1.0` keep pulling it from the old names; nothing breaks for
  them until they upgrade.
