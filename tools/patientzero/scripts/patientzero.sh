#!/bin/sh
# Runs PatientZero (ADR 0010) on the kind cluster next to the Heartbeat chart,
# against the sandbox SQL Server. The Makefile calls this: make patientzero-up
# and make patientzero-down. Run make sqlserver-dev-up first.
#
# up:
#   1. Adds PATIENTZERO_SQL_CREDENTIAL=patientzero:<random password> to the
#      SQL Server env file if it has none.
#   2. As sa, inside the SQL Server container: creates the patientzero and
#      patientzero_log databases and PatientZero's login, db_owner of those
#      two databases and nothing else, and adds the Heartbeat collector's
#      login to a role in patientzero_log that PatientZero grants SELECT on
#      the log views, nothing more.
#   3. Builds the image, loads it into kind and installs the chart into the
#      patientzero namespace. PatientZero creates its schemas at startup.
#   4. Redeploys the Heartbeat chart with an outsystems_logs collector that
#      reads patientzero_log. A later make kind-deploy drops that collector;
#      run make patientzero-up again to restore it.
# down: uninstalls the chart, redeploys Heartbeat without the collector, and
# drops the two databases and the login.
# load: runs the k6 load (tools/patientzero/load) as a Job next to
# PatientZero, follows its output and exits with k6's result. Interrupting it
# stops following, not the load.
# load-stop: deletes the load Job.
# scenario: runs the scenario SCENARIO (tools/patientzero/scenarios) through
#   PatientZero's admin endpoint, with the k6 load profile the scenario names
#   running next to it, follows it to its end, saves its run journal to
#   .tmp/patientzero-scenarios/<run>.jsonl and exits 0 only if every signal
#   check passed. Without SCENARIO it lists the scenarios. Interrupting it
#   stops the run (its fault is removed) and the load it started. For a
#   scenario with cluster faults it is the run's host agent: it restarts or
#   stops the db-collector, the OTel Collector, Loki or SQL Server when
#   PatientZero asks, and ends an outage it started when interrupted.
# scenarios: runs every scenario, or those SCENARIOS names, one after the
#   other in SCENARIO_ORDER (below), as scenario does, and goes on after a
#   failure. Writes .tmp/patientzero-scenarios/summary.md (and the GitHub
#   step summary in CI) and exits 0 only if every scenario passed. The
#   scenarios workflow runs it on a fresh cluster (.github/workflows/scenarios.yml).
# scale: a production-sized test logging database, patientzero_scale on the
# sandbox SQL Server: two weeks of the surveyed installation's log, written
# straight into the cycle tables (tools/patientzero/cmd/oslog-scale), then
# every collector query measured against it. The Heartbeat collector's login
# can read it like patientzero_log. Writes .tmp/patientzero-scale/
# expected.json and report.md. It does not touch PatientZero on kind.
# scale-measure: measures again, on the database as it is.
# scale-collect: redeploys Heartbeat with a second outsystems_logs collector,
#   patientzero-scale, that reads patientzero_scale from SCALE_LOOKBACK back.
#   Its target and deployment.environment.name are named after the generated
#   log's end (scale-<end>), so a regenerated log gets fresh watermarks and
#   its own rows in Loki.
# scale-check: compares what Loki holds for that log with expected.json,
#   through a port-forward to Loki; writes .tmp/patientzero-scale/check.md.
# scale-down: drops patientzero_scale and removes the patientzero-scale
#   collector.
#
# Every kubectl and helm call names the kind context explicitly.
#
# Environment (defaults in brackets), shared with scripts/kind.sh:
#   KIND_CLUSTER           cluster name [heartbeat]
#   HEARTBEAT_NAMESPACE    Heartbeat's namespace [heartbeat]
#   PATIENTZERO_NAMESPACE  PatientZero's namespace [patientzero]
#   SQLSERVER_CONTAINER    sandbox SQL Server container [heartbeat-sqlserver-dev]
#   SQLSERVER_ENV_FILE     SA password and credentials [.env.sqlserver-dev]
#   PROFILE, EXTRA_VALUES  passed on to scripts/kind.sh deploy
#   LOAD                   load profile: smoke, steady, peak, soak or day [steady]
#   LOAD_DURATION          how long the profile holds its load [the profile's]
#   LOAD_SEED              seed of the virtual users' choices [1]
#   SCALE                  volume relative to the surveyed installation [1]
#   SCALE_SEED             seed of the generated log [1]
#   SCALE_END              last instant generated, RFC 3339 [now]
#   SCALE_LOOKBACK         how far back the patientzero-scale collector starts [48h]
#   SCENARIO               scenario to run; empty to list them []
#   SCENARIO_LOAD          0 to run a scenario without starting its load [1]
#   SCENARIOS              scenarios to run, space-separated; empty for all []
set -eu
cd "$(dirname "$0")/../../.."

CLUSTER=${KIND_CLUSTER:-heartbeat}
CONTEXT=kind-$CLUSTER
NAMESPACE=${PATIENTZERO_NAMESPACE:-patientzero}
HEARTBEAT_NAMESPACE=${HEARTBEAT_NAMESPACE:-heartbeat}
SQLSERVER_CONTAINER=${SQLSERVER_CONTAINER:-heartbeat-sqlserver-dev}
SQLSERVER_ENV_FILE=${SQLSERVER_ENV_FILE:-.env.sqlserver-dev}
STATE_DIR=.tmp/kind/$CLUSTER
CHART=tools/patientzero/chart
RELEASE=patientzero
CREDENTIAL_KEY=PATIENTZERO_SQL_CREDENTIAL
COLLECTOR_KEY=HEARTBEAT_CREDENTIAL_ENV_SQLSERVER_DEV
READER_ROLE=heartbeat_oslog_reader
DATABASE=patientzero
LOG_DATABASE=patientzero_log
SCALE_DATABASE=patientzero_scale
HEARTBEAT_VALUES=$STATE_DIR/patientzero-heartbeat.yaml
# The patientzero-scale collector's name, lookback and start, while it runs.
SCALE_STATE=$STATE_DIR/patientzero-scale
SCALE_DIR=.tmp/patientzero-scale
LOAD_DIR=tools/patientzero/load
LOAD_JOB=patientzero-load
ADMIN_SECRET=patientzero-admin
SCENARIO_DIR=.tmp/patientzero-scenarios
# The order of make patientzero-scenarios: the SLO scenarios first, while the
# burn-rate windows hold no other fault (they check that, scenarios/README.md);
# late-commits last, as its OutSystemsLateLogRows alert keeps firing for the
# 15 minutes it looks back, which restarts checks is quiet. A scenario file
# missing here fails the scenarios package's tests.
SCENARIO_ORDER="bad-deploy slow-sql checkout-errors orderlist-latency restarts rotation log-backlog log-growth loki-outage otel-outage tempo-outage collector-outage sqlserver-outage late-commits"
# Renovate keeps this pin current (renovate.json).
K6_IMAGE=grafana/k6:2.3.0@sha256:2dd4756ec6e14e62778916e1eb850108e4b499304c02df56760fa0bdb0fba0b8

kubectl_() { kubectl --context "$CONTEXT" "$@"; }
log() { printf '==> %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

cluster_exists() { kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; }
sqlserver_healthy() { [ "$(docker inspect -f '{{.State.Health.Status}}' "$SQLSERVER_CONTAINER" 2>/dev/null)" = healthy ]; }
env_value() { grep "^$1=" "$SQLSERVER_ENV_FILE" | head -n 1 | cut -d= -f2-; }

# Adds PatientZero's credential to the env file once, in the plain KEY=value
# form scripts/kind.sh reads. "Dev-" plus hex digits meets SQL Server's
# password complexity.
ensure_credential() {
	[ -f "$SQLSERVER_ENV_FILE" ] || die "$SQLSERVER_ENV_FILE is missing; run make sqlserver-dev-init and make sqlserver-dev-up"
	if [ -z "$(env_value "$CREDENTIAL_KEY")" ]; then
		(umask 077 && printf '%s=patientzero:Dev-%s\n' "$CREDENTIAL_KEY" "$(openssl rand -hex 16)" >>"$SQLSERVER_ENV_FILE")
		log "added PatientZero's login to $SQLSERVER_ENV_FILE"
	fi
	credential=$(env_value "$CREDENTIAL_KEY")
	pz_login=${credential%%:*}
	pz_password=${credential#*:}
	if [ "$pz_login" = "$credential" ] || [ -z "$pz_login" ] || [ -z "$pz_password" ] ||
		[ "$(printf '%s' "$pz_login" | tr 'A-Z' 'a-z')" = sa ]; then
		die "$CREDENTIAL_KEY in $SQLSERVER_ENV_FILE must be <login>:<password> for PatientZero's own login, not sa"
	fi
	collector=$(env_value "$COLLECTOR_KEY")
	collector_login=${collector%%:*}
	[ -n "$collector_login" ] && [ "$collector_login" != "$collector" ] ||
		die "$COLLECTOR_KEY in $SQLSERVER_ENV_FILE must be <login>:<password>; run make sqlserver-dev-init"
	[ "$(printf '%s' "$collector_login" | tr 'A-Z' 'a-z')" != "$(printf '%s' "$pz_login" | tr 'A-Z' 'a-z')" ] ||
		die "PatientZero and the Heartbeat collector must use different logins"
}

# Runs a batch as sa in the container. sqlcmd substitutes $(NAME) from the
# environment as plain text inside N'...' literals, so quotes are doubled.
sql_quote() { printf '%s' "$1" | sed "s/'/''/g"; }
sa_sql() {
	SQLCMDPASSWORD=$(env_value MSSQL_SA_PASSWORD) \
		PZ_LOGIN=$(sql_quote "$pz_login") PZ_PASSWORD=$(sql_quote "${pz_password:-}") \
		PZ_COLLECTOR_LOGIN=$(sql_quote "${collector_login:-}") PZ_READER_ROLE=$READER_ROLE \
		PZ_DATABASE=$DATABASE PZ_LOG_DATABASE=$LOG_DATABASE \
		docker exec -i -e SQLCMDPASSWORD -e PZ_LOGIN -e PZ_PASSWORD -e PZ_COLLECTOR_LOGIN \
		-e PZ_READER_ROLE -e PZ_DATABASE -e PZ_LOG_DATABASE \
		"$SQLSERVER_CONTAINER" /opt/mssql-tools18/bin/sqlcmd -S localhost -U sa -C -b -h -1
}

grant_logins() {
	log "creating $DATABASE, $LOG_DATABASE and the $pz_login login (db_owner of those two only)"
	sa_sql <<'SQL'
SET NOCOUNT ON;
SET XACT_ABORT ON;
DECLARE @login nvarchar(max) = N'$(PZ_LOGIN)';
DECLARE @password nvarchar(max) = N'$(PZ_PASSWORD)';
DECLARE @collector nvarchar(max) = N'$(PZ_COLLECTOR_LOGIN)';
DECLARE @role sysname = N'$(PZ_READER_ROLE)';
DECLARE @sql nvarchar(max), @db sysname;
-- EXEC takes no expressions as parameter values, so the quoted names are
-- computed first.
DECLARE @qlogin nvarchar(300) = QUOTENAME(@login), @qcollector nvarchar(300) = QUOTENAME(@collector),
	@qrole nvarchar(300) = QUOTENAME(@role);

-- Checks, before any change: PatientZero's login holds nothing on the
-- server beyond CONNECT SQL, so its rights are the two databases' only.
IF LEN(@login) = 0 OR LEN(@login) > 128 OR LEN(@password) = 0 OR LEN(@password) > 128
	THROW 50000, 'the PatientZero login and password must be 1 to 128 characters', 1;
IF SUSER_ID(@collector) IS NULL
	THROW 50000, 'the Heartbeat collector login does not exist; run make sqlserver-dev-up first', 1;
IF EXISTS (SELECT 1 FROM sys.server_role_members AS m
	JOIN sys.server_principals AS p ON p.principal_id = m.member_principal_id WHERE p.name = @login)
	THROW 50000, 'the existing PatientZero login is a member of a server role; drop it or use another name', 1;
IF EXISTS (SELECT 1 FROM sys.server_permissions AS sp
	JOIN sys.server_principals AS p ON p.principal_id = sp.grantee_principal_id
	WHERE p.name = @login AND (sp.state <> 'G' OR sp.permission_name <> N'CONNECT SQL'))
	THROW 50000, 'the existing PatientZero login holds server permissions beyond CONNECT SQL; drop it or use another name', 1;

IF SUSER_ID(@login) IS NULL
	SET @sql = N'CREATE LOGIN ' + QUOTENAME(@login) + N' WITH PASSWORD = ' + QUOTENAME(@password, N'''');
ELSE
	SET @sql = N'ALTER LOGIN ' + QUOTENAME(@login) + N' WITH PASSWORD = ' + QUOTENAME(@password, N'''');
EXEC sys.sp_executesql @sql;

DECLARE dbs CURSOR LOCAL FAST_FORWARD FOR SELECT N'$(PZ_DATABASE)' UNION ALL SELECT N'$(PZ_LOG_DATABASE)';
OPEN dbs;
FETCH NEXT FROM dbs INTO @db;
WHILE @@FETCH_STATUS = 0
BEGIN
	IF DB_ID(@db) IS NULL
	BEGIN
		SET @sql = N'CREATE DATABASE ' + QUOTENAME(@db);
		EXEC sys.sp_executesql @sql;
	END
	SET @sql = N'USE ' + QUOTENAME(@db) + N';
IF DATABASE_PRINCIPAL_ID(@login) IS NULL
	EXEC (N''CREATE USER '' + @qlogin + N'' FOR LOGIN '' + @qlogin);
EXEC (N''ALTER ROLE db_owner ADD MEMBER '' + @qlogin);';
	EXEC sys.sp_executesql @sql, N'@login sysname, @qlogin nvarchar(300)', @login = @login, @qlogin = @qlogin;
	FETCH NEXT FROM dbs INTO @db;
END
CLOSE dbs;

-- The collector reads the log views exactly as it reads a real logging
-- database: as its own user, through the role PatientZero grants SELECT on
-- the views to.
SET @sql = N'USE ' + QUOTENAME(N'$(PZ_LOG_DATABASE)') + N';
IF DATABASE_PRINCIPAL_ID(@collector) IS NULL
	EXEC (N''CREATE USER '' + @qcollector + N'' FOR LOGIN '' + @qcollector);
IF DATABASE_PRINCIPAL_ID(@role) IS NULL
	EXEC (N''CREATE ROLE '' + @qrole);
IF IS_ROLEMEMBER(@role, @collector) = 0
	EXEC (N''ALTER ROLE '' + @qrole + N'' ADD MEMBER '' + @qcollector);';
EXEC sys.sp_executesql @sql, N'@collector sysname, @role sysname, @qcollector nvarchar(300), @qrole nvarchar(300)',
	@collector = @collector, @role = @role, @qcollector = @qcollector, @qrole = @qrole;
GO
SQL
}

drop_logins() {
	log "dropping $DATABASE, $LOG_DATABASE and the $pz_login login"
	sa_sql <<'SQL'
SET NOCOUNT ON;
DECLARE @login sysname = N'$(PZ_LOGIN)', @sql nvarchar(max), @db sysname;
DECLARE dbs CURSOR LOCAL FAST_FORWARD FOR SELECT N'$(PZ_DATABASE)' UNION ALL SELECT N'$(PZ_LOG_DATABASE)';
OPEN dbs;
FETCH NEXT FROM dbs INTO @db;
WHILE @@FETCH_STATUS = 0
BEGIN
	IF DB_ID(@db) IS NOT NULL
	BEGIN
		SET @sql = N'ALTER DATABASE ' + QUOTENAME(@db) + N' SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE ' + QUOTENAME(@db);
		EXEC sys.sp_executesql @sql;
	END
	FETCH NEXT FROM dbs INTO @db;
END
CLOSE dbs;
IF SUSER_ID(@login) IS NOT NULL
BEGIN
	SET @sql = N'DROP LOGIN ' + QUOTENAME(@login);
	EXEC sys.sp_executesql @sql;
END
GO
SQL
}

node_arch() {
	case "$(docker exec "$CLUSTER-control-plane" uname -m)" in
	x86_64) echo amd64 ;;
	aarch64 | arm64) echo arm64 ;;
	*) die "unsupported node architecture" ;;
	esac
}

# Tags the image by its content, as scripts/kind.sh does, so a stale image
# cannot hide behind a reused tag.
build_image() {
	arch=$(node_arch)
	log "building heartbeat/patientzero for linux/$arch"
	docker build --quiet --platform "linux/$arch" --provenance=false \
		-t heartbeat/patientzero:build -f tools/patientzero/Dockerfile . >/dev/null
	id=$(docker image inspect -f '{{.Id}}' heartbeat/patientzero:build)
	tag=dev-$(printf '%s' "${id#sha256:}" | cut -c1-12)
	docker tag heartbeat/patientzero:build "heartbeat/patientzero:$tag"
	log "loading heartbeat/patientzero:$tag into $CLUSTER"
	kind load docker-image --name "$CLUSTER" "heartbeat/patientzero:$tag" >/dev/null
}

install_chart() {
	kubectl_ create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl_ apply -f - >/dev/null
	# The credential reaches kubectl in a 0600 file, never on its command line.
	secret_env=$(mktemp "${TMPDIR:-/tmp}/patientzero-credential.XXXXXX")
	trap 'rm -f "$secret_env"' EXIT
	trap 'exit 130' INT TERM
	printf '%s=%s\n' "$CREDENTIAL_KEY" "$credential" >"$secret_env"
	kubectl_ -n "$NAMESPACE" create secret generic patientzero-sql --from-env-file="$secret_env" \
		--dry-run=client -o yaml | kubectl_ apply -f - >/dev/null
	# The admin token: random, created once, kept across reinstalls. Only the
	# Secret holds it; make patientzero-scenario reads it from there.
	if ! kubectl_ -n "$NAMESPACE" get secret "$ADMIN_SECRET" >/dev/null 2>&1; then
		printf 'admin-token=%s\n' "$(openssl rand -hex 32)" >"$secret_env"
		kubectl_ -n "$NAMESPACE" create secret generic "$ADMIN_SECRET" --from-env-file="$secret_env" >/dev/null
	fi
	rm -f "$secret_env"
	log "installing release $RELEASE to $CONTEXT/$NAMESPACE"
	helm upgrade --install "$RELEASE" "$CHART" \
		--kube-context "$CONTEXT" --namespace "$NAMESPACE" \
		--set "image.tag=$tag" \
		--set "sql.host=$SQLSERVER_CONTAINER" \
		--set sql.trustServerCertificate=true \
		--set "sql.database=$DATABASE" --set "sql.logDatabase=$LOG_DATABASE" \
		--set "logReaderRole=$READER_ROLE" \
		--set "signals.prometheusURL=http://prometheus.$HEARTBEAT_NAMESPACE.svc:9090" \
		--set "signals.lokiURL=http://loki.$HEARTBEAT_NAMESPACE.svc:3100" \
		--set "signals.alertmanagerURL=http://alertmanager.$HEARTBEAT_NAMESPACE.svc:9093" \
		--wait --timeout 5m
}

# kind.yaml's backendNetworkPolicies, which network_peers repeats: the
# operator range on the OTel Collector, Prometheus and Alertmanager.
KIND_VALUES=infra/helm/values/kind.yaml
KIND_PEERS='  otelCollector:
    extraPeers:
      - ipBlock:
          cidr: 0.0.0.0/0
          except:
            - 10.244.0.0/16
  prometheus:
    extraPeers:
      - ipBlock:
          cidr: 0.0.0.0/0
          except:
            - 10.244.0.0/16
  alertmanager:
    extraPeers:
      - ipBlock:
          cidr: 0.0.0.0/0
          except:
            - 10.244.0.0/16'

# PatientZero's scenario runner reads Prometheus, Loki and Alertmanager, whose
# NetworkPolicies admit only Heartbeat's own components and extraPeers. This
# admits the PatientZero pod to the three. A values file replaces a list, so
# kind.yaml's peers are repeated, and a change to them stops here.
network_peers() {
	current=$(awk '/^backendNetworkPolicies:$/ { on = 1; next }
		on && /^[^ #]/ { exit }
		on && NF && $0 !~ /^[[:space:]]*#/' "$KIND_VALUES")
	[ "$current" = "$KIND_PEERS" ] ||
		die "$KIND_VALUES no longer has the backendNetworkPolicies peers network_peers repeats; update KIND_PEERS in $0"
	cat <<EOF
# PatientZero's scenario signals (tools/patientzero/scripts/patientzero.sh).
backendNetworkPolicies:
  prometheus:
    extraPeers:
      - ipBlock:
          cidr: 0.0.0.0/0
          except:
            - 10.244.0.0/16
      - &patientzero
        namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: $NAMESPACE
        podSelector:
          matchLabels:
            app.kubernetes.io/name: patientzero
  alertmanager:
    extraPeers:
      - ipBlock:
          cidr: 0.0.0.0/0
          except:
            - 10.244.0.0/16
      - *patientzero
  loki:
    extraPeers:
      - *patientzero
EOF
}

# The Heartbeat values that add PatientZero's collectors: the one reading
# patientzero_log while PatientZero is installed, and the one reading
# patientzero_scale after make patientzero-scale-collect. With neither, there
# is no values file. A values file replaces a list instead of merging it, so
# this copies the sandbox's values file, whose last entry is
# integrations.collectors, and appends to that list. The sandbox target's host
# is SQLSERVER_CONTAINER, so a throwaway cluster reads its own container.
# While PatientZero is installed, network_peers follows.
heartbeat_values() {
	with_patientzero=
	if helm status "$RELEASE" --kube-context "$CONTEXT" --namespace "$NAMESPACE" >/dev/null 2>&1; then
		with_patientzero=1
	fi
	scale_label=
	[ -f "$SCALE_STATE" ] && read -r scale_label scale_lookback _ <"$SCALE_STATE"
	if [ -z "$with_patientzero$scale_label" ]; then
		rm -f "$HEARTBEAT_VALUES"
		return
	fi
	src=infra/helm/values/sqlserver-dev.yaml
	awk '/^  collectors:$/ { found = 1; next }
		found && NF && $0 !~ /^    / && $0 !~ /^[[:space:]]*#/ { bad = 1 }
		END { exit !(found && !bad) }' "$src" ||
		die "$src no longer ends with integrations.collectors; update heartbeat_values in $0"
	mkdir -p "$STATE_DIR"
	{
		# The sandbox target is this run's container, as kind-e2e points it.
		sed "s/^\( *host: \)heartbeat-sqlserver-dev\$/\1$SQLSERVER_CONTAINER/" "$src"
		[ -z "$with_patientzero" ] || log_collector patientzero local-dev "" patientzero-log "$LOG_DATABASE"
		[ -z "$scale_label" ] ||
			log_collector patientzero-scale "$scale_label" "$scale_lookback" "$scale_label" "$SCALE_DATABASE"
		[ -z "$with_patientzero" ] || network_peers
	} >"$HEARTBEAT_VALUES"
}

# log_collector <id> <environment> <initial lookback or empty> <target> <database>
log_collector() {
	cat <<EOF
    # $5 (tools/patientzero/scripts/patientzero.sh).
    - id: $1
      kind: outsystems_logs
      enabled: true
      config:
        environment: $2
        scrape_interval: 15s
        lag: 15s${3:+
        initial_lookback: $3}
        probes:
          # Instant is the start of the screen request or timer.
          - name: screen
            lag: 1m
          - name: cyclic_job
            lag: 1m
          - name: mobile_request
          - name: integration
          - name: general
          - name: error
          - name: extension
          - name: request_event
        targets:
          - name: $4
            host: $SQLSERVER_CONTAINER
            port: 1433
            database_name: $5
            credential_ref: env/sqlserver-dev
EOF
}

# Redeploys Heartbeat with the values heartbeat_values wrote, if any.
redeploy() {
	if [ -f "$HEARTBEAT_VALUES" ]; then
		EXTRA_VALUES="$HEARTBEAT_VALUES ${EXTRA_VALUES:-}" scripts/kind.sh deploy >/dev/null
	else
		scripts/kind.sh deploy >/dev/null
	fi
}

cmd_up() {
	cluster_exists || die "kind cluster $CLUSTER does not exist; run make sqlserver-dev-up"
	sqlserver_healthy || die "$SQLSERVER_CONTAINER is not running and healthy; run make sqlserver-dev-up"
	[ -f "$STATE_DIR/sqlserver-dev" ] || die "Heartbeat on $CLUSTER does not target $SQLSERVER_CONTAINER; run make sqlserver-dev-up"
	ensure_credential
	grant_logins
	build_image
	install_chart
	heartbeat_values
	log "redeploying Heartbeat with the patientzero outsystems_logs collector"
	redeploy
	kubectl_ -n "$NAMESPACE" get pods,svc
	log "PatientZero is up; try: kubectl --context $CONTEXT -n $NAMESPACE port-forward svc/patientzero 8080"
}

cmd_down() {
	if cluster_exists; then
		if helm status "$RELEASE" --kube-context "$CONTEXT" --namespace "$NAMESPACE" >/dev/null 2>&1; then
			helm uninstall "$RELEASE" --kube-context "$CONTEXT" --namespace "$NAMESPACE" --wait >/dev/null
		fi
		kubectl_ delete namespace "$NAMESPACE" --ignore-not-found >/dev/null
		log "removed $RELEASE and the $NAMESPACE namespace"
		if [ -f "$HEARTBEAT_VALUES" ]; then
			heartbeat_values
			log "redeploying Heartbeat without the patientzero collector"
			redeploy
		fi
	else
		rm -f "$HEARTBEAT_VALUES"
	fi
	if sqlserver_healthy && [ -f "$SQLSERVER_ENV_FILE" ]; then
		credential=$(env_value "$CREDENTIAL_KEY")
		pz_login=${credential%%:*}
		if [ -n "$pz_login" ] && [ "$(printf '%s' "$pz_login" | tr 'A-Z' 'a-z')" != sa ]; then
			drop_logins
		fi
	fi
}

# The load runs inside the cluster: a port-forward would be the bottleneck,
# and the requests reach PatientZero the way kind-e2e's will.
cmd_load() {
	profile=${LOAD:-steady}
	case $profile in
	smoke | steady | peak | soak | day) ;;
	*) die "LOAD must be smoke, steady, peak, soak or day, not $profile" ;;
	esac
	cluster_exists || die "kind cluster $CLUSTER does not exist; run make sqlserver-dev-up and make patientzero-up"
	helm status "$RELEASE" --kube-context "$CONTEXT" --namespace "$NAMESPACE" >/dev/null 2>&1 ||
		die "PatientZero is not installed on $CLUSTER; run make patientzero-up"
	start_load "$profile"
	follow_load
}

# start_load <profile>: replaces the load Job with a new run of profile and
# waits for its pod to leave Pending (the image pull). Sets run_id.
start_load() {
	profile=$1
	run_id=$profile-$(date -u +%Y%m%dT%H%M%SZ)
	kubectl_ -n "$NAMESPACE" create configmap "$LOAD_JOB" --from-file="$LOAD_DIR" \
		--dry-run=client -o yaml | kubectl_ apply -f - >/dev/null
	kubectl_ -n "$NAMESPACE" delete job "$LOAD_JOB" --ignore-not-found --wait >/dev/null
	log "starting the $profile load, run $run_id"
	kubectl_ apply -f - >/dev/null <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: $LOAD_JOB
  namespace: $NAMESPACE
  labels:
    app.kubernetes.io/name: $LOAD_JOB
    app.kubernetes.io/part-of: patientzero
spec:
  backoffLimit: 0
  # Kept a day for its logs; the next run replaces it.
  ttlSecondsAfterFinished: 86400
  template:
    metadata:
      # PatientZero's NetworkPolicy admits port 8080 from pods with this name
      # only (tools/patientzero/chart/templates/networkpolicy.yaml).
      labels:
        app.kubernetes.io/name: $LOAD_JOB
        app.kubernetes.io/part-of: patientzero
    spec:
      restartPolicy: Never
      automountServiceAccountToken: false
      enableServiceLinks: false
      securityContext:
        runAsNonRoot: true
        # The k6 image's own user.
        runAsUser: 12345
        runAsGroup: 12345
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: k6
          image: $K6_IMAGE
          args: [run, --no-color, --no-usage-report, /load/patientzero.js]
          env:
            - name: BASE_URL
              value: http://patientzero.$NAMESPACE.svc:8080
            - name: PROFILE
              value: "$profile"
            - name: DURATION
              value: "${LOAD_DURATION:-}"
            - name: RUN_ID
              value: "$run_id"
            - name: SEED
              value: "${LOAD_SEED:-1}"
          volumeMounts:
            - name: load
              mountPath: /load
              readOnly: true
            - name: tmp
              mountPath: /tmp
          resources:
            requests:
              cpu: 100m
              memory: 128Mi
            limits:
              memory: 512Mi
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: [ALL]
      volumes:
        - name: load
          configMap:
            name: $LOAD_JOB
        - name: tmp
          emptyDir: {}
EOF
	# Waits for the pod to leave Pending (the image pull) before following.
	i=0
	until kubectl_ -n "$NAMESPACE" get pods -l "job-name=$LOAD_JOB" -o jsonpath='{.items[*].status.phase}' 2>/dev/null |
		grep -qE 'Running|Succeeded|Failed'; do
		i=$((i + 1))
		[ "$i" -le 150 ] || die "the load pod did not start; kubectl --context $CONTEXT -n $NAMESPACE describe job $LOAD_JOB"
		sleep 2
	done
}

# follow_load: follows the load Job's output and exits with k6's result.
follow_load() {
	kubectl_ -n "$NAMESPACE" logs -f "job/$LOAD_JOB" || true
	i=0
	while :; do
		status=$(kubectl_ -n "$NAMESPACE" get job "$LOAD_JOB" -o jsonpath='{.status.succeeded}/{.status.failed}')
		case $status in
		1/*) log "load run $run_id finished and k6's thresholds held"; return 0 ;;
		*/1) die "load run $run_id failed: a k6 threshold was crossed or k6 could not run; see its output above" ;;
		esac
		i=$((i + 1))
		[ "$i" -le 30 ] || die "load run $run_id has not finished; make patientzero-load-stop stops it"
		sleep 2
	done
}

# The admin endpoint through a port-forward, with the token in a 0600 header
# file, never on a command line. kubectl runs directly, so $! is kubectl and
# kill stops it.
#
# open_forward replaces the port-forward. A port-forward does not last: kubectl
# exits when a stream to the pod fails, and on the terminal's Ctrl-C, which
# reaches it too. Every admin call that fails opens a new one and retries.
open_forward() {
	[ -z "$forward" ] || kill "$forward" 2>/dev/null || true
	: >"$work/port-forward.log"
	kubectl --context "$CONTEXT" -n "$NAMESPACE" port-forward svc/patientzero :8080 >"$work/port-forward.log" 2>&1 &
	forward=$!
	i=0
	until grep -q '^Forwarding from 127.0.0.1:' "$work/port-forward.log"; do
		i=$((i + 1))
		[ "$i" -le 30 ] || return 1
		sleep 1
	done
	admin_port=$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9]*\) .*/\1/p' "$work/port-forward.log" | head -n 1)
}

scenario_forward() {
	work=$(mktemp -d "${TMPDIR:-/tmp}/patientzero-scenario.XXXXXX")
	open_forward || die "the port-forward to PatientZero did not start: $(cat "$work/port-forward.log")"
	(
		umask 077
		kubectl_ -n "$NAMESPACE" get secret "$ADMIN_SECRET" -o json |
			jq -r '"Authorization: Bearer " + (.data["admin-token"] | @base64d)' >"$work/admin-header"
	) || die "cannot read the $ADMIN_SECRET Secret; run make patientzero-up"
}
# admin <path> <curl arguments...>: an admin request through the port-forward.
admin() {
	path=$1
	shift
	curl -sS --max-time 40 -H "@$work/admin-header" "$@" "http://127.0.0.1:$admin_port$path"
}

# admin_retry <variable> <path> <curl arguments...>: admin, up to three times,
# with a new port-forward after each failure; the response goes into
# <variable>. Runs in this shell, so the new port-forward is kept.
admin_retry() {
	var=$1
	shift
	attempt=0
	while :; do
		if response_=$(admin "$@"); then
			eval "$var=\$response_"
			return 0
		fi
		attempt=$((attempt + 1))
		[ "$attempt" -lt 3 ] || return 1
		log "the port-forward to PatientZero failed; opening a new one"
		open_forward || true
	done
}

# The host agent: PatientZero asks for a run's cluster faults in its status
# (fault_request), and this applies them with kubectl and docker and
# answers (scenarios/README.md, "Cluster faults"). The outages it started
# and has not ended are listed in $work/outages, so an interrupted run ends
# them.

pod_uid() { kubectl_ -n "$HEARTBEAT_NAMESPACE" get pod "$1" -o jsonpath='{.metadata.uid}' 2>/dev/null || true; }
pod_ready() {
	[ "$(kubectl_ -n "$HEARTBEAT_NAMESPACE" get pod "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" = True ]
}

# The agent's waits give up after 4 minutes, inside the 5 PatientZero waits
# for an answer, so the run records why.

# wait_replaced <pod> <uid>: waits until a pod of that name, another than
# uid, is Ready.
wait_replaced() {
	i=0
	until uid_=$(pod_uid "$1") && [ -n "$uid_" ] && [ "$uid_" != "$2" ] && pod_ready "$1"; do
		i=$((i + 1))
		[ "$i" -le 120 ] || { echo "$1 was not replaced and Ready within 4 minutes"; return 1; }
		sleep 2
	done
}

wait_sqlserver() {
	i=0
	until sqlserver_healthy; do
		i=$((i + 1))
		[ "$i" -le 48 ] || { echo "$SQLSERVER_CONTAINER did not become healthy within 4 minutes"; return 1; }
		sleep 5
	done
}

# end_outage <workload>: starts the workload again and waits until it is
# ready.
end_outage() {
	if [ "$1" = sqlserver ]; then
		docker start "$SQLSERVER_CONTAINER" >/dev/null && wait_sqlserver || return 1
	else
		kubectl_ -n "$HEARTBEAT_NAMESPACE" scale "statefulset/$1" --replicas=1 >/dev/null &&
			kubectl_ -n "$HEARTBEAT_NAMESPACE" rollout status "statefulset/$1" --timeout=4m >/dev/null || return 1
	fi
	grep -vx "$1" "$work/outages" >"$work/outages.new" || true
	mv "$work/outages.new" "$work/outages"
}

# cluster_fault <action> <type> <workload>: applies (start) or removes
# (stop) a cluster fault; on failure it prints why.
cluster_fault() {
	case $3 in
	db-collector | otel-collector | loki | tempo) pod=$3-0 ;;
	sqlserver) pod= ;;
	*) echo "unknown workload $3"; return 1 ;;
	esac
	case $2:$1 in
	restart:start)
		if [ -n "$pod" ]; then
			uid=$(pod_uid "$pod")
			kubectl_ -n "$HEARTBEAT_NAMESPACE" delete pod "$pod" --wait=false >/dev/null && wait_replaced "$pod" "$uid"
		else
			docker restart "$SQLSERVER_CONTAINER" >/dev/null && wait_sqlserver
		fi
		;;
	restart:stop) ;;
	outage:start)
		printf '%s\n' "$3" >>"$work/outages"
		if [ -n "$pod" ]; then
			kubectl_ -n "$HEARTBEAT_NAMESPACE" scale "statefulset/$3" --replicas=0 >/dev/null &&
				kubectl_ -n "$HEARTBEAT_NAMESPACE" wait --for=delete "pod/$pod" --timeout=3m >/dev/null
		else
			docker stop "$SQLSERVER_CONTAINER" >/dev/null
		fi
		;;
	outage:stop) end_outage "$3" ;;
	*) echo "unknown cluster fault $2 $1"; return 1 ;;
	esac
}

# agent_answer <status>: applies the fault request in a run's status, once,
# and answers it.
agent_answer() {
	id=$(printf '%s' "$1" | jq -r '.fault_request.id // empty')
	[ -n "$id" ] && [ "$id" != "$answered" ] || return 0
	action=$(printf '%s' "$1" | jq -r .fault_request.action)
	type=$(printf '%s' "$1" | jq -r .fault_request.type)
	workload=$(printf '%s' "$1" | jq -r '.fault_request.params.workload // empty')
	log "cluster fault: $type $workload, $action"
	if out=$(cluster_fault "$action" "$type" "$workload" 2>&1); then
		msg=
	else
		msg=$(printf '%s' "${out:-failed}" | tail -c 400)
		log "cluster fault $type $workload failed to $action: $msg"
	fi
	answered=$id
	admin_retry answer_ "/admin/run/faults/$id" -X POST -H 'Content-Type: application/json' \
		--data "$(jq -cn --arg e "$msg" '{error: $e}')" --fail -o /dev/null ||
		log "could not answer the $type request; the run waits for it, then fails"
}

scenario_cleanup() {
	status=$?
	if [ -n "$run" ]; then
		log "stopping run $run"
		# Detached, PatientZero does not wait for an agent that has stopped
		# answering; the outages are ended below.
		admin_retry stopped_ "/admin/run/stop${agent:+?detach=1}" -X POST --fail -o /dev/null ||
			log "could not stop run $run; it ends on its own, or: POST /admin/run/stop"
	fi
	if [ -s "$work/outages" ]; then
		for workload in $(cat "$work/outages"); do
			log "ending the $workload outage"
			end_outage "$workload" || log "could not start $workload again; start it by hand"
		done
	fi
	if [ -n "$started_load" ]; then
		kubectl_ -n "$NAMESPACE" delete job "$LOAD_JOB" --ignore-not-found --wait=false >/dev/null 2>&1 || true
	fi
	[ -z "$forward" ] || kill "$forward" 2>/dev/null || true
	rm -rf "$work"
	exit "$status"
}

cmd_scenario() {
	cluster_exists || die "kind cluster $CLUSTER does not exist; run make sqlserver-dev-up and make patientzero-up"
	helm status "$RELEASE" --kube-context "$CONTEXT" --namespace "$NAMESPACE" >/dev/null 2>&1 ||
		die "PatientZero is not installed on $CLUSTER; run make patientzero-up"
	command -v jq >/dev/null || die "jq is required"
	work= forward= run= started_load= agent= answered=
	trap scenario_cleanup EXIT
	trap 'exit 130' INT TERM
	scenario_forward
	admin_retry listing /admin/scenarios --fail || die "GET /admin/scenarios failed"
	if [ -z "${SCENARIO:-}" ]; then
		printf '%s' "$listing" | jq -r '.scenarios[] |
			"\(.name)  (\(.faults | join(", ")); load \(.load); at most \(.max_duration_seconds | ceil)s)\(if .runnable then "" else "  NOT RUNNABLE HERE: " + .reason end)\n  \(.description)\n"'
		log "run one with: make patientzero-scenario NAME=<name>"
		return 0
	fi
	info=$(printf '%s' "$listing" | jq -c --arg n "$SCENARIO" '.scenarios[] | select(.name == $n)')
	[ -n "$info" ] || die "no scenario $SCENARIO; make patientzero-scenario lists them"
	[ "$(printf '%s' "$info" | jq -r .runnable)" = true ] ||
		die "$SCENARIO cannot run in PatientZero: $(printf '%s' "$info" | jq -r .reason)"
	profile=$(printf '%s' "$info" | jq -r .load)
	max=$(printf '%s' "$info" | jq -r '.max_duration_seconds | ceil')
	# A scenario with cluster faults runs with this command as its host
	# agent; its faults wait up to 5 minutes each for a workload to be ready.
	poll=5
	if [ "$(printf '%s' "$info" | jq -r .agent)" = true ]; then
		agent=1 poll=2
		command -v docker >/dev/null || die "docker is required for $SCENARIO's cluster faults"
		: >"$work/outages"
		max=$((max + 600))
	fi
	if [ "$profile" != none ] && [ "${SCENARIO_LOAD:-1}" != 0 ]; then
		# The load outlasts the scenario; it is stopped when the scenario ends.
		LOAD_DURATION=$((max + 120))s
		log "starting the $profile load for the scenario"
		start_load "$profile"
		started_load=1
	fi
	response=$(admin "/admin/scenarios/$SCENARIO/start${agent:+?agent=host}" -X POST -w '\n%{http_code}') || die "starting $SCENARIO failed"
	code=$(printf '%s\n' "$response" | tail -n 1)
	body=$(printf '%s\n' "$response" | sed '$d')
	[ "$code" = 202 ] || die "starting $SCENARIO failed (HTTP $code): $body"
	run=$(printf '%s' "$body" | jq -r .run)
	log "run $run of $SCENARIO started; it takes at most ${max}s"
	deadline=$(($(date +%s) + max + 120))
	seen=
	while :; do
		admin_retry status /admin/run --fail || die "GET /admin/run failed"
		[ "$(printf '%s' "$status" | jq -r .run)" = "$run" ] || die "PatientZero reports another run; was it restarted?"
		state=$(printf '%s' "$status" | jq -r .state)
		# Each step once, then each check as it settles.
		for line in $(printf '%s' "$status" | jq -r '(if .step then "step:" + .step else empty end),
			(.checks[] | select(.result != "pending") | "check:" + .signal + ":" + .result)'); do
			case " $seen " in *" $line "*) continue ;; esac
			seen="$seen $line"
			case $line in
			step:*) log "step ${line#step:}" ;;
			check:*) log "check $(printf '%s' "${line#check:}" | sed 's/:/: /')" ;;
			esac
		done
		[ "$state" = running ] || break
		[ -z "$agent" ] || agent_answer "$status"
		[ "$(date +%s)" -lt "$deadline" ] || die "run $run has not ended after ${max}s; stopping it"
		sleep "$poll"
	done
	ended=$run
	run=
	mkdir -p "$SCENARIO_DIR"
	admin_retry saved_ "/admin/runs/$ended/journal" --fail -o "$SCENARIO_DIR/$ended.jsonl" ||
		die "fetching the journal of $ended failed"
	jq -r 'select(.event == "check") | .data |
		"  \(.result | ascii_upcase)  \(.signal): \(.kind)\(if (.condition // .state) then " " + (.condition // .state) else "" end) within \(.within) of the \(.from) of \(.step); \(.attempts) evaluations, last \(if .delivery then [.delivery[] | "\(.log_type) \(.loki)/\(.expected)"] | join(", ") else .values end)\(if .error then "; " + .error else "" end)"' \
		"$SCENARIO_DIR/$ended.jsonl"
	jq -r 'select(.event == "run_end") | .data |
		"  \(.requests) requests (\(.failed) failed, \(.injected) injected) from \(.users) users in \(.sessions) sessions over \(.seconds | floor)s"' \
		"$SCENARIO_DIR/$ended.jsonl"
	log "journal: $SCENARIO_DIR/$ended.jsonl"
	[ "$state" = passed ] || die "run $ended ended $state$(printf '%s' "$status" | jq -r 'if .error then ": " + .error else "" end')"
	log "run $ended passed"
}

cmd_scenarios() {
	# Names are words: no glob expands them.
	set -f
	names=${SCENARIOS:-$SCENARIO_ORDER}
	for name in $names; do
		case " $SCENARIO_ORDER " in
		*" $name "*) ;;
		*) die "no scenario $name; SCENARIOS takes names from: $SCENARIO_ORDER" ;;
		esac
	done
	mkdir -p "$SCENARIO_DIR"
	summary=$SCENARIO_DIR/summary.md
	printf '| Scenario | Result | Took |\n| --- | --- | --- |\n' >"$summary"
	failed=
	for name in $names; do
		log "scenario $name"
		started=$(date +%s)
		if SCENARIO=$name "$0" scenario; then
			result=passed
		else
			result=failed
			failed="$failed $name"
		fi
		printf '| %s | %s | %s s |\n' "$name" "$result" "$(($(date +%s) - started))" >>"$summary"
	done
	cat "$summary"
	if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
		{ echo "## PatientZero scenarios"; echo; cat "$summary"; } >>"$GITHUB_STEP_SUMMARY"
	fi
	[ -z "$failed" ] || die "scenarios failed:$failed"
}

cmd_load_stop() {
	cluster_exists || return 0
	kubectl_ -n "$NAMESPACE" delete job "$LOAD_JOB" --ignore-not-found --wait >/dev/null
	log "stopped the load"
}

# Runs tools/patientzero/cmd/oslog-scale as sa against the sandbox SQL
# Server. The URL carries sa's password, so it goes in through the
# environment; the sandbox password needs no URL escaping.
oslog_scale() {
	sqlserver_healthy || die "$SQLSERVER_CONTAINER is not running; run make sqlserver-dev-up"
	[ -f "$SQLSERVER_ENV_FILE" ] || die "$SQLSERVER_ENV_FILE is missing; run make sqlserver-dev-init and make sqlserver-dev-up"
	port=$(docker port "$SQLSERVER_CONTAINER" 1433/tcp | head -n 1 | sed 's/.*://')
	[ -n "$port" ] || die "could not read the published port of $SQLSERVER_CONTAINER"
	PATIENTZERO_SCALE_SQLSERVER="sqlserver://sa:$(env_value MSSQL_SA_PASSWORD)@127.0.0.1:$port?encrypt=true&TrustServerCertificate=true" \
		GOCACHE=${GOCACHE:-$(pwd)/.tmp/gocache} \
		go -C tools/patientzero run ./cmd/oslog-scale -database "$SCALE_DATABASE" "$@"
}

cmd_scale() {
	collector=$(env_value "$COLLECTOR_KEY")
	log "generating $SCALE_DATABASE at ${SCALE:-1} times the surveyed installation"
	oslog_scale -scale "${SCALE:-1}" -seed "${SCALE_SEED:-1}" ${SCALE_END:+-end "$SCALE_END"} \
		-reader-role "$READER_ROLE" -reader-login "${collector%%:*}" generate
	cmd_scale_measure
	# A running patientzero-scale collector moves to the new log's name.
	if [ -f "$SCALE_STATE" ] && cluster_exists; then
		cmd_scale_collect
	fi
}

cmd_scale_measure() {
	log "measuring the collector's queries on $SCALE_DATABASE"
	oslog_scale measure
}

cmd_scale_collect() {
	cluster_exists || die "kind cluster $CLUSTER does not exist; run make sqlserver-dev-up"
	sqlserver_healthy || die "$SQLSERVER_CONTAINER is not running and healthy; run make sqlserver-dev-up"
	[ -f "$STATE_DIR/sqlserver-dev" ] || die "Heartbeat on $CLUSTER does not target $SQLSERVER_CONTAINER; run make sqlserver-dev-up"
	end=$(sed -n 's/^  "end": "\([^"]*\)",*$/\1/p' "$SCALE_DIR/expected.json" 2>/dev/null)
	[ -n "$end" ] || die "$SCALE_DIR/expected.json is missing; run make patientzero-scale"
	label=scale-$(printf '%s' "$end" | tr -d ':.-' | tr 'A-Z' 'a-z')
	stored=
	[ -f "$SCALE_STATE" ] && read -r _ stored _ <"$SCALE_STATE"
	lookback=${SCALE_LOOKBACK:-${stored:-48h}}
	mkdir -p "$STATE_DIR"
	printf '%s %s %s\n' "$label" "$lookback" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >"$SCALE_STATE"
	heartbeat_values
	log "redeploying Heartbeat with the patientzero-scale collector: $SCALE_DATABASE from $lookback back, as $label"
	redeploy
	log "extracting; make patientzero-scale-check compares Loki with $SCALE_DIR/expected.json once it caught up"
}

# Runs oslog-scale check against Loki through a port-forward. It needs no SQL
# Server: the known answers are in expected.json.
cmd_scale_check() {
	cluster_exists || die "kind cluster $CLUSTER does not exist"
	[ -f "$SCALE_STATE" ] || die "the patientzero-scale collector is not running; run make patientzero-scale-collect"
	read -r label lookback started <"$SCALE_STATE"
	work=$(mktemp -d "${TMPDIR:-/tmp}/patientzero-scale-check.XXXXXX")
	kubectl_ -n "$HEARTBEAT_NAMESPACE" port-forward svc/loki :3100 >"$work/port-forward.log" 2>&1 &
	forward=$!
	trap 'kill "$forward" 2>/dev/null; rm -rf "$work"' EXIT
	trap 'exit 130' INT TERM
	i=0
	until grep -q '^Forwarding from 127.0.0.1:' "$work/port-forward.log"; do
		i=$((i + 1))
		[ "$i" -le 30 ] || die "the port-forward to Loki did not start: $(cat "$work/port-forward.log")"
		sleep 1
	done
	port=$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9]*\) .*/\1/p' "$work/port-forward.log" | head -n 1)
	log "checking Loki's $label rows against $SCALE_DIR/expected.json"
	GOCACHE=${GOCACHE:-$(pwd)/.tmp/gocache} go -C tools/patientzero run ./cmd/oslog-scale \
		-loki "http://127.0.0.1:$port" -environment "$label" -collected-from "$started" -lookback "$lookback" check
}

# Removes the patientzero-scale collector first, so it does not read a
# dropped database.
cmd_scale_down() {
	if [ -f "$SCALE_STATE" ]; then
		rm -f "$SCALE_STATE"
		if cluster_exists; then
			heartbeat_values
			log "redeploying Heartbeat without the patientzero-scale collector"
			redeploy
		fi
	fi
	oslog_scale drop
}

case "${1:-}" in
up) cmd_up ;;
down) cmd_down ;;
load) cmd_load ;;
load-stop) cmd_load_stop ;;
scenario) cmd_scenario ;;
scenarios) cmd_scenarios ;;
scale) cmd_scale ;;
scale-measure) cmd_scale_measure ;;
scale-collect) cmd_scale_collect ;;
scale-check) cmd_scale_check ;;
scale-down) cmd_scale_down ;;
*) sed -n '2,/^set -eu/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//' >&2; exit 2 ;;
esac
