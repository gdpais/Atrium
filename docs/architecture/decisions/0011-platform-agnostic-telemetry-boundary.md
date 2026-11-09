# 0011. Keep the observability platform replaceable; bundle the Grafana stack

- Status: Accepted; the platform review before production is closed by [ADR 0019](0019-keep-grafana-and-share-telemetry-as-otlp.md)
- Date: 2026-10-19

## Context

[ADR 0004](0004-core-technology-choices.md) chose Prometheus, Loki, Grafana
and Alertmanager, and the chart bundles them. The organization standardizes
new observability work on the ELK stack in Elastic Cloud, so the platform
Atrium's data lands in may change. The project has no budget for one yet:
it runs on kind, with about $100 of AWS credit for later.

What was found on 2026-10-18 and 2026-10-19 (list prices; check them again
before deciding):

- Elastic Cloud has no free plan, only a 14-day trial. Serverless
  Observability bills per GB ingested (from $0.07–0.09) and retained (from
  $0.017–0.019 per month); Hosted bills provisioned capacity (about $99 a
  month for the smallest Standard deployment). Kibana on Elastic Cloud only
  comes with an Elastic Cloud deployment and cannot front a self-managed
  cluster.
- Self-managed Elasticsearch and Kibana are free under the Basic license, but
  Basic cannot notify (index and server-log actions only; email, Slack,
  webhook and PagerDuty are paid) and has no SLO burn-rate rules. They need
  about 4 GiB of RAM, twice the bundled Grafana stack.
- Elastic Cloud accepts OTLP directly (the managed OTLP endpoint, generally
  available on Serverless and on Hosted from 9.0), like most platforms.

Atrium's code was already close to neutral: the db-collector exposes
Prometheus metrics and sends logs as OTLP to the OpenTelemetry Collector, and
only the Collector and the chart name a backend. The coupling was the
integrations config, which required `grafana`, `loki` and `alertmanager`
blocks (nothing read them), push endpoints for Loki and Alertmanager that
nothing used, and chart names that assumed Prometheus.

## Decision

Keep the Grafana stack as the bundled platform, and keep everything else
independent of it:

1. **Atrium's services emit only open formats.** OTLP to the OpenTelemetry
   Collector, and Prometheus exposition on `/metrics`. They never write to a
   platform's own API.
2. **The Collector and the chart are the only places that name a platform.**
   Another platform is a values file: turn the bundled engines off, let the
   Collector scrape `/metrics` and send logs and metrics as OTLP
   ([`external-platform.example.yaml`](../../../infra/helm/values/external-platform.example.yaml)).
   Exporters are named for their role (`otlp_http/logs`), so such a file
   repoints them.
3. **The integrations config is neutral.** `platform` (`grafana`, or
   `generic` for a platform Atrium has nothing for) with `dashboards`,
   `logs` and `alerting`, each the platform's UI or query API. No push
   endpoints. `dbCollector.networkPolicy.prometheus` becomes `scrapers`.
4. **Content is per platform.** Dashboards, recording and alert rules, label
   or index policy, and deep-link templates are written for one platform.
   The catalog of what to measure and alert on stays neutral
   ([derived metrics](../application-observability.md#derived-metrics));
   Atrium ships content for Grafana only, until another platform is
   chosen.
5. **Future readers go through adapters.** The API's deep links, the session
   analyzer and reports will query logs and metrics through an interface,
   chosen by `platform`, with Grafana (Loki and Prometheus) the first
   implementation.

The platform is reconsidered before production (roadmap phase 5): the
organization's ELK against the Grafana stack, on the same data, with the
Collector sending to both.

## Consequences

- Moving to Elastic, or to any platform that takes OTLP, needs no service
  code: a values file, the platform's content, and its query adapter once the
  API exists.
- Phase 2 builds Grafana dashboards and Prometheus rules, as planned. They are
  the part a platform change rewrites; the derived metrics, extraction and
  queries are not.
- The integrations config is incompatible with earlier releases: rename
  `grafana`, `loki` and `alertmanager` to `dashboards`, `logs` and `alerting`,
  and drop `loki.endpoint` and `alertmanager.endpoint`. The loader rejects an
  unknown `platform` and the push endpoints.
- Traces follow the same rule when they arrive [TODO 3.6](../../../TODO.md):
  the Collector's traces pipeline exports through a role-named exporter, and
  the example sends them as OTLP too.
- Not covered: validating the Collector's config with the real binary in CI;
  both the default config and the example were checked with
  `otelcol-contrib validate` when this was written.
