-- Expand only (CONTRIBUTING): seed rows, so the API of the previous release
-- keeps working beside this one.

-- The roles below admin (ADR 0013 §8). Roles rank viewer < operator < admin;
-- each API operation declares the least one it needs (apps/api/openapi.yaml).
insert into roles (name, description)
values
  ('operator', 'Changes inventory, telemetry sources, database targets and probes; not users or roles'),
  ('viewer', 'Reads everything; for wallboards and people who only look')
on conflict (name) do nothing;
