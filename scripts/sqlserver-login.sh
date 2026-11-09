#!/bin/sh
# Creates the least-privilege collector login in a local SQL Server container,
# or resets the password of an existing one, with exactly the grants
# documented in docs/guides/database-targets.md: VIEW SERVER STATE and VIEW
# ANY DEFINITION, plus what every login has (CONNECT SQL, public).
#
# Every check runs before anything changes. It refuses sa, a name or password
# longer than 128 characters, and an existing login that is in any server role
# or holds any server permission beyond CONNECT SQL and the two grants. The
# changes run in one transaction that is rolled back if the login ends up
# sysadmin-equivalent (a member of sysadmin, or holding CONTROL SERVER), for
# example through a permission granted to public. sa is used only here, for
# setup; the collector never logs in as sa. Safe to rerun.
#
# Usage: scripts/sqlserver-login.sh <container>
#
# Environment (all required; passed to sqlcmd through the environment, never
# on a command line):
#   MSSQL_SA_PASSWORD             sa password of the container
#   ATRIUM_COLLECTOR_LOGIN     collector login name (not sa)
#   ATRIUM_COLLECTOR_PASSWORD  collector login password
set -eu

container=${1:?usage: scripts/sqlserver-login.sh <container>}
: "${MSSQL_SA_PASSWORD:?set MSSQL_SA_PASSWORD}"
: "${ATRIUM_COLLECTOR_LOGIN:?set ATRIUM_COLLECTOR_LOGIN}"
: "${ATRIUM_COLLECTOR_PASSWORD:?set ATRIUM_COLLECTOR_PASSWORD}"

# sqlcmd substitutes $(NAME) from the environment into the batch as plain
# text, inside N'...' literals, so any quote is doubled first. The quoted
# heredoc keeps the shell from expanding $(NAME) itself.
sql_quote() { printf '%s' "$1" | sed "s/'/''/g"; }
ATRIUM_COLLECTOR_LOGIN=$(sql_quote "$ATRIUM_COLLECTOR_LOGIN")
ATRIUM_COLLECTOR_PASSWORD=$(sql_quote "$ATRIUM_COLLECTOR_PASSWORD")
export ATRIUM_COLLECTOR_LOGIN ATRIUM_COLLECTOR_PASSWORD
SQLCMDPASSWORD=$MSSQL_SA_PASSWORD docker exec -i \
	-e SQLCMDPASSWORD -e ATRIUM_COLLECTOR_LOGIN -e ATRIUM_COLLECTOR_PASSWORD \
	"$container" /opt/mssql-tools18/bin/sqlcmd -S localhost -U sa -C -b -h -1 <<'SQL'
SET NOCOUNT ON;
SET XACT_ABORT ON;
-- nvarchar(max) so that an over-long value is refused, not truncated.
DECLARE @login nvarchar(max) = N'$(ATRIUM_COLLECTOR_LOGIN)';
DECLARE @password nvarchar(max) = N'$(ATRIUM_COLLECTOR_PASSWORD)';
DECLARE @sql nvarchar(max), @elevated int;
-- Reports whether @login is sysadmin-equivalent, by impersonating it.
DECLARE @elevated_sql nvarchar(max) = N'EXECUTE AS LOGIN = ' + QUOTENAME(@login, N'''') + N';
SET @e = CASE WHEN IS_SRVROLEMEMBER(N''sysadmin'') = 1 OR HAS_PERMS_BY_NAME(NULL, NULL, N''CONTROL SERVER'') = 1 THEN 1 ELSE 0 END;
REVERT;';

-- 1. Checks, before any change.
IF LEN(@login) = 0 OR LEN(@login) > 128
	THROW 50000, 'ATRIUM_COLLECTOR_LOGIN must be 1 to 128 characters', 1;
IF LEN(@password) = 0 OR LEN(@password) > 128
	THROW 50000, 'ATRIUM_COLLECTOR_PASSWORD must be 1 to 128 characters', 1;
IF @login = N'sa'
	THROW 50000, 'ATRIUM_COLLECTOR_LOGIN must name a dedicated login, not sa', 1;
IF SUSER_ID(@login) IS NOT NULL
BEGIN
	IF EXISTS (SELECT 1 FROM sys.server_role_members AS m
		JOIN sys.server_principals AS p ON p.principal_id = m.member_principal_id
		WHERE p.name = @login)
		THROW 50000, 'the existing collector login is a member of a server role; drop it or use another name', 1;
	IF EXISTS (SELECT 1 FROM sys.server_permissions AS sp
		JOIN sys.server_principals AS p ON p.principal_id = sp.grantee_principal_id
		WHERE p.name = @login
			AND (sp.state <> 'G' OR sp.permission_name NOT IN (N'CONNECT SQL', N'VIEW SERVER STATE', N'VIEW ANY DEFINITION')))
		THROW 50000, 'the existing collector login holds server permissions beyond CONNECT SQL, VIEW SERVER STATE and VIEW ANY DEFINITION; drop it or use another name', 1;
	EXEC sys.sp_executesql @elevated_sql, N'@e int OUTPUT', @e = @elevated OUTPUT;
	IF @elevated = 1
		THROW 50000, 'the existing collector login is sysadmin-equivalent; drop it or use another name', 1;
END

-- 2. Changes, in one transaction.
BEGIN TRANSACTION;
IF SUSER_ID(@login) IS NULL
	SET @sql = N'CREATE LOGIN ' + QUOTENAME(@login) + N' WITH PASSWORD = ' + QUOTENAME(@password, N'''');
ELSE
	SET @sql = N'ALTER LOGIN ' + QUOTENAME(@login) + N' WITH PASSWORD = ' + QUOTENAME(@password, N'''');
EXEC sys.sp_executesql @sql;
SET @sql = N'GRANT VIEW SERVER STATE, VIEW ANY DEFINITION TO ' + QUOTENAME(@login);
EXEC sys.sp_executesql @sql;
EXEC sys.sp_executesql @elevated_sql, N'@e int OUTPUT', @e = @elevated OUTPUT;
IF @elevated = 1
BEGIN
	ROLLBACK TRANSACTION;
	THROW 50000, 'the collector login would be sysadmin-equivalent (check permissions granted to public); nothing was changed', 1;
END
COMMIT TRANSACTION;
GO
SQL
