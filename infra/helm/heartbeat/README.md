# Heartbeat Helm chart

Heartbeat's DB collector for SQL Server, its `integrations.yaml`, the API
with its PostgreSQL and Valkey, Prometheus rules and Grafana dashboards, plus
pinned Prometheus, Alertmanager, Grafana, Loki, Tempo and OpenTelemetry
Collector, each of which an environment can turn off. The bundled PostgreSQL
and Valkey are for trying Heartbeat out, without backups; production points
the API at managed services (`api.postgres`, `api.redis`).

**Guide:** [Installing a release](https://github.com/gdpais/Heartbeat/blob/master/docs/guides/install-from-release.md)
covers the Secrets, pinning the image digest, adding SQL Server targets,
upgrades (with a one-time step from 0.1.0) and uninstalling. For the docs that
match your version, open the same path at its `vX.Y.Z` tag.

## Before you install

The chart creates no Secrets. Create these in the release's namespace first,
or the pods that need them wait in `CreateContainerConfigError`:

| Secret | Keys | For |
| --- | --- | --- |
| `heartbeat-runtime` | `admin-token` | The collector's admin endpoints |
| `heartbeat-api-credentials` | `HEARTBEAT_CREDENTIAL_SECRET_API_POSTGRES`, `HEARTBEAT_CREDENTIAL_SECRET_API_REDIS` | The API's logins to PostgreSQL and Valkey, each `username:password` |
| `heartbeat-postgresql` | `password` | The bundled PostgreSQL's `heartbeat` role |
| `heartbeat-valkey` | `password` | The bundled Valkey's `default` user |
| `grafana-admin` | `admin-user`, `admin-password` | Grafana's admin login |

## Install

```bash
helm install heartbeat oci://ghcr.io/gdpais/charts/heartbeat --version X.Y.Z \
  --namespace heartbeat --wait --timeout 10m
```

Install one release per namespace; object names are fixed. The images
default to `ghcr.io/gdpais/heartbeat/db-collector` and
`ghcr.io/gdpais/heartbeat/api` at the chart's version. Pin the digests from
the release notes with `dbCollector.image.digest` and `api.image.digest`.
The API's pods apply its database migrations in an init container before
they start.

## Values

`helm show values` prints the defaults with their comments.
`values.schema.json` checks them: a misspelt or unknown key fails the
install instead of being ignored, at the top level and inside the chart's
own components (`dbCollector`, `api`, `postgresql`, `valkey`,
`podSecurityContext`, `containerSecurityContext`, `integrations`,
`managedCollectors`, `prometheusRules`, `grafanaDashboards`,
`backendNetworkPolicies`, `otelCollectorQueue`). The bundled engines' values
(`prometheus`, `grafana`, `loki`, `tempo`, `otelCollector`) are their charts'
own and are checked only as far as those charts check them.

`managedCollectors` is the list of collectors the Heartbeat API renders
from approved probe assignments, appended to `integrations.collectors`;
collector ids starting with `api-` are reserved for it
([collectors rendered by the API](https://github.com/gdpais/Heartbeat/blob/master/docs/reference/configuration.md#collectors-rendered-by-the-api)).

Source: <https://github.com/gdpais/Heartbeat>
