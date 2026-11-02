-- name: CreateUser :one
insert into users (email, display_name, password_hash)
values ($1, $2, $3)
returning id, email, display_name, status;

-- name: GrantRole :one
-- Fails with no rows when the role does not exist.
insert into user_roles (user_id, role_id)
select @user_id, id
from roles
where name = @role_name
returning role_id;

-- name: GetLoginUser :one
-- The user an email logs in as, compared without regard to case, as the
-- unique index on lower(email) does.
select id, email, status, password_hash
from users
where lower(email) = lower(@email);

-- name: GetSessionUser :one
-- A user and the names of their roles, for each request with a session.
select u.id, u.email, u.display_name, u.status,
  coalesce(array_agg(r.name order by r.name) filter (where r.name is not null), '{}')::text[] as roles
from users u
left join user_roles ur on ur.user_id = u.id
left join roles r on r.id = ur.role_id
where u.id = @id
group by u.id;

-- name: RecordLogin :exec
update users set last_login_at = now() where id = @id;

-- name: UpgradePasswordHash :execrows
-- Replaces the hash only if it is still the one the login checked, so a
-- password changed meanwhile is kept.
update users set password_hash = @new_hash
where id = @id and password_hash = @old_hash;
