-- Expand only (CONTRIBUTING): a check constraint loosened, so every row the
-- previous release writes still passes.

-- A probe assignment the inventory file manages is approved by the merge of
-- the file's pull request (ADR 0021 §3), not by a user: its approved_at is
-- set and its approved_by is null. An assignment the API manages keeps the
-- rule of 0006: approved by a user, with both set, in an approved state.
alter table probe_assignments drop constraint probe_assignments_approval_ck;
alter table probe_assignments add constraint probe_assignments_approval_ck check (
  (state in ('draft', 'rejected')) = (approved_at is null)
  and case managed_by
    when 'config' then approved_by is null
    else (approved_by is null) = (approved_at is null)
  end
);
