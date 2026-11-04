-- Expand only (CONTRIBUTING): new columns with defaults, a looser default and
-- an index. No released API writes probe_assignments, so no row can violate
-- the checks.

-- An assignment's approval state (ADR 0014 §4): workflow metadata, apart from
-- the desired state (the rendered values file, in git) and the observed state
-- (the collector's GET /admin/config). draft: created or changed, not yet
-- approved, and not rendered. approved: rendered into managedCollectors.
-- proposed: in an open pull request to the config repository. applied: the
-- collector runs it as rendered. rejected: refused, or its pull request
-- closed unmerged. failed: the config repository's checks, or the collector,
-- refused it. proposed and failed come with the pull request output; with
-- the output `none`, an assignment goes draft -> approved -> applied, or
-- draft -> rejected.
alter table probe_assignments add column state text not null default 'draft'
  check (state in ('draft', 'approved', 'proposed', 'applied', 'rejected', 'failed'));
-- Why an assignment was rejected or failed, or a link that says.
alter table probe_assignments add column state_detail text not null default '';
alter table probe_assignments add column pull_request_url text;
-- Who approved it, and when; null while it is not approved. A user is
-- disabled, never deleted, so the reference stays.
alter table probe_assignments add column approved_by uuid references users(id) on delete restrict;
alter table probe_assignments add column approved_at timestamptz;
alter table probe_assignments add column created_by uuid references users(id) on delete restrict;
alter table probe_assignments add constraint probe_assignments_approval_ck check (
  (state in ('draft', 'rejected')) = (approved_by is null and approved_at is null)
);

-- The probe policy of an assignment, as the collector reads it: null leaves
-- the timeout to the definition, then to the collector (half the interval, at
-- most 10s), and max_rows to the collector (10000).
alter table probe_assignments add column timeout_ms integer check (timeout_ms > 0 and timeout_ms <= 25000);
alter table probe_assignments add column max_rows integer check (max_rows > 0 and max_rows <= 100000);
alter table probe_assignments add constraint probe_assignments_interval_ck check (interval_seconds between 5 and 3600);

-- Renders and the applied check read the assignments in these states.
create index probe_assignments_state_idx on probe_assignments (state, database_target_id);
