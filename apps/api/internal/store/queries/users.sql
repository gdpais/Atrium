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
