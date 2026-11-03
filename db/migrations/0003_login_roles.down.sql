delete from user_roles where role_id in (select id from roles where name in ('operator', 'viewer'));
delete from roles where name in ('operator', 'viewer');
