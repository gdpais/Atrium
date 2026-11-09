#!/bin/sh
# Runs every built-in SQL Server probe and every OutSystems log query, then
# PatientZero's live tests (tools/patientzero, ADR 0010), against a disposable
# SQL Server container (make test-sqlserver). The container has a
# random SA password, no volume and a random loopback port, and is removed on
# exit. It never touches the sqlserver-dev sandbox.
#
# The probes run as a least-privilege login with only the documented grants
# (scripts/sqlserver-login.sh), so a probe that needs more fails here. The log
# queries run as atrium_oslog, a login with no server permissions that the
# tests give SELECT on their fixture tables only. sa is used for setup and by
# PatientZero's live tests, which create and drop their own databases. sa and
# a CONTROL SERVER login, under their own credential references, are used
# only by the test of the sysadmin-equivalence check.
#
# Environment:
#   SQLSERVER_IMAGE  SQL Server image [mcr.microsoft.com/mssql/server:2022-latest]
#   SQLSERVER_MEMORY_LIMIT_MB  SQL Server's memory cap in MB, at least 2048 [2048]
set -eu

SQLSERVER_IMAGE=${SQLSERVER_IMAGE:-mcr.microsoft.com/mssql/server:2022-latest}
SQLSERVER_MEMORY_LIMIT_MB=${SQLSERVER_MEMORY_LIMIT_MB:-2048}
CONTAINER=atrium-sqlserver-test-$$

log() { printf '%s\n' "$*" >&2; }
die() { log "error: $*"; exit 1; }

cleanup() { docker rm -f "$CONTAINER" >/dev/null 2>&1 || true; }
trap cleanup EXIT INT TERM

# SQL Server does not start with less than about 2 GB.
case "$SQLSERVER_MEMORY_LIMIT_MB" in
"" | *[!0-9]*) die "SQLSERVER_MEMORY_LIMIT_MB must be a number of MB, not $SQLSERVER_MEMORY_LIMIT_MB" ;;
esac
[ "$SQLSERVER_MEMORY_LIMIT_MB" -ge 2048 ] || die "SQLSERVER_MEMORY_LIMIT_MB must be at least 2048 (SQL Server needs 2 GB), not $SQLSERVER_MEMORY_LIMIT_MB"

password="Test-$(openssl rand -hex 16)"
collector_login=atrium_collector
collector_password="Test-$(openssl rand -hex 16)"
control_password="Test-$(openssl rand -hex 16)"
oslog_password="Test-$(openssl rand -hex 16)"
log "starting $CONTAINER ($SQLSERVER_IMAGE, memory capped at $SQLSERVER_MEMORY_LIMIT_MB MB)"
# The password goes in through the environment, not the command line. The
# cap keeps SQL Server from sizing itself to 80% of the Docker VM, next to
# the kind clusters (scripts/kind.sh).
MSSQL_SA_PASSWORD=$password MSSQL_MEMORY_LIMIT_MB=$SQLSERVER_MEMORY_LIMIT_MB docker run -d --name "$CONTAINER" \
	--platform linux/amd64 -p 127.0.0.1::1433 \
	-e ACCEPT_EULA=Y -e MSSQL_PID=Developer -e MSSQL_SA_PASSWORD -e MSSQL_MEMORY_LIMIT_MB \
	--health-cmd 'SQLCMDPASSWORD="$MSSQL_SA_PASSWORD" /opt/mssql-tools18/bin/sqlcmd -S localhost -U sa -C -Q "SELECT 1" >/dev/null 2>&1 || exit 1' \
	--health-interval 5s --health-timeout 5s --health-retries 40 --health-start-period 20s \
	"$SQLSERVER_IMAGE" >/dev/null

log "waiting for $CONTAINER to become healthy"
i=0
until [ "$(docker inspect -f '{{.State.Health.Status}}' "$CONTAINER")" = healthy ]; do
	i=$((i + 1))
	[ "$i" -le 60 ] || die "$CONTAINER did not become healthy"
	sleep 5
done

# SQL Server reports the cap as its physical memory when it applies it.
memory_mb=$(SQLCMDPASSWORD=$password docker exec -e SQLCMDPASSWORD "$CONTAINER" \
	/opt/mssql-tools18/bin/sqlcmd -S localhost -U sa -C -b -h -1 -W \
	-Q "SET NOCOUNT ON; SELECT physical_memory_kb / 1024 FROM sys.dm_os_sys_info") ||
	die "could not read SQL Server's memory from $CONTAINER"
memory_mb=$(printf '%s' "$memory_mb" | tr -d '[:space:]')
[ "$memory_mb" -le "$SQLSERVER_MEMORY_LIMIT_MB" ] ||
	die "SQL Server in $CONTAINER sees $memory_mb MB, not the $SQLSERVER_MEMORY_LIMIT_MB MB cap"
log "SQL Server sees $memory_mb MB (cap $SQLSERVER_MEMORY_LIMIT_MB MB)"

log "creating the least-privilege login $collector_login"
MSSQL_SA_PASSWORD=$password ATRIUM_COLLECTOR_LOGIN=$collector_login ATRIUM_COLLECTOR_PASSWORD=$collector_password \
	scripts/sqlserver-login.sh "$CONTAINER"
log "creating atrium_control, a CONTROL SERVER login for the sysadmin check"
# sqlcmd substitutes $(CONTROL_PASSWORD) from the environment; the password
# is never on a command line.
SQLCMDPASSWORD=$password CONTROL_PASSWORD=$control_password docker exec -i \
	-e SQLCMDPASSWORD -e CONTROL_PASSWORD "$CONTAINER" \
	/opt/mssql-tools18/bin/sqlcmd -S localhost -U sa -C -b >/dev/null <<'SQL'
CREATE LOGIN atrium_control WITH PASSWORD = N'$(CONTROL_PASSWORD)';
GRANT CONTROL SERVER TO atrium_control;
GO
SQL
log "creating atrium_oslog, a login with no server permissions for the log queries"
SQLCMDPASSWORD=$password OSLOG_PASSWORD=$oslog_password docker exec -i \
	-e SQLCMDPASSWORD -e OSLOG_PASSWORD "$CONTAINER" \
	/opt/mssql-tools18/bin/sqlcmd -S localhost -U sa -C -b >/dev/null <<'SQL'
CREATE LOGIN atrium_oslog WITH PASSWORD = N'$(OSLOG_PASSWORD)';
GO
SQL

port=$(docker port "$CONTAINER" 1433/tcp | head -n 1 | sed 's/.*://')
[ -n "$port" ] || die "could not read the published port of $CONTAINER"

gocache=${GOCACHE:-$(pwd)/.tmp/gocache}

ATRIUM_TEST_SQLSERVER_ADDR="127.0.0.1:$port" \
	ATRIUM_CREDENTIAL_SQLSERVER_TEST="$collector_login:$collector_password" \
	ATRIUM_CREDENTIAL_SQLSERVER_TEST_SA="sa:$password" \
	ATRIUM_CREDENTIAL_SQLSERVER_TEST_CONTROL="atrium_control:$control_password" \
	ATRIUM_CREDENTIAL_SQLSERVER_TEST_OSLOG="atrium_oslog:$oslog_password" \
	GOCACHE=$gocache \
	go test -tags=sqlserver -count=1 -timeout=5m -v \
	-run 'AgainstSQLServer$' ./services/db-collector/internal/collectors/...

# The URL carries sa's password, so it goes in through the environment, not
# the command line. The password is letters, digits and a hyphen, so it needs
# no URL escaping.
log "running PatientZero's live tests"
PATIENTZERO_TEST_SQLSERVER="sqlserver://sa:$password@127.0.0.1:$port?encrypt=true&TrustServerCertificate=true" \
	GOCACHE=$gocache \
	go -C tools/patientzero test -count=1 -timeout=5m -v \
	-run '^TestLive' ./internal/...
