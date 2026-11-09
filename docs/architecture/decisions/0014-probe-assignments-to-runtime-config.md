# 0014. Approved probe assignments reach the collector as a pull request to the config repository

- Status: Accepted; amended 2026-11-04 and narrowed by
  [ADR 0021](0021-inventory-as-code-and-no-web-ui.md) (below)
- Date: 2026-10-21. Amended 2026-11-04, when the API side was built [4.5]:
  a target runs at one interval, so it renders to one collector (§2); the
  `none` output renders in the API's pod for `make kind-assignments`, beside
  the endpoint (§5); approval, changes and the applied check are spelled out
  (§4). Narrowed by [ADR 0021](0021-inventory-as-code-and-no-web-ui.md):
  assignments the inventory file declares render from that file in the
  configuration repository's own pull request, approved by its review; §3's
  pull request from the API, and its GitHub App, apply only to assignments
  the API manages.

## Context

Phase 3 lets operators register database targets and assign probes to them
in the API [4.5]. The DB collector must then run those probes. Constraints:

- The collector reads only `integrations.yaml`, mounted from the
  `heartbeat-integrations` ConfigMap, and hot-reloads it: file polling every
  5 s after the kubelet updates the volume (about a minute; 65 s measured on
  kind), or `POST /admin/config/reload`. An invalid file is rejected and the
  previous config keeps running ([ADR 0002](0002-runtime-config-in-yaml.md),
  [configuration reference](../../reference/configuration.md#validation-and-reload-behavior)).
- The chart renders that ConfigMap from the `integrations` values. Values
  files are layered and lists replace, not merge: a second file that sets
  `integrations.collectors` replaces the whole list.
- Production deploys through Argo CD from the `heartbeat-deploy` config
  repository ([ADR 0005](0005-production-delivery-and-operations-defaults.md)),
  which doesn't exist yet. Argo CD reverts changes made to the cluster
  outside git, so an API that writes the ConfigMap would be undone at the
  next sync, or would need Argo told to ignore it, and git would no longer
  be the desired state.
- On kind, `make kind-deploy` runs `helm upgrade` with `kind.yaml` plus the
  files in `EXTRA_VALUES`.
- The collector's model and the database's differ. A collector has one
  `scrape_interval`, a probe list and a target list; `probe_assignments` has
  an interval per assignment, `probe_definitions` carries a versioned
  `query_template`, and neither table has an approval state.
- Credentials are references (`env/…`, `kv/…`, `secret/…`). The collector
  resolves them from `HEARTBEAT_CREDENTIAL_*` variables loaded from one
  Secret, so a new reference needs a new Secret key and a collector restart.
  A missing one fails only that target's connection.

## Decision

1. **The API never writes cluster state.** It has no Kubernetes credentials
   and runs no Helm. Its output is a values file.
2. **One generated values file, owned by the API.** The API renders every
   approved, enabled assignment into one file holding a new chart key,
   `managedCollectors`, a list of collectors. The chart appends it to
   `integrations.collectors` when it renders the ConfigMap, so hand-written
   collectors stay in the environment's own values and the API never edits
   them. Rendering rules:
   - one `sqlserver` collector per target and interval, with the ID
     `api-<target id>-<interval>s`; changing an interval replaces that
     collector, as any reload does when an ID changes. Hand-written
     collector IDs may not start with `api-` (schema check). *Amended
     2026-11-04:* a target runs at one interval, so it has one collector.
     The collector refuses a target that two enabled collectors scrape
     (`validateUniqueSQLServerTargets`): its series carry only the
     environment and target labels, so two collectors would write the same
     ones. An approval is therefore refused while the target's other
     approved, enabled assignments run at another interval; to change it,
     change each assignment and approve them again;
   - targets carry name, host, port, database, environment name and
     `credential_ref`; probes are listed by name, and a custom
     `probe_definitions.query_template` is written out in full, so its SQL
     is reviewed in the pull request under the
     [probe review checklist](../database-observability.md#probe-review-checklist);
   - output is deterministic (sorted, no timestamps): the same approvals
     render the same bytes, so nothing changes when nothing changed;
   - the file is validated with the integrations schema and the Go loader
     before it leaves the API, and starts with a header saying it is
     generated.
3. **Approval opens a pull request; merging applies it.** In production the
   API pushes the rendered file to one branch it owns on `heartbeat-deploy`
   (for example `heartbeat-api/assignments`) and opens, or updates, one pull
   request against the environment's path. Pending approvals share that one
   pull request, so they never conflict with each other. The description
   lists each assignment, who approved it, and any credential reference new
   to the file. The config repository's own CI (chart render, schema, loader)
   and review rules apply; Argo CD syncs the merge and the collector reloads.
   Approval in the API records intent; the merged commit is the change, with
   its review trail.
4. **States are workflow metadata, separate from desired and observed
   state.** An expand migration adds to each assignment a state (`draft`,
   `approved`, `proposed`, `applied`, `rejected`, `failed`), the pull
   request URL and the approver. `applied` is set only when the collector
   reports the collector ID with the rendered targets and probes in its
   redacted `GET /admin/config`; the API holds the admin token as a Secret
   reference and reads nothing else from the collector. PostgreSQL holds the
   request, git the desired state, the collector the observed state
   (ADR 0002).
   *Amended 2026-11-04:* operators create and change assignments; admins
   approve and reject them, as admins own probe definitions. A draft or a
   rejected assignment can be approved. A change (interval, timeout, row
   limit, enabled, or another version of the same definition), or to its
   target's host, port, database or credential reference, puts an
   assignment back to `draft`, so the next render leaves it out until it is
   approved again: what runs is always what was approved. An approval renders
   the whole file in its transaction and checks it with the schema and the
   loader, so one that would make an invalid file is refused. The applied
   check runs every 30 s and moves `approved` to `applied` only; an
   assignment the collector stops running stays `applied`, and the
   collector's reload metrics and readiness say why (§7). Its audit records
   name the API itself (actor type `system`) as the actor.
5. **kind renders the same file.** The API's output target is configurable:
   `github` (above) or `none`, which only serves the rendered file at an
   admin endpoint. On kind, a make target (for example
   `make kind-assignments`) downloads it to an ignored path and runs
   `make kind-deploy` with it in `EXTRA_VALUES`. Same renderer, same chart
   key, no pull request, and the API still needs no access to the host.
   *Amended 2026-11-04:* `make kind-assignments` runs
   `api probe-assignments render` in the API's pod, which reads PostgreSQL
   only, rather than downloading from the endpoint, so kind needs no login.
   The file stays in the cluster's ignored state directory, and later
   `make kind-deploy` runs keep it, as they keep the SQL Server sandbox. The
   endpoint, `GET /api/v1/probe-assignments/rendered`, serves the same bytes
   to operators. `none` is the only output until the config repository
   exists, so there is no setting yet.
6. **Secrets are references only.** The API validates the reference format
   and renders references; it never sees or stores a credential. The value
   reaches the cluster on its own path: External Secrets from AWS Secrets
   Manager in production, a `kubectl`-created Secret on kind
   ([database targets guide](../../guides/database-targets.md#3-provide-the-credential)).
   Until the collector reads credentials from mounted files (the ADR 0002
   follow-up), a new reference also needs a collector restart, so the pull
   request flags it. A target whose credential is missing fails on its own,
   visibly in `GET /admin/config`; other targets keep running.
7. **Drift and reconciliation.**
   - Cluster against git: Argo CD's self-heal.
   - Git against PostgreSQL: on every approval and on a schedule, the API
     renders the file and compares it with the default branch. A difference
     it didn't cause (a hand edit to the generated file) is reported and
     overwritten by its next pull request.
   - Collector against git: the collector's `last_reload_err`,
     `runtime_diverged` and readiness, and the API's `applied` check.
8. **When it doesn't go through.**
   - Pull request closed without merging: its assignments become `rejected`
     with the link, and later renders leave them out. To retry, edit and
     approve again.
   - The config repository's checks fail: the assignments become `failed`
     with the check's link; the pull request stays open for the next render
     to fix.
   - Merged, but the collector rejects or fails to apply the file: it keeps
     the previous config, the assignments become `failed`, and the fix is a
     revert or a new pull request.
   - GitHub unreachable: the assignments stay `approved` and the API retries
     with backoff; nothing is lost.

Considered and not chosen:

- **The API writes the ConfigMap or patches the collector.** Argo CD reverts
  it, or must ignore it; the API would need cluster write access; no review
  trail; git stops being the desired state.
- **The API commits straight to the config repository's default branch.**
  Simpler, but it bypasses the repository's review and checks, and
  "approved" means one click.
- **The collector reads assignments from the API or PostgreSQL.** Reverses
  ADR 0002: collection would depend on the control plane being up.
- **An Argo CD plugin or ApplicationSet generator that asks the API.** The
  desired state would live in a database, not in git.
- **The API rewrites `integrations.collectors` directly.** Lists replace, so
  it would have to own every collector, hand-written ones too.

## Consequences

- Assigning a probe takes a review and a sync, minutes rather than seconds.
  That is the point for production; on kind the make target keeps it to one
  command.
- The chart gains `managedCollectors` (schema, render test, the `api-`
  prefix rule). The integrations file format itself doesn't change.
- To build the GitHub path, the API needs write access to `heartbeat-deploy`:
  a GitHub App installed on that repository only, with contents and pull
  requests read and write, its private key in AWS Secrets Manager through
  External Secrets [18]. Not set up now; until then the output target is
  `none`.
- The config repository needs CI that renders the chart with each
  environment's values and loads the result with the Go loader, and branch
  protection, before the API opens pull requests against it [18].
- The API needs a network path to the collector's admin port, allowed in the
  collector's NetworkPolicy.
- The same pattern (render to a file in the config repository, review, sync)
  fits rendered Prometheus rules and Alertmanager routes later [4.7]; that is
  a separate decision.
- `outsystems_logs` collectors are not rendered: an OutSystems source maps an
  application to its logs, and the extraction collector stays in YAML
  ([ADR 0017](0017-outsystems-sources-map-application-identity.md)).
