-- 0006's rule again. An assignment the inventory file approved names no
-- user, which that rule refuses, so it goes back to draft: unrendered until
-- the file is applied again by a release with 0008.
update probe_assignments set state = 'draft', state_detail = '', approved_at = null
where managed_by = 'config' and approved_by is null and approved_at is not null;
alter table probe_assignments drop constraint if exists probe_assignments_approval_ck;
alter table probe_assignments add constraint probe_assignments_approval_ck check (
  (state in ('draft', 'rejected')) = (approved_by is null and approved_at is null)
);
