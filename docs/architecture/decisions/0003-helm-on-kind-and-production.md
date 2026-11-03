# 0003. Deliver the platform with Helm on kind and production Kubernetes

- Status: Accepted. Implemented for kind and CI on 2026-10-03; production
  delivery follows [ADR 0005](0005-production-delivery-and-operations-defaults.md).
- Date: 2026-09-29

## Context

Local development uses Docker Compose (`infra/docker-compose.yml`, 9 services).
A small Kustomize bundle (`infra/k8s/local`) deploys only the DB collector,
the gateway and PostgreSQL. Production is intended to run on cloud Kubernetes.
Keeping Compose for development and a separate production definition means two
deployment systems that drift, and Compose never exercises Kubernetes behavior
such as pod replacement, readiness routing, projected ConfigMaps and rollouts.
The collector hardening work depends on exactly that behavior.

## Decision

- Package Heartbeat with **Helm** and use the same versioned chart locally, in CI
  and in production, with environment-specific values.
- Use **kind** for the local and integration-CI cluster.
- Keep **SQL Server in Docker** as an external test target, outside the cluster.
- Adapt Make targets and CI as part of the migration, then **retire the
  platform Compose definition** once equivalent workflows pass. Do not complete
  the Kustomize bundle first and translate it again later.
- **Principle:** what Heartbeat *deploys* goes through Helm; what Heartbeat
  *tests against* (the SQL Server target, the disposable PostgreSQL schema
  fixture) stays as Docker fixtures.
- Keep the **collector a singleton** until explicit target ownership across
  replicas exists; never scale it by raising the replica count.

## Proposed design (as planned; see Implementation for what changed)

- One umbrella chart `heartbeat` owns Heartbeat workloads and content
  (collector, gateway, integrations config, Prometheus rules, Grafana
  dashboards). Third-party engines are pinned, condition-gated dependencies,
  using namespaced charts only (`prometheus-community/prometheus` with
  Alertmanager, `grafana/grafana`, Loki single binary, OpenTelemetry Collector),
  with no cluster-scoped CRDs. Production can disable any engine and point at
  shared services.
- The collector runs as a StatefulSet with one replica, which gives at-most-one
  semantics during node partitions. A headless Service with
  `publishNotReadyAddresses` lets Prometheus scrape it via `dns_sd_configs`
  even when it is unready.
- Values files embed complete `integrations.yaml` documents per environment,
  validated by the same schema and Go loader.
- Two local profiles: `minimal` (collector, Prometheus, Alertmanager, Grafana)
  and `full` (adds Loki, OTel Collector, gateway). PostgreSQL and migrations
  stay out of the deployed stack until the API exists.
- Grafana is stateless; dashboards come from source.
- Access uses kind `extraPortMappings` on the existing Compose ports instead of
  `kubectl port-forward`.
- The Prometheus `alerting` block and a Watchdog rule (external dead-man's
  switch) close the current Prometheus → Alertmanager gap.

## Consequences and risks

Ranked by effect on the goal:

1. **Monitoring failure domain.** If Heartbeat and its alert path run on the
   infrastructure being investigated, a failure there removes the evidence too.
   Keep an external availability check or notification path; a separate
   namespace is not a separate failure domain.
2. **Collector ownership and database load.** Kubernetes can overlap or replace
   pods, but it does not know who owns a SQL target. Control update overlap and
   test query budgets.
3. **Durability.** PVCs are not backups. Metadata and telemetry need separate
   retention and restore policies, tested independently of pod startup.
4. **Environment fidelity.** kind validates objects and application behavior,
   not cloud storage, networking or identity. Test production values, ingress,
   TLS and SQL connectivity in the target environment.
5. **Release boundaries.** A Helm rollback does not reverse schema changes or
   restore telemetry. Pin dependencies; keep app, schema and stateful upgrades
   separate.
6. **Development cost.** A local cluster adds CPU/RAM and concepts. Keep unit
   tests fast, automate the loop, and measure the cost.

## Acceptance criteria

The migration is done when these pass with retained evidence:

- A fresh cluster loads both images on the node's actual CPU architecture and
  every workload becomes ready.
- The same chart renders for kind, CI and production values without separately
  maintained production templates.
- A pod reaches the Docker SQL target (DNS, TCP, authentication, TLS) without
  widening database exposure.
- A code change produces a new running image; a stale `:local` tag cannot mask it.
- With one SQL target configured, `heartbeat_collector_target_up` and probe
  metrics are fresh in Prometheus and Grafana shows the target.
- A valid ConfigMap update changes the active config version. An invalid one is
  rejected visibly while collection continues. A partial reload failure rolls
  back or reports divergence and unreadiness.
- An unavailable SQL target shows as failed, healthy targets continue, and the
  collector does not restart just because a database is down.
- A singleton collector update drains the old pod before the new one polls; the
  collection gap and SQL-side concurrency are measured.
- A stateful restart keeps data designated as persistent; concurrent CI runs
  are isolated and cannot delete developer resources.
- The daily development loop meets an agreed time and resource budget.

## Implementation (2026-10-03)

The chart is [`infra/helm/heartbeat`](../../../infra/helm/heartbeat), values are
in [`infra/helm/values`](../../../infra/helm/values), and the workflow is
`make kind-*` ([guide](../../guides/kubernetes-local.md)). The proposed design
was followed, with these differences:

- Object names are fixed (`db-collector`, `prometheus`, `alertmanager`, `loki`,
  `otel-collector`, ...) and one release runs per namespace, so in-cluster
  addresses match the Compose-era names that `integrations.yaml` and the engine
  configs already used.
- Grafana and Loki come from the `grafana-community` chart repository, where
  Grafana moved those charts. Every engine chart pins the same engine version
  Compose ran.
- The integrations schema is embedded in the chart's `values.schema.json`, so
  Helm rejects invalid config before applying anything; a Go test keeps the
  copy in sync and loads each rendered profile with the Go loader.
- Loki has no host port: its chart cannot set a NodePort. It is reached through
  Grafana or `kubectl port-forward`.
- PostgreSQL, Redis and the platform Compose stack were removed. Compose
  remains only as the disposable PostgreSQL test fixture.
- Helm 4 upgrades with server-side apply, so a hand edit to a chart-owned field
  makes the next upgrade conflict until the chart's version is re-applied.

Acceptance criteria and how they are checked (`make chart-check` and
`make kind-e2e`, both in CI). Measurements are from one kind run on an AMD64
host; the e2e script prints them on every run.

| Criterion | Check | Result |
| --- | --- | --- |
| Fresh cluster loads both images on the node's architecture; all workloads ready | e2e; `kind.sh` refuses an architecture mismatch | All 7 workloads ready; full deploy about 50s after images load |
| One chart renders for kind, CI and production values | chart-check over 5 profiles, Helm 4 and Helm 3 | Pass |
| A pod reaches the Docker SQL target (DNS, TCP, auth, TLS) without widening exposure | e2e; SQL Server on the kind network, no host port in CI, loopback only locally | Target up |
| A code change produces a new running image | Content-derived tags, `imagePullPolicy: Never`; e2e compares the running image to the built tag | Pass |
| Target metrics fresh in Prometheus; Grafana shows the target | e2e queries Prometheus and Grafana's datasource proxy | Pass |
| Valid ConfigMap update changes the config version | e2e, same pod | About 65s (kubelet sync dominates) |
| Invalid ConfigMap update rejected visibly while collection continues | e2e checks `last_reload_err`, version and freshness | Reported after about 90s; collection uninterrupted |
| Partial reload failure rolls back or reports divergence | Unit tests in the collector (not repeated on kind) | Pass |
| Unavailable target shows failed; others continue; no restart | e2e stops the SQL container; a second, unresolvable target stays failed | Down after about 20s, recovered about 60s after restart, restartCount 0 |
| Singleton update drains the old pod first; gap and SQL concurrency measured | e2e samples running pods and SQL Server sessions every second during a rollout | Never more than 1 collector or 2 sessions; collection gap 12s and 31s in two runs at a 20s scrape interval |
| Stateful restart keeps persistent data | e2e replaces the Prometheus and Alertmanager pods | TSDB and silences kept |
| Concurrent CI runs isolated, cannot delete developer resources | e2e uses its own cluster name, kubeconfig and container names, and deletes only those | By construction |
| Daily loop meets an agreed budget | Budget agreed 2026-10-28: the full profile holds at most 2.5 GiB of RAM in the kind node, the SQL Server sandbox and PatientZero not included. CI's `kind end-to-end` job times out after 25 minutes | Full profile with the API, PostgreSQL and Valkey 1.85 GiB at its peak under `make patientzero-load LOAD=steady` (2026-11-02, method below); before them about 2.0 GiB RAM in the kind node, measured idle, and 1.8 GiB after 12 hours up (SQL Server sandbox +0.9 GiB at first, 2.2 GiB after 12 hours: its 2 GiB cap limits SQL Server's own memory, not the container's); `kind end-to-end` 10–12 minutes in CI on 21–22 October, up to 18.6 on 28 October; redeploy under a minute; `minimal` profile for smaller machines |

Update 2026-10-28: the local loop's budget is agreed (last row above).

- Memory is the peak `docker stats` usage of the kind node container
  (`heartbeat-control-plane` for the default `KIND_CLUSTER`) over at least
  30 minutes of `make patientzero-load` after a deploy, less the
  `patientzero` namespace: PatientZero and its k6 Job (up to 512 MiB) run in
  the same node but are not part of the profile, and
  `docker exec heartbeat-control-plane crictl stats --label io.kubernetes.pod.namespace=patientzero`
  shows their share. Load grows Prometheus's head block and Loki's
  ingesters until they flush, so an idle reading understates it; the
  2.0 GiB above was measured idle.
- The 4.1 chart change that adds the API, PostgreSQL and Valkey was the
  first measured this way (2026-11-02, chart at `8dd7efe`): the full profile
  with and without them, one after the other on the same fresh node, with
  `docker stats` every 10 seconds under `LOAD=steady`. With them the peak
  was 1,892 MiB (1.85 GiB) over 32 minutes, without them 1,756 MiB
  (1.71 GiB) over 25 minutes: 136 MiB more at the peak, 82 MiB more on
  average, and 0.65 GiB under the budget. Their own containers held 47 MiB
  of it (API 17, PostgreSQL 18, Valkey 12 MiB, from `crictl stats`); the
  rest is the node's per-pod overhead and the longer window.
- The budget gives up a second local cluster. A sandbox that has run for
  hours holds about 2.2 GiB, so the budget plus the sandbox is 4.7 GiB,
  which leaves the rest of this project's Docker host (7.6 GiB in
  `docker info`) for image builds. A second cluster with its own fresh SQL
  Server, such as `make kind-e2e`, adds about 3.4 GiB at the budget:
  8.1 GiB in all, more than the host has. Today's profile, 1.85 GiB at its
  peak, still fits, with about 0.8 GiB to spare (two profiles, the
  sandbox's 2.2 GiB and the second cluster's fresh SQL Server's 0.9 GiB). Run the second cluster's checks in CI,
  or `docker restart heartbeat-sqlserver-dev` first, which frees the
  sandbox's memory and keeps its databases. On 2026-10-22, before SQL
  Server's memory was capped, a load flood and a second cluster each got it
  killed.
- Time is bounded by CI, not by this budget: the `kind end-to-end` job
  fails after 25 minutes. It grew with its checks, not its pods, so when it
  nears the timeout the levers are the checks: shorter `wait_for` windows,
  waits run in parallel, or the job split in two.

A change that would exceed the budget is trimmed first: smaller requests, or
the component off in the `kind` profile. Otherwise it asks for a new budget
instead of growing past this one.

Not covered by kind (risk 4): cloud storage classes, ingress and TLS, IAM and
External Secrets, ECR pulls, and SQL connectivity from the EKS VPC.

## Open decisions

Production Kubernetes distribution and cloud; image and chart registry; deploy
mechanism (GitOps or CI); where production values live; dead-man's-switch
service; secrets backend; Grafana SSO provider. These are tracked in
[TODO.md](../../../TODO.md).

Update 2026-09-30: decided in
[ADR 0005](0005-production-delivery-and-operations-defaults.md) (AWS EKS, Argo CD,
ECR, AWS Secrets Manager, alert channels, Grafana access).
