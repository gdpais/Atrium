# 0012. Keep user IDs, session IDs and client IPs raw in Loki; restrict who reaches them

- Status: Accepted; compliance sign-off required before production
- Date: 2026-10-21

## Context

OutSystems 11 log rows carry user IDs, session IDs, request keys and client
IPs. The extractor sends them to the OTel Collector, and Loki keeps them as
structured metadata, never as index labels
([application observability](../application-observability.md)). Whether to
keep them raw, replace them with a keyed hash (HMAC) or truncate them (for
example, the last octet of an IPv4 address) was left open in the
[product overview](../../product/overview.md#open-questions), and the
2026-10-19 security review flagged that any pod in the namespace can reach
Loki.

What each option costs:

- **Raw.** Investigations search for the user or session that reported a
  problem, by the value support gives them, and correlate it with the
  application's own screens and tables. Nothing to manage. The data is
  personal data wherever Loki or a copy of it goes.
- **Keyed hash.** Still correlates within Atrium, and the key can be
  rotated. But support has the raw value, so every search needs the key to
  hash it first, and Grafana's log views show only hashes. The key becomes a
  secret every environment must hold and protect, and anyone with the key
  and the data can reverse a known value anyway.
- **Truncate.** Loses the per-user and per-session answers the dashboards and
  investigations need (active users and sessions come from Loki [10.4]).
  Only fits client IPs, and only when nobody needs the exact address.

## Decision

1. **Keep user IDs, session IDs, request keys and client IPs raw** in Loki's
   structured metadata. They stay out of index labels and metric labels, as
   before.
2. **Restrict who can reach them.** NetworkPolicies allow only the components
   that use Loki to reach it [9.6]. Grafana, the only human path to the logs,
   needs a login outside kind (anonymous viewing is allowed only in the kind
   values, behind loopback ports). Later Atrium features that copy
   identifiers out of Loki (investigation evidence, reports) keep them under
   the same access rules and retention.
3. **The extractor stays able to hash before export.** If compliance asks
   for a keyed hash or truncation, it is added in the extractor behind a
   setting, without changing the pipeline, the labels or the queries' shape.
4. **Production needs compliance sign-off first.** Before the first
   production deploy [18], someone accountable for data protection confirms
   raw storage, Loki's retention and who has Grafana access, or asks for
   option 3. Until then raw identifiers live only on kind and in test
   environments with synthetic (PatientZero) or approved data.

## Consequences

- Investigations and dashboards keep working on the values support and the
  SRE team already use.
- Access is restricted in the chart: Loki admits only the OTel Collector,
  Grafana and Prometheus, and the other bundled engines only their own
  clients [9.6]. Operators reach Loki through Grafana or `kubectl
  port-forward` ([network access](../../guides/kubernetes-local.md#network-access)).
- Loki is a store of personal data: its retention, backups and access are
  part of the production review, and any application that receives the logs
  as OTLP ([ADR 0019](0019-keep-grafana-and-share-telemetry-as-otlp.md)) must
  carry the same access rules [18].
- `docs/architecture/workflows.md`'s "prefer hashed identifiers" rule now
  reads as "keep identifiers out of labels; raw values only behind access
  control" and points here.
- Switching to a keyed hash later re-keys every stored value: old and new
  logs don't correlate across the switch unless the old ones age out first.
