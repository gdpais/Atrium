# 0016. Audit records are written in the change's transaction and delivered as OTLP log records

- Status: Accepted
- Date: 2026-10-27

## Context

Phase 3 builds the API, the first component that changes Atrium's own
records, so it needs an audit trail [4.10]. What is already said about it:

- [Operator workflows](../workflows.md#audit): every operator or admin
  change appends one JSONL line (time, actor, action, entity type and ID,
  before and after, request ID, config version) to a dated file such as
  `audit/atrium-audit-YYYY-MM-DD.jsonl`; files are rotated and shipped to
  Loki or a SIEM [15.3]. Secrets never enter it.
- [ADR 0013](0013-api-design-and-subsystem-boundaries.md): "A request that
  writes several rows, and its audit record [4.10], runs in one transaction."
- [ADR 0001](0001-postgresql-metadata-only.md) and the
  [data model](../data-model.md): PostgreSQL holds no audit streams; an
  `audit_events` table only if relational audit search or compliance exports
  are needed [17].
- [ADR 0011](0011-platform-agnostic-telemetry-boundary.md): services emit
  only OTLP and Prometheus exposition; only the OTel Collector names a
  platform.

These don't fit together:

- A line appended to a file can't be part of a PostgreSQL transaction. If the
  file is written after the commit, a crash in between loses the record of a
  change that happened; if before, a rolled-back change leaves a record.
- The API is a Deployment that may run more than one replica. A file per pod
  needs a volume per pod (a Deployment can't give each replica its own
  `ReadWriteOnce` volume), or is lost with the pod on an `emptyDir`.
- Nothing in the stack ships container files or standard output: the
  platform receives OTLP from the services and scrapes Prometheus endpoints.
  Shipping files would mean a node log agent, which no other part of
  Atrium needs.

Phase 2 built an ordered log path that loses nothing across restarts: OTLP
to the OTel Collector, a persistent sending queue, Loki
([application observability](../application-observability.md)).

## Decision

1. **The record.** One JSON object per change: `id` (a UUID, unique per
   record), `time`, `request_id`, `actor` (user ID and email), `action`,
   `entity_type`, `entity_id`, `before`, `after`, and `config_version` when
   the change concerns runtime config. Defined as a JSON Schema,
   `audit_record.schema.json`, in `packages/telemetry-contracts`. A read is
   not audited; a failed login is, without the password.
2. **Written in the same transaction.** The API inserts the record into an
   `audit_outbox` table in the transaction that makes the change. A change
   that commits always has its record; one that rolls back has none.
3. **Delivered as OTLP log records.** A drainer in each API replica claims the
   oldest rows (`FOR UPDATE SKIP LOCKED`, in batches), sends them to the OTel
   Collector as OTLP/HTTP log records (the record as the body;
   `service.name = atrium-api`, `event.name = atrium.audit` and the
   record's `id` as attributes), and deletes them once the Collector accepts
   them. Delivery is at least once; the `id` identifies a duplicate. The
   Collector routes them to Loki through the same persistent queue as the
   OutSystems logs, and to a SIEM later by adding an exporter there.
4. **The outbox is a queue, not a store.** Rows live until the Collector
   accepts them, seconds normally. PostgreSQL answers no audit queries and
   keeps no history, so ADR 0001 holds. While the Collector is unreachable,
   changes still commit and rows wait. The API exports the outbox's row count
   and the age of its oldest row, and an alert fires when delivery has
   stalled for a few minutes, as `OutSystemsLogExportBlocked` does for logs.
5. **Searched where it lands.** Audit records are read in Grafana from Loki
   (`{service_name="atrium-api"} | event_name="atrium.audit"`). An
   audit search API and an `audit_events` table stay deferred [17].
6. **No secrets, tested.** Records hold credential references, never values;
   password hashes, session IDs and tokens never appear in `before` or
   `after`. A test changes each audited entity and fails if any of them shows
   up in a record.
7. **Retention is Loki's for now**: 15 days by default, 30 days in the
   production example. How long audit records must be kept is part of the
   compliance sign-off before production
   ([ADR 0012](0012-personal-data-in-telemetry.md), [9.2]); if it is longer,
   the Collector exports them to a SIEM or object storage as well.

Considered and not chosen:

- **Dated JSONL files.** Not atomic with the change, one file per replica
  needs a volume per pod, and shipping them needs a node agent.
- **Send OTLP straight after the commit.** No table, but a crash after the
  commit, or a Collector outage longer than the retry, loses the record of a
  change that happened.
- **An `audit_events` table as the store.** Atomic and searchable, but it
  makes PostgreSQL an audit store, which ADR 0001 keeps out until relational
  search or compliance exports are needed [17].
- **Standard output.** Nothing collects it.

## Consequences

- One expand migration adds `audit_outbox`; the API gains a small OTLP log
  exporter. The db-collector's (`internal/otlp`)
  is the model; sharing it is a refactor for that slice to weigh.
- The OTel Collector's NetworkPolicy admits the API on its OTLP port.
- Changes don't depend on the Collector being up; audit delivery does, and is
  alerted on.
- ADR 0013's sentence on the audit record now holds as written.
- The workflows doc's file-based audit, and TODO 15.3's file rotation, are
  replaced by this record; shipping to a SIEM becomes a Collector exporter.
- Collector reload results and collector add, remove and change decisions
  stay where they are: the collector's own logs and its
  `atrium_collector_config_*` metrics.
