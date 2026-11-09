# 0009. Remove the OTel gateway

- Status: Accepted
- Date: 2026-10-18

## Context

The OTel gateway (`services/otel-gateway`) was planned as a thin Go service
that parses what the stock OpenTelemetry Collector cannot, starting with
OutSystems. It never got a caller:

- `POST /v1/atrium/events` normalized one JSON event into the
  [application event contract](../../../packages/telemetry-contracts/src/application_event.schema.json)
  and returned it to the caller. Nothing was exported to the OTel Collector,
  and nothing sends those events.
- `POST /v1/atrium/alerts` was Alertmanager's default receiver. It counted
  each delivery and discarded it, so every alert except `Watchdog` reached a
  sink that looked like delivery and was not.

[ADR 0008](0008-outsystems-11-log-extraction.md) moved OutSystems 11 ingest to
the db-collector and left the gateway's future open until push sources
(TODO 3.4) start. The push sources in view already speak OTLP: OutSystems 11
Cloud log streaming and ODC's App Analytics stream point straight at the
OTel Collector's OTLP receiver, and their field mapping fits the Collector's
`transform` processor (OTTL).

Keeping the gateway still costs something on every change: a multi-arch image
built and published for each release, a Deployment, a scrape job and an `up`
alert in the chart, a kind NodePort, kind end-to-end runs on its changes, and
docs describing a component that does nothing.

## Decision

| Concern | Choice | Why |
| --- | --- | --- |
| The service | Remove `services/otel-gateway`: code, Dockerfile, chart template and values, scrape job, kind port, release image, CI path rule | It has no caller and no planned one that OTLP plus OTTL does not cover. |
| Alertmanager's default receiver | A receiver with no integrations (a null receiver) in the chart's defaults; environments configure real receivers, as the production example already does ([ADR 0005](0005-production-delivery-and-operations-defaults.md)) | Alerts stay visible in Alertmanager and Grafana without pretending to be delivered. Alert history for investigations (phase 7) goes through the API into PostgreSQL when that work starts. |
| Future push sources | OTLP into the OTel Collector, mapped with OTTL. A source that needs parsing the Collector cannot express gets its own decision then | Collector configuration first, as the guardrails already say. |
| Application event contract | Kept in `packages/telemetry-contracts` | It describes the normalized event the API and analysis jobs will read; it has no producer today. |
| `opentelemetry.endpoint` in the runtime config | Kept | The db-collector's `outsystems_logs` kind exports to it. |

Considered and not chosen:

- **Keep the gateway as the bridge for push sources.** It would need
  forwarding, auth, body limits, batching and config reload before carrying
  any traffic, all of which the OTel Collector already has.
- **Keep only the alert endpoint for phase 7.** A no-op sink until then, and
  phase 7 needs persisted alert events with ownership and audit, which belong
  in the API.
- **Drop both endpoints and keep an empty service.** All the release and
  chart cost, for no behavior.

## Consequences

- A release publishes the db-collector image and the chart. The
  `heartbeat/otel-gateway` package on GHCR only serves releases up to
  `v0.1.0`.
- Until an environment configures receivers, alerts other than `Watchdog`
  notify no one. This was already true in practice; the default no longer
  hides it.
- The minimal kind profile no longer has to override the default receiver.
- Adding a custom ingest service later is a new decision, with forwarding to
  the OTel Collector from the start.
