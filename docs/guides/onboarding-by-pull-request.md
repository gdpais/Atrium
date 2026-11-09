# Onboarding by Pull Request

How to add, change or retire an application, its telemetry source and its
database targets by editing the environment's inventory file and merging a
pull request ([ADR 0021](../architecture/decisions/0021-inventory-as-code-and-no-web-ui.md)).

Each environment has one inventory file. The API applies it to PostgreSQL,
and the rows it creates refuse the API's own writes, so the file is the only
way to change them. Every field and rule is in the
[configuration reference](../reference/configuration.md#inventory-file);
[`config/inventory.example.yaml`](../../config/inventory.example.yaml) shows
them all. This guide is the workflow around the file.

| Where the file lives | How it reaches the API |
| --- | --- |
| kind | `api.inventory` in [`infra/helm/values/kind.yaml`](../../infra/helm/values/kind.yaml), applied by `make kind-deploy` |
| Installed from a release | A file of its own, passed with `helm upgrade … --set-file api.inventory=inventory.yaml` |
| Production | `environments/<env>/inventory.yaml` in the private [`atrium-deploy`](https://github.com/gdpais/atrium-deploy) repository. Its `scripts/render.sh` writes the file as `inventory-values.yaml` (`api.inventory`) beside the render below, and Argo CD passes both as values files |

## 1. Prepare what the file refers to

The file holds references, never secrets, and the API checks only its own
rules. Before the pull request:

- **A database target** needs the collector's login on the server and its
  credential in the collector's Secret, under the `credential_ref` the file
  names: [onboarding SQL Server targets](database-targets.md), steps 1 and 3.
- **An OutSystems source** needs an `outsystems_logs` collector that reads its
  environment's logging database; those collectors stay hand-written in the
  `integrations` values ([ADR 0017](../architecture/decisions/0017-outsystems-sources-map-application-identity.md)).
  The source's `application_name` is the name OutSystems logs for the
  application.

## 2. Edit the file

Add the application with its components and telemetry source, and the
target with its probes, under the file's `version` and `environment`:

```yaml
applications:
  - name: Claims
    platform: outsystems
    owner_team: Claims squad
    components:
      - {name: Claims portal, type: reactive_web_app, runtime_name: ClaimsPortal}
    telemetry_sources:
      - type: outsystems
        ingest_mode: agentless
        config: {application_name: Claims}

database_targets:
  - name: claims-prod
    host: claims-sql.db.internal
    database_name: Claims
    credential_ref: env/claims-prod
    probes:
      - {name: blocking, interval_seconds: 30}
      - {name: waits, interval_seconds: 30}
```

A row is matched by its name ([how rows are matched](../reference/configuration.md#applying-the-file)),
so:

| To | Do |
| --- | --- |
| Change a row | Edit its fields; the API updates it in place |
| Retire an application, a source or a target | Set `status: disabled`, or remove it: either way the row is disabled, never deleted, and its history stays |
| Stop a probe | `enabled: false`, or remove it |
| Rename an application | Rename it: a new row is created and the old one disabled |
| Rename a target | Not possible: a target keeps its name, and a disabled one still holds its server, port and database, so a new name for the same database is refused. A new target under another name works only for another server or database |
| Take over a row made through the API | Not possible: a file that lists it is refused. A row with nothing depending on it can be deleted through the API first; a target with probe assignments cannot, and stays the API's |

## 3. Check and render

Run the same commands the configuration repository's CI runs. In
`atrium-deploy`, `scripts/render.sh` rewrites the renders and
`scripts/check.sh` runs every check, the collector's `db-collector
check-config` on the rendered chart included; both take `ATRIUM_DIR`, a
Atrium checkout at the commit in `ATRIUM_REF`. From an Atrium checkout:

```bash
go run ./apps/api/cmd/api inventory check -f inventory.yaml
go run ./apps/api/cmd/api inventory render -f inventory.yaml > managed-collectors.yaml
```

`check` names each problem with its place in the file, and never quotes a
credential reference. `render` writes the `managedCollectors` values file
that the target's probes make
([rendering the file's collectors](../reference/configuration.md#rendering-the-files-collectors)).
Commit it with the file: the reviewer sees the collectors the change runs,
and CI fails when the committed render is stale.

## 4. Open the pull request

The review is the approval: once merged, the file's probe assignments are
approved with no user named, and the change is audited with the file's
revision. Reviewers check:

- the target's host, database and `credential_ref`, and that the login and
  Secret exist (step 1);
- the probes and their interval against the
  [probe review checklist](../architecture/database-observability.md#probe-review-checklist);
- the rendered collectors in `managed-collectors.yaml`.

## 5. Apply

| Where | Steps |
| --- | --- |
| kind | `make kind-deploy`; for the target's probes, `make kind-assignments` |
| Installed from a release | `helm upgrade` with `--set-file api.inventory=inventory.yaml`, `-f managed-collectors.yaml` after your other values files |
| Production | Merge in `atrium-deploy`; Argo CD syncs the file and the render (by hand until the first production deploy is validated [18]) |

The API rereads the file every 30 seconds and applies a changed one in one
transaction. A file it refuses, because it fails validation or names a row
the API manages, changes nothing: the last applied inventory stays.

## 6. Verify

```bash
# The file applied: changed or unchanged, never invalid or error.
curl -s localhost:8081/metrics | grep '^atrium_api_inventory_applies_total'
# The rows, with who manages them (config for the file's).
curl -s localhost:8081/metrics | grep -E '^atrium_api_inventory_(application|source|database_target)_info'
# The probes, applied once the collector runs them.
curl -sS -b cookies 'localhost:8081/api/v1/probe-assignments?state=applied' | jq '.items[] | {id, collector_id}'
```

Each row is one series of the
[inventory info metrics](../reference/metrics-and-endpoints.md#inventory-info-metrics),
so Grafana lists it too; a source shows `observation="observed"` once its
collector reads its logs. The API's own change to one of these rows
answers 409, saying the inventory file manages it
([one writer per row](../reference/metrics-and-endpoints.md#inventory)).
Each change is audited in Loki with actor type `config`, the file's path and
its revision:

```logql
{service_name="atrium-api"} | event_name="atrium.audit" | json | config_version=~"sha256:.+"
```

When `atrium_api_inventory_applies_total{result="invalid"}` grows, the
API's log names each problem; fix the file in a new pull request. The
collector's own checks are in
[onboarding SQL Server targets, step 5](database-targets.md#5-verify).
