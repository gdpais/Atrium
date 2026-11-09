# 0017. An OutSystems 11 telemetry source maps an application to its log identity; the extraction collector stays in YAML

- Status: Accepted. Updated by [ADR 0021](0021-inventory-as-code-and-no-web-ui.md):
  there is no UI; the revisit trigger below now reads "if installations are
  onboarded from the inventory file".
- Date: 2026-10-27

## Context

Phase 3 registers telemetry sources through the API [4.4], and its exit
registers PatientZero with its source. What exists:

- `telemetry_sources` rows belong to one application (`application_id not
  null`), with `source_type`, `ingest_mode` (`otlp`, `file`, `api` or
  `agentless`) and a `config` JSON object; the environment is derived through
  the application ([data model](../data-model.md)).
- OutSystems 11 self-managed keeps every application's logs in one logging
  database per installation. One `outsystems_logs` collector reads all of it,
  sets `service.name` from each row's `Application_Name` and
  `deployment.environment.name` from its own `environment` setting
  ([ADR 0008](0008-outsystems-11-log-extraction.md); example in
  `infra/helm/values/production.example.yaml`). Its settings (log types,
  `lag`, `batch_limit`, time zone, the logging database and its credential)
  belong to the installation, not to an application.
- [ADR 0014](0014-probe-assignments-to-runtime-config.md) renders approved
  probe assignments into `sqlserver` collectors. It says nothing about
  `outsystems_logs`.
- What the API and later workers need from a source is which telemetry
  belongs to an application: deep links [4.9], investigations [6], alert
  policies [13] and reports [14] all filter Loki and Prometheus by
  `service.name` and `deployment.environment.name`.

So a source per application can't own an extraction collector: two
applications on one installation would read the same database twice.

## Decision

1. **A source is an identity mapping.** An OutSystems 11 source is
   `source_type = outsystems`, `ingest_mode = agentless` (Atrium reads the
   logging database; nothing is installed on the platform), with `config`
   holding `application_name`: the OutSystems `Application_Name`, which is the
   `service.name` of its logs and the application label of its derived
   metrics. The API validates `config` against a JSON Schema per source type
   in `packages/config-schema`. No migration.
2. **One path for the environment.** The source has no environment of its
   own; it is its application's. The environment's `slug` must equal the
   extraction collector's `environment`, the logs'
   `deployment.environment.name`. Within an environment, an `application_name`
   maps to one application.
3. **The extraction collector stays in YAML.** One `outsystems_logs`
   collector per installation, hand-written in the environment's values and
   reviewed there ([ADR 0002](0002-runtime-config-in-yaml.md)). Installations
   change rarely; applications change often, and the API manages those.
4. **The API checks the mapping is observed.** Using the same read-only
   access to the collector's redacted `GET /admin/config` as ADR 0014's
   `applied` check, the API shows whether an enabled `outsystems_logs`
   collector reports the source's environment. A source can be registered
   before extraction is set up; the check reports it, it doesn't block it.
5. **OTLP sources map the same way.** OutSystems Cloud and ODC, and other
   OTLP sources [3.4], send `service.name` themselves; their source carries
   the same `application_name`, with `ingest_mode = otlp`.

Considered and not chosen:

- **The API renders the `outsystems_logs` collector too.** The logging
  database would become a database target, the log types probe definitions
  assigned to it, and ADR 0014's renderer would emit an `outsystems_logs`
  collector for them. One onboarding path, but more renderer and state for
  something set up once per installation. Revisit, in a new record, if
  installations are onboarded from the UI [10.1].
- **One collector per source.** Reads the same logging database once per
  application.
- **Match applications by name**, with no mapping. Display names and
  OutSystems `Application_Name` differ, and renaming either breaks the link.

## Consequences

- Registering an OutSystems application is metadata only: no pull request,
  no reload. Adding an installation is a values change, as today.
- The data model's example for `telemetry_sources` changes from `ingest_mode
  = api` with `log_labels` to `agentless` with `application_name`.
- Deep links, and later investigations, alerts and reports, filter by the
  source's `application_name` and the environment's `slug`.
- An `application_name` that matches no logs shows nothing; the observed
  check covers the collector, not the name. Checking the name against Loki
  would need the API to query telemetry, which waits for the adapters
  investigations need [6].
