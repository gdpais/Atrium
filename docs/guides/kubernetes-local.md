# Kubernetes Delivery (Helm on kind)

Heartbeat is packaged as one Helm chart, used unchanged for kind, CI and
production with environment-specific values
([ADR 0003](../architecture/decisions/0003-helm-on-kind-and-production.md),
[ADR 0005](../architecture/decisions/0005-production-delivery-and-operations-defaults.md)).
To just run it, see [local development](local-development.md). This page
explains what is deployed and how to work with it.

## Layout

| Path | Contents |
| --- | --- |
| `infra/helm/heartbeat/` | The chart: Heartbeat templates, `values.yaml` defaults, `values.schema.json`, `Chart.yaml`/`Chart.lock` |
| `infra/helm/heartbeat/files/` | Prometheus rules (and their promtool tests) and Grafana dashboards, shipped as ConfigMaps |
| `infra/helm/values/` | Environment values: `kind.yaml`, `minimal.yaml`, `sqlserver-dev.yaml`, `production.example.yaml`, and `external-platform.example.yaml` (no bundled engines; the OTel Collector sends everything as OTLP to another platform, [ADR 0011](../architecture/decisions/0011-platform-agnostic-telemetry-boundary.md)) |
| `infra/kind/cluster.yaml` | kind cluster: one node, host port mappings on 127.0.0.1 |
| `infra/kind/e2e.yaml` | The temporary cluster of `make kind-e2e`: no host ports, and a 10 s kubelet sync so changed ConfigMaps and Secrets arrive sooner |
| `scripts/kind.sh` | Cluster, image and deploy workflow behind the `make kind-*` targets |
| `scripts/kind-e2e.sh` | Acceptance checks on an isolated, temporary cluster (`make kind-e2e`) |

## What the chart deploys

Heartbeat's own objects (fixed names; one release per namespace):

| Object | Notes |
| --- | --- |
| `StatefulSet/db-collector` | Always one replica: a replacement pod starts only after the old one is gone, so SQL targets are never polled twice. `podManagementPolicy: Parallel`, so an upgrade replaces a pod that never became Ready ([kubernetes/kubernetes#67250](https://github.com/kubernetes/kubernetes/issues/67250)); the field cannot change in place, so an install from chart 0.1.0 [deletes the StatefulSet once](install-from-release.md#upgrade). Liveness `/healthz` (process only), readiness `/readyz`. No config checksum: ConfigMap changes hot-reload without a restart |
| `Service/db-collector-headless` | Headless, `publishNotReadyAddresses`; Prometheus discovers the collector through it even while it is unready |
| `Service/db-collector` | Operator access (health, admin endpoints); ready pods only. kind: NodePort with `externalTrafficPolicy: Local`, so the NetworkPolicy sees the host's address |
| `NetworkPolicy/db-collector` | Ingress to port 8082 only from the bundled Prometheus and the `dbCollector.networkPolicy.scrapers` (a shared Prometheus or another platform's scraper) and `.operators` peers; no peers means deny all. kind adds every source outside the pod subnet `10.244.0.0/16` (the host through the loopback NodePort, and containers on the Docker `kind` network); the production example adds a VPN range. Kubelet probes and `kubectl port-forward` are not affected. Needs a CNI that enforces NetworkPolicy (kind's kindnet does) |
| `NetworkPolicy/otel-collector`, `/prometheus`, `/alertmanager`, `/loki` | Ingress to each bundled engine only from the components that use it, on their ports; see [network access](#network-access). Rendered only for an engine that is on |
| `ConfigMap/heartbeat-integrations` | `integrations.yaml`, rendered from the `integrations` values, with the `managedCollectors` list appended to its collectors ([collectors rendered by the API](../reference/configuration.md#collectors-rendered-by-the-api)) |
| `ConfigMap/heartbeat-prometheus-rules` | Rule files; Prometheus mounts them at `/etc/prometheus-rules` and its config reloader reloads on change. `prometheusRules.configReloadAlert.enabled: false` leaves out the config reload alert ([rules](../reference/metrics-and-endpoints.md#prometheus-scrape-jobs-and-rules)) |
| `ConfigMap/heartbeat-dashboards` | Dashboards; label `grafana_dashboard: "1"` for shared Grafanas with a dashboard sidecar |
| `PersistentVolumeClaim/otel-collector-queue` | The OTel Collector's logs export queue (`otelCollectorQueue.persistence`, 1 GiB): rows the Collector accepted survive its restart. Only with the Collector, so not in the `minimal` profile. kind uses its default `standard` (local-path) class, a directory on the node that lives as long as the cluster. Without persistence the queue is an emptyDir, which survives a container restart but not a new pod |

Third-party engines are pinned chart dependencies, each turned off with
`<name>.enabled=false` so an environment can use shared services instead:

| Dependency | Version | Engine | Notes |
| --- | --- | --- | --- |
| `prometheus` (prometheus-community) | 29.35.0 | Prometheus v3.15.0, Alertmanager v0.34.1 | Static and DNS discovery only, no cluster RBAC. Sends alerts to Alertmanager |
| `grafana` (grafana-community) | 13.2.7 | Grafana 13.2.3 | Stateless; datasources and dashboards from source |
| `loki` (grafana-community) | 18.13.7 | Loki 3.7.8 | Monolithic, filesystem storage locally |
| `opentelemetry-collector` (alias `otelCollector`) | 0.175.0 | otelcol-contrib 0.161.0 | One-replica StatefulSet (`otel-collector-0`), so only one pod at a time opens the export queue. Metrics to Prometheus, logs to Loki through a queue on disk |

Rules the chart keeps (checked by `make chart-check`): only namespaced
objects, no CRDs; deterministic rendering (no random values, lookups or
hooks), because Argo CD renders the chart itself; Secrets created outside the
chart and referenced by name; unknown values rejected by the schema, at the
top level and inside the chart's own components;
renders with Helm 3 and Helm 4.

## Network access

Every Heartbeat component and bundled engine except Grafana has an ingress
NetworkPolicy, so a pod in the namespace (or in another one) reaches only what
the chart lists. Loki holds raw user IDs, session IDs and client IPs, so who
reaches it is part of their protection
([ADR 0012](../architecture/decisions/0012-personal-data-in-telemetry.md)).
The engine policies are `backendNetworkPolicies` in the values; the
collector's is `dbCollector.networkPolicy`.

| Policy | Port | Admitted from | Why |
| --- | --- | --- | --- |
| `db-collector` | 8082 | Prometheus | Scrape |
| `otel-collector` | 4318 | db-collector | OTLP/HTTP logs from `outsystems_logs` collectors |
| | 8889 | Prometheus | Scrape of the Collector's Prometheus exporter (metrics pushed as OTLP) |
| `prometheus` | 9090 | Grafana | Prometheus datasource |
| `alertmanager` | 9093 | Prometheus | Alerts, and the `alertmanager` scrape job |
| `loki` | 3100 | OTel Collector | OTLP log pushes |
| | 3100 | Grafana | Loki datasource |
| | 3100 | Prometheus | The `loki` scrape job |
| | 9095, 7946 | Loki | Its own gRPC (compactor address) and memberlist ring, through its Services |

A row exists only while both ends are on: `minimal` renders no Loki or
Collector policy and admits neither to anything, a disabled scrape job admits
no Prometheus, and the external-platform example renders only the Collector's
policy, for the db-collector. An engine nothing uses denies all ingress
(`ingress: []`); an empty `from`, which would admit every source, is never
rendered. Peers match by the pods' `app.kubernetes.io/name` and `instance`
labels in the release namespace, so whoever may create pods there can claim
them: RBAC on the namespace is part of the access control.

Not covered, by design:

- **Grafana** has no policy. Whatever serves its UI (an ingress, a load
  balancer, the operator) must reach port 3000, and its login is the control.
  Anonymous viewing is on only in the kind values, so on kind any pod, and
  any container on the Docker `kind` network, can query Loki through
  Grafana's datasource; that is why kind holds only synthetic data. Grafana has no
  Alertmanager datasource and Prometheus does not scrape Grafana, so neither
  flow is admitted anywhere.
- **Kubelet probes** come from the pod's node, which NetworkPolicy does not
  filter: the collector's `:8082`, the Collector's `:13133`, Prometheus's
  `:9090` and its reloader's `:8080`, Alertmanager's `:9093` and its
  reloader's `:8080`, Loki's `:3100` and Grafana's `:3000`.
- **`kubectl port-forward`** connects from inside the pod, so NetworkPolicy
  never sees it. It is how operators reach Loki, and the others outside kind:
  `kubectl -n heartbeat port-forward svc/loki 3100`.
- **PatientZero** sends nothing to the engines: it writes its logs to SQL
  Server, and the db-collector reads them.

**The collector's egress.** The policies above limit what reaches each
pod. `dbCollector.networkPolicy.egress` also limits where the collector
connects, off by default and on in `kind.yaml` and the production example:

| Destination | Ports | Why |
| --- | --- | --- |
| `egress.dns` (kube-dns in `kube-system` by default) | UDP and TCP 53 | Name resolution |
| The bundled OTel Collector, while it is on | its OTLP/HTTP container port, 4318 by default | OTLP/HTTP logs |
| `egress.targets` | each `port` of the enabled collectors' targets (1433 while there are none) | The monitored SQL Servers and logging databases |
| `egress.extraRules` | as given | An OTel Collector outside the chart, a proxy |

NetworkPolicy matches addresses, not host names, so `egress.targets` are
peers, usually ipBlock CIDRs of the databases' subnets; with egress on and a
target configured, an install without them fails rather than cutting the
collector off its databases. On kind they are everything outside the pod
subnet, since the sandbox SQL Server is a container on the Docker `kind`
network: the collector can reach it, DNS and the OTel Collector, and times
out on any other pod or Service, which `make kind-e2e` checks.

`egress.dns` takes peers and allows port 53 only. Where cluster DNS is
something else, set `dns: []` and allow it in `egress.extraRules`: with
NodeLocal DNSCache, ipBlocks for its address (`169.254.20.10/32` by default)
and for the kube-dns Service IP; on OpenShift, the `openshift-dns` pods on
port 5353.

**Keep the collector's Service ClusterIP in production.** `/metrics` and
`/healthz` are unauthenticated and name the collectors, targets and
environments. A NodePort or LoadBalancer Service makes port 8082 reachable
from anything that reaches a node, and with `externalTrafficPolicy: Cluster`
the NetworkPolicy then sees the node's address instead of the client's, so
its peers no longer tell sources apart. `kind.yaml` uses a NodePort only
because kind binds it to `127.0.0.1`.

`backendNetworkPolicies.<engine>.extraPeers` admits more NetworkPolicyPeers
to an engine's ports (the Collector: 4317, 4318, and 8889 while its exporter
is on; Prometheus 9090; Alertmanager 9093; Loki 3100), such as a shared
Grafana or Prometheus in another namespace (a `namespaceSelector` and a
`podSelector` in one peer) or an operator range. `kind.yaml` adds every
source outside the pod subnet to Prometheus, Alertmanager and the Collector,
as for the collector, so the loopback NodePorts and `make health` keep
working; Loki has no NodePort and stays limited to its three clients.
`backendNetworkPolicies.enabled: false` removes the four engine policies.

On kind, the API server's service and pod proxy (which `make kind-e2e` uses)
also comes from the node, so it is not filtered either; in a cluster whose
control plane runs elsewhere it is. kind's kindnet enforces NetworkPolicy,
and `make kind-e2e` checks it: pods the policies do not list, in the
namespace and in another one with Grafana's labels, time out on the
collector, Loki, Prometheus, Alertmanager and the Collector, while the
flows above keep working. A cluster whose CNI does not enforce NetworkPolicy
accepts the objects and drops nothing.

## Secrets

The chart never creates Secrets. `scripts/kind.sh` creates kind-only values;
an install from a release creates its own
([installing a release](install-from-release.md#1-create-the-namespace-and-secrets)):

| Secret | Keys | kind value | Production |
| --- | --- | --- | --- |
| `heartbeat-runtime` | `admin-token` | `local-admin-token` | External Secrets Operator |
| `grafana-admin` | `admin-user`, `admin-password` | `admin` / `admin` | Generated, External Secrets Operator |
| `heartbeat-sqlserver-dev-credentials` | `HEARTBEAT_CREDENTIAL_ENV_SQLSERVER_DEV` | From `.env.sqlserver-dev` | Per-environment credential Secret |

## Images

The chart's default image is the release's,
`ghcr.io/gdpais/heartbeat/db-collector` at the chart's `appVersion`. On kind,
`kind.yaml` points it at the local `heartbeat/db-collector` instead:
`make kind-images` builds it for the node's CPU architecture (it refuses a
mismatch), tags it `dev-<first 12 hex of the image ID>` and loads it with
`kind load`. Pods use `imagePullPolicy: Never`, so the cluster can only run
what was loaded. The tag changes whenever the binary changes, so a code change
always produces a new pod. Production references ECR images by digest
(`image.digest`).

The tags are kept in `.tmp/kind/<cluster>/images.env`. `make kind-deploy`, and
everything that deploys through it (`make sqlserver-dev-up`, PatientZero),
rebuilds first when a file under `services/` or `internal/`, `go.mod` or
`go.sum` is newer than that file, so a pull or a branch switch never leaves the
current chart running an image built from older code. Unchanged code builds
from cache to the same tag, and nothing rolls out.

## The cluster

`make kind-up` creates `heartbeat` from the node image pinned in the Makefile
(`KIND_NODE_IMAGE`, the newest Kubernetes minor EKS supports) and
`infra/kind/cluster.yaml`. NodePorts map to the Compose-era host ports on
127.0.0.1 only. Every `kubectl`/`helm` call in the scripts names the
`kind-heartbeat` context, so they never act on another cluster.

SQL Server is not in the cluster: `make sqlserver-dev-up` runs it as a Docker
container on the `kind` network, and pods reach it by container name.

## Working with it

```bash
make kind-status                                   # pods and services
kubectl --context kind-heartbeat -n heartbeat logs statefulset/db-collector
kubectl --context kind-heartbeat -n heartbeat port-forward svc/loki 3100
helm --kube-context kind-heartbeat -n heartbeat get values heartbeat
EXTRA_VALUES=my-values.yaml make kind-deploy        # layer your own values
```

Change the deployed config through values and `make kind-deploy`. Helm 4
upgrades with server-side apply, so a field you edit by hand with `kubectl`
(for example the integrations ConfigMap) becomes owned by kubectl, and the next
upgrade fails with a conflict. Re-apply the chart's version with
`--server-side --field-manager=helm --force-conflicts`, or delete the object
and redeploy. In production Argo CD reverts such drift.

## Production

Production values live in the `heartbeat-deploy` config repository and Argo CD
renders the chart from them (ADR 0005).
[`production.example.yaml`](../../infra/helm/values/production.example.yaml)
shows the shape (ECR digests, External Secrets, gp3 volumes, S3 for Loki,
internal ALB for Grafana, Slack/Teams/WhatsApp and healthchecks.io receivers)
and is rendered in CI so it stays valid. kind does not test cloud storage,
networking, identity, ingress or TLS; those need testing in the target
environment.
