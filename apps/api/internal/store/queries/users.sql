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

-- name: RecordLogin :execrows
-- Records a login, only while the user is active and has the password hash
-- the login checked. A password change or a disabling that committed since
-- the check, or holds the row now, wins: the login records nothing and
-- fails, so it cannot start a session the change has already ended.
update users set last_login_at = now()
where id = @id and password_hash = @password_hash and status = 'active';

-- name: UpgradePasswordHash :execrows
-- Replaces the hash only if it is still the one the login checked, so a
-- password changed meanwhile is kept.
update users set password_hash = @new_hash
where id = @id and password_hash = @old_hash;

-- name: ListUsers :many
-- Every user with the names of their roles, by email; only those with
-- status when it is not null.
select u.id, u.email, u.display_name, u.status, u.last_login_at, u.created_at, u.updated_at,
  coalesce(array_agg(r.name order by r.name) filter (where r.name is not null), '{}')::text[] as roles
from users u
left join user_roles ur on ur.user_id = u.id
left join roles r on r.id = ur.role_id
where sqlc.narg(status)::text is null or u.status = sqlc.narg(status)
group by u.id
order by u.email;

-- name: GetUser :one
-- A user with the names of their roles.
select u.id, u.email, u.display_name, u.status, u.last_login_at, u.created_at, u.updated_at,
  coalesce(array_agg(r.name order by r.name) filter (where r.name is not null), '{}')::text[] as roles
from users u
left join user_roles ur on ur.user_id = u.id
left join roles r on r.id = ur.role_id
where u.id = @id
group by u.id;

-- name: LockUser :one
-- Locks the user's row until the transaction ends: a login racing the
-- change waits for it (RecordLogin).
select id from users where id = @id for update;

-- name: LockAdminRole :one
-- Serialises every change that could leave no active admin: each takes this
-- lock first, then counts the others (CountOtherActiveAdmins).
select id from roles where name = 'admin' for update;

-- name: CountOtherActiveAdmins :one
select count(*) from users u
join user_roles ur on ur.user_id = u.id
join roles r on r.id = ur.role_id
where r.name = 'admin' and u.status = 'active' and u.id <> @id;

-- name: UpdateUser :exec
update users set email = @email, display_name = @display_name, status = @status, updated_at = now()
where id = @id;

-- name: SetPasswordHash :execrows
-- Replaces the user's hash; when if_hash is not null, only if it is still
-- that one, as when the user changes their own password.
update users set password_hash = @new_hash, updated_at = now()
where id = @id and (sqlc.narg(if_hash)::text is null or password_hash = sqlc.narg(if_hash));

-- name: GrantRoleIfMissing :execrows
-- Grants the role; 0 rows when the user has it already or it does not
-- exist.
insert into user_roles (user_id, role_id)
select @user_id, id from roles where name = @role_name
on conflict do nothing;

-- name: RevokeRole :execrows
delete from user_roles
where user_id = @user_id and role_id = (select id from roles where name = @role_name);

-- name: RoleExists :one
select exists (select 1 from roles where name = @name);

-- name: TouchUser :exec
update users set updated_at = now() where id = @id;
