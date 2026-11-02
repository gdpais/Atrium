drop table if exists audit_outbox;
delete from user_roles where role_id in (select id from roles where name = 'admin');
delete from roles where name = 'admin';
alter table users drop column if exists password_hash;
