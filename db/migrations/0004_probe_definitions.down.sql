-- For tests and local use only (ADR 0013 §6). It removes every probe
-- definition: a built-in has no place in 0001's columns, and a custom one
-- would lose the built-in it overrides. It fails while an assignment
-- references one.
delete from probe_definitions;

alter table probe_definitions alter column timeout_ms set not null;
alter table probe_definitions drop constraint probe_definitions_kind_ck;
alter table probe_definitions drop column description;
alter table probe_definitions drop column overrides;
alter table probe_definitions drop column builtin;
alter table probe_definitions alter column query_template set not null;
alter table probe_definitions drop constraint probe_definitions_version_ck;
alter table probe_definitions alter column version type text using version::text;
