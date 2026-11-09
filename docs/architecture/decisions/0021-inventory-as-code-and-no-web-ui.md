# 0021. Inventory as code; no Web UI

- Status: Accepted
- Date: 2026-11-04

## Context

[ADR 0020](0020-web-ui-served-by-the-api.md) chose a Web UI for operators,
and its first screens shipped [TODO 5.1–5.2]. Atrium's operators are its
maintainers, sysadmins and the DevOps team: a request to monitor an
application, add a database target or change a probe reaches them as a
ticket, and they already deliver the platform as code (Helm values, Argo CD,
infrastructure as code: ADRs 0003, 0005).

Two ways to change the same thing would remain:

- The runtime half of the inventory is already code: the collector's
  integrations are YAML ([ADR 0002](0002-runtime-config-in-yaml.md)), and
  approved probe assignments become a values file proposed as a pull request
  ([ADR 0014](0014-probe-assignments-to-runtime-config.md)). A UI change would
  go form → PostgreSQL → rendered YAML → pull request → Argo CD.
- A Web UI is a second front end to build, secure and keep current (Node, npm,
  React, Playwright, a CSP), beside Grafana, which every user of Atrium
  already works in.

Grafana cannot replace the UI for changes: it has no forms of its own, a form
plugin would call the API through Grafana's proxy, around the session and
`Origin` rules of [ADR 0013](0013-api-design-and-subsystem-boundaries.md) §8,
and a Grafana app plugin is the same React UI tied to Grafana's releases and
to one platform ([ADR 0011](0011-platform-agnostic-telemetry-boundary.md)).

The API's endpoints all need a login: a session from a local user's password
(argon2id), a role for each operation (`viewer` < `operator` < `admin`) and
an `Origin` on every change. They hold no risk a UI would remove.

## Decision

1. **No Web UI.** `apps/web`, its embed in the API, the API's static handler
   and Content-Security-Policy, the Node stage of the API image, the `web` CI
   job and the Playwright run in `kind-e2e` are removed. The API serves only
   `/api/v1/…`, `/metrics` and its health endpoints; any other path is a 404.
   ADR 0020 is superseded; React leaves the core stack of
   [ADR 0004](0004-core-technology-choices.md).
2. **The inventory is code.** Environments, applications, components,
   telemetry sources, database targets, probe definitions and probe
   assignments are declared per environment in one YAML file, the inventory,
   kept in the configuration repository (`atrium-deploy`, [TODO 18]) beside
   the environment's values. A change is a pull request: review approves it,
   git is its history, and that repository's CI validates it with a JSON
   Schema in `packages/config-schema` and the API's own checks, run offline
   (ownership, credential references only, probe limits, the collector's
   loader).
3. **The API applies the inventory.** The chart mounts the file into the API
   from a ConfigMap. The API reads it at start and again when it has changed
   (checked every 30 s, as it reads the integrations file for links), and
   reconciles PostgreSQL to it under an advisory lock, so one replica applies
   it at a time: rows are matched by the names they are already unique by (an
   environment's slug, an application's name in its environment, a target's
   name in its environment, …), created or updated to match, and a row the
   file no longer lists is disabled, never deleted. Each change is audited
   ([ADR 0016](0016-audit-records-through-an-outbox.md)) with the actor
   `config` and the file's revision. A file that fails validation changes
   nothing: the last applied inventory stays, the error is logged and
   counted, and readiness is unaffected, as for the collector's reload
   (ADR 0002). Probe assignments in the file are approved by the merge.
4. **One writer per row.** Rows carry who manages them, `config` or `api`.
   The API's create, update and delete endpoints stay, for kind, tests and
   future automation, but refuse a row the inventory manages (409, naming
   the file). The file never touches a row the API manages.
5. **Probe assignments render from the file.** `api inventory render` turns
   the inventory's assignments into the `managedCollectors` values file
   (ADR 0014's format and rules) without a database, so the pull request
   that changes an assignment carries the collectors it renders; the
   configuration repository's CI fails when the committed render is stale.
   This replaces ADR 0014's pull request opened by the API, and with it the
   API's GitHub App, for assignments the inventory manages. API-managed
   assignments keep ADR 0014's path; on kind, `make kind-assignments`.
6. **Grafana shows the inventory.** The API exposes the inventory as info
   metrics on `/metrics` (an application's environment, status and manager;
   a target's environment and engine; whether the collector observes each
   source), and a dashboard lists them beside the health they explain. No
   new datasource or credential: Prometheus already scrapes the API.
7. **The API keeps its login.** Local users, sessions, roles and the `Origin`
   check stay as ADR 0013 decides; scripts log in as kind-e2e does. Tokens
   for machine clients are added when the first one needs them, not before.

## Consequences

- A change to the inventory needs git and a review, which suits the people
  who make it; someone without git access asks them. Reviews replace the
  draft/approve states for config-managed assignments.
- The API's inventory endpoints and their tests stay; the reconciler calls
  the same validation, so the file and the API cannot accept different
  inventories.
- `atrium-deploy` becomes the place an environment is described, values
  and inventory together, and production delivery [TODO 18] no longer waits
  for a UI. The API needs no GitHub App while every assignment is in the
  inventory.
- ADR 0013's ownership rules hold: the inventory file joins the YAML and
  Kubernetes desired state, and PostgreSQL keeps the metadata the API
  serves, joins and audits, now reconciled from that file. Its Web UI row
  and its UI client of the OpenAPI contract are gone.
- Removing the UI removes Node and npm from the repository, the toolchain
  and CI, and the API image's Node stage; `kind-e2e` loses its browser run.
- Investigations, alerts and reports [TODO 6, 12–14] still need a way to be
  started and read. Each decides it with its API and worker, in Grafana or
  the API, and a new ADR if it needs more.
- The 5.1–5.2 screens are deleted; git keeps them, and ADR 0020 describes
  how to rebuild a UI if one is ever needed.

## Alternatives considered

- **Keep the Web UI.** A second front end for a handful of people who work
  in git, with changes that end in a pull request anyway.
- **Edit the inventory in Grafana** (a form plugin or an app plugin). See
  Context: around the API's security model, or the same UI inside Grafana.
- **A CLI on the API as the only way to change the inventory.** Keeps
  PostgreSQL as the source of truth, with no review and no history beyond
  the audit log; a pull request gives both.
- **The inventory file as the only writer, with the API's inventory endpoints
  read-only.** Simpler, but drops tested endpoints that kind, the end-to-end
  tests and later automation use; one writer per row gets the same safety.
- **Apply the file from a Job (an Argo CD sync hook) calling the API.** It
  would need API credentials and a hook; the API already reloads mounted
  configuration (the integrations ConfigMap, [TODO 4.9]).
