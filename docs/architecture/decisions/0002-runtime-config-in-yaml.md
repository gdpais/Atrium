# 0002. Runtime integration and collector config come from YAML/Kubernetes

- Status: Accepted. Updated by [ADR 0014](0014-probe-assignments-to-runtime-config.md)
  (approved probe assignments render to `managedCollectors`) and
  [ADR 0021](0021-inventory-as-code-and-no-web-ui.md) (the inventory is a
  file; there is no UI).
- Date: 2026-06-04 (recorded 2026-09-29)

## Context

Collectors must be added, changed and removed without redeploying the platform.
An earlier design stored collector instances and assignments in PostgreSQL,
which conflicted with Kubernetes as the deployment's desired-state source and
required the API to exist before any collection could run.

## Decision

`config/integrations.yaml` (delivered as a Kubernetes ConfigMap in clusters)
is the desired state for integration endpoints, Grafana URL templates,
notification channels, credential references and collectors (targets and
probes). Secrets are referenced, never embedded.

Services load it through the shared config manager in `internal/config`:

- parse into typed structs and validate before applying anything;
- compute a `config_version` hash;
- publish immutable snapshots; never mutate active config in place;
- diff collectors by stable ID and start, update, drain or stop only what changed;
- fail closed: an invalid candidate keeps the previous config, and a failed
  apply is rolled back.

Reload triggers: explicit `SIGHUP` and authenticated
`POST /admin/config/reload` (deterministic, preferred in production), and
optional polling for file changes (`ATRIUM_CONFIG_WATCH_INTERVAL`). ConfigMap
volumes update through symlink swaps, so watchers must watch the parent
directory and debounce.

Runtime state is exposed through `/metrics`, `/healthz`, `/readyz` and logs,
not stored in PostgreSQL.

## Consequences

- Collection works without the control plane, and config changes are
  reviewable in git.
- The `database_targets`, `probe_definitions` and `probe_assignments` tables
  exist for a future UI workflow but are not read by the collector. Reintroduce
  API-driven assignments only once that workflow exists, and keep desired state
  and observed state separate. How they will reach the collector:
  [ADR 0014](0014-probe-assignments-to-runtime-config.md).
- Onboarding a database is a config change today; see the
  [database targets guide](../../guides/database-targets.md).
- Credential rotation needed a restart while credentials came from
  environment variables. Since [2.5] the chart mounts the credentials Secret
  as files that the collector reads on every connection, so a new or rotated
  credential needs none
  ([credential resolution](../../reference/configuration.md#credential-resolution)).
