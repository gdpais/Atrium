# 0018. Heartbeat's own telemetry

- Status: Accepted
- Date: 2026-11-02. Amended 2026-11-02: every trace is kept by default, not
  10%, after the volume was measured (decision 4)

## Context

Heartbeat is built to watch other systems, and little of that was turned on
itself. Before the API and Web work continued, it was assessed on 2026-11-01:

- The db-collector and the API expose useful Prometheus metrics, but nothing
  shows connection pools or OTLP export. Cycle time is a last-value gauge,
  which hides the tail, and no metric says which build is running.
- The services log JSON to stderr, so their logs are reachable only with
  `kubectl logs` and are gone with the pod. The OutSystems applications' logs,
  by contrast, are in Loki.
- There are no traces, and nothing to store them in
  ([TODO 3.6](../../../TODO.md) waited for a first source).
- The only dashboard for Heartbeat itself has one placeholder panel, and it has
  no objectives of its own.

[ADR 0011](0011-platform-agnostic-telemetry-boundary.md) already bounds the
answer: services emit only OTLP and Prometheus exposition.

## Decision

1. **Metrics stay on Prometheus `/metrics`.** An OpenTelemetry metrics SDK would
   duplicate a path that works and that every rule and dashboard reads.

2. **Logs go over OTLP in-process, and stderr stays.** A `slog` handler
   (`internal/telemetry`) writes each record to stderr as before and hands it to
   a bounded queue: at most 1,024 records and 8 MiB. A full queue drops the
   record and counts it. Logging never blocks a service and never depends on the
   Collector. A failing export warns on stderr only, at most once a minute, so
   it cannot feed itself. A filelog agent was the alternative: it would also
   catch crashes, but it needs a DaemonSet with host access in a chart hardened
   against exactly that. Output from before the exporter starts, and panics, stay
   in `kubectl logs`; restarts have an alert.

3. **Heartbeat's logs have their own Collector receiver and pipeline.** They go
   to port 4319 (`otlp/self` → `logs/self` → `otlp_http/self-logs`), never to
   4318. The OutSystems logs pipeline holds one request on disk and makes the
   receiver wait for it, which is what keeps a backlog in order
   ([application observability](../application-observability.md)). Sharing it,
   Heartbeat's logs would stall during the backlog they should explain, and take
   places in its drain. A routing connector could split them on the same port,
   but it would put new code in the middle of that path, and a second receiver
   leaves the path as it is. `telemetry` therefore reads only
   `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT`, not the general
   `OTEL_EXPORTER_OTLP_ENDPOINT`, so that a service cannot join the delivery path
   by accident.

4. **Traces use the official OpenTelemetry Go SDK and OTLP/HTTP exporter,**
   configured by the standard `OTEL_*` variables. In a scratch build this grows
   the collector's binary from 21.1 MB to 26.8 MB and brings gRPC into the build,
   which the OTLP logs client had kept out. That is accepted: a hand-written span
   exporter is more code to own for a binary size that costs nothing at run time.
   Sampling is parent-based by ratio, set by `telemetry.traces.sampleRatio`:
   everything by default. A trace is one collection cycle, a few a minute; on
   kind's full load Tempo stored about 80 MB a day
   ([what the telemetry costs](../../reference/metrics-and-endpoints.md#what-the-telemetry-costs)),
   and the production example makes about half as many spans. A 10% default,
   decided first, would have lost nine in ten of the failing cycles a trace is
   opened for, to save storage that costs cents a month on S3. An environment
   lowers the ratio in its values once the API's request traffic [4.1] makes
   storage matter.

5. **Tempo stores traces,** in single-binary mode, in the `full` profile and off
   in `minimal`. Heartbeat is the first trace source, so TODO 3.6 comes before
   the rest of 4.1. The Collector's exporter is named for its role, like the
   others (ADR 0011).

6. **Identity follows OpenTelemetry:** `service.namespace=heartbeat`,
   `service.name=db-collector|api`, `service.version`, `service.instance.id`,
   `deployment.environment.name` (`telemetry.environment` in the chart). The
   `heartbeat_service_info` gauge carries version, revision and environment. It
   cannot be a scrape label: the collector's series already use `environment`
   for the monitored target, and Prometheus would rename theirs.

7. **pprof is opt-in, on a loopback listener** (`HEARTBEAT_PPROF_ADDR`), reached
   with `kubectl port-forward`. It needs no token and no NetworkPolicy rule, and
   an address that is not loopback is refused. Continuous profiling is left out.

8. **Restarts come from `changes(process_start_time_seconds[1h])`.**
   kube-state-metrics stays off; the full profile has about 0.5 GiB of headroom
   ([ADR 0003](0003-helm-on-kind-and-production.md)), and Tempo needs it more.

9. **Heartbeat's dashboards and rules are its own,** separate from the OutSystems
   ones, sharing the multiwindow burn-rate pattern but no recording rules.

## Consequences

- Heartbeat's logs are in Loki under `service_namespace="heartbeat"`, with
  Loki's retention and access. They can name hosts, ports and logins, as the raw
  driver errors in `GET /admin/config` do, and never passwords
  ([ADR 0012](0012-personal-data-in-telemetry.md) covers access).
- Logs can be lost: a full queue, a Collector restart (the `logs/self` queue is
  in memory) or a Loki outage longer than the retry time. stderr still has them,
  and `heartbeat_telemetry_log_records_total` counts what the service dropped.
  That is the difference from OutSystems rows, which must not be lost.
- Records in Loki are cut at 4 KiB per value; stderr keeps the full text.
- The OTLP client moved from the db-collector to `internal/otlp`, since the API's
  audit outbox ([ADR 0016](0016-audit-records-through-an-outbox.md)) needs it too.
- A platform other than the bundled one repoints `otlp_http/self-logs` as it does
  `otlp_http/logs`
  ([`external-platform.example.yaml`](../../../infra/helm/values/external-platform.example.yaml)).
- The API has this wiring but is not deployed yet. The chart's API pull request
  [4.1] sets its environment variables and adds its scrape job.

Update 2026-11-02: the chart deploys the API with the db-collector's
variables, the `api` scrape job, and admission to the Collector's 4319 for
its logs and 4318 for its spans, which take the OTLP/HTTP receiver like the
collector's (point 4); `make kind-e2e` finds its logs in Loki, its metrics in
Prometheus and a request's trace in Tempo.
