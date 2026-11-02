-- name: InsertAuditRecord :exec
-- Adds a record to the outbox, in the transaction of the change it records
-- (ADR 0016).
insert into audit_outbox (id, record)
values ($1, $2);

-- name: ClaimAuditRecords :many
-- Locks the oldest rows no other drainer holds, up to the limit. The locks
-- last until the transaction ends: deleted once the Collector accepts the
-- records, or released for the next round.
select seq, id, record
from audit_outbox
order by seq
limit $1
for update skip locked;

-- name: DeleteAuditRecords :exec
delete from audit_outbox
where seq = any(@seqs::bigint[]);

-- name: AuditOutboxStats :one
-- The rows waiting and the age of the oldest, 0 when there is none.
select
  count(*)::bigint as rows,
  coalesce(extract(epoch from now() - min(created_at)), 0)::float8 as oldest_age_seconds
from audit_outbox;
