-- Expand only (CONTRIBUTING): a new table, a nullable column and a seed row,
-- so the API of the previous release keeps working beside this one.

-- Local logins (ADR 0013 §7): an argon2id hash in the PHC string format,
-- which carries its own parameters. Null for a user who cannot log in with a
-- password.
alter table users add column password_hash text;

-- The admin role `api users create-admin` grants. The other roles arrive
-- with the code that checks them [4.2].
insert into roles (name, description)
values ('admin', 'Every operation, users and roles included')
on conflict (name) do nothing;

-- Audit records waiting for the OTel Collector (ADR 0016): inserted in the
-- transaction of the change they record, deleted once the Collector accepts
-- them. A queue, not a store: nothing reads a row after it is delivered.
create table audit_outbox (
  -- Delivery order: the oldest first.
  seq bigint generated always as identity primary key,
  -- The record's id, which identifies a record delivered twice.
  id uuid not null unique,
  -- When the row was written, for the age of the oldest row.
  created_at timestamptz not null default now(),
  -- The record, valid against audit_record.schema.json.
  record jsonb not null
);
