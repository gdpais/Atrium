#!/bin/sh
# End-to-end acceptance checks for the kind/Helm delivery (ADR 0003). CI runs
# it; `make kind-e2e` runs it locally next to your dev cluster without touching
# it.
#
# By default it creates its own uniquely named cluster, kubeconfig and SQL
# Server container, and removes exactly those on exit, so concurrent runs are
# isolated and never delete developer resources. Set E2E_CLUSTER to run against
# an existing kind cluster instead (it is then left in place).
#
# Needs: docker, kind, kubectl, helm, jq. Prints measured evidence as it goes.
set -eu
cd "$(dirname "$0")/.."

: "${KIND_NODE_IMAGE:?set KIND_NODE_IMAGE (the Makefile pins it)}"
run_id=$(date +%s)-$$
if [ -n "${E2E_CLUSTER:-}" ]; then
	CLUSTER=$E2E_CLUSTER
	own_cluster=false
else
	CLUSTER=heartbeat-e2e-$run_id
	own_cluster=true
fi
NAMESPACE=heartbeat
CONTEXT=kind-$CLUSTER
SQL_CONTAINER=$CLUSTER-sqlserver
WORK=$(mktemp -d)

export KIND_CLUSTER=$CLUSTER HEARTBEAT_NAMESPACE=$NAMESPACE
export SQLSERVER_CONTAINER=$SQL_CONTAINER SQLSERVER_ENV_FILE=$WORK/sqlserver.env
# No host port: the test target is reachable only on the kind network.
export SQLSERVER_HOST_PORT=
if $own_cluster; then
	# A private kubeconfig and no host port mappings, so this run cannot clash
	# with the developer cluster or another run.
	export KUBECONFIG=$WORK/kubeconfig
	export KIND_CONFIG=${E2E_KIND_CONFIG-infra/kind/e2e.yaml}
fi

port_forward_pid=
# The NetworkPolicy step's second namespace.
other_ns=kind-e2e-netpol
cleanup() {
	status=$?
	[ -z "$port_forward_pid" ] || kill "$port_forward_pid" 2>/dev/null || true
	if ! $own_cluster; then
		kubectl --context "$CONTEXT" delete namespace "$other_ns" --ignore-not-found --wait=false >/dev/null 2>&1 || true
	fi
	if [ "$status" -ne 0 ] && kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
		echo "--- diagnostics"
		kubectl --context "$CONTEXT" -n "$NAMESPACE" get pods -o wide || true
		kubectl --context "$CONTEXT" -n "$NAMESPACE" logs statefulset/db-collector --tail 50 || true
		echo "--- API, its migrations and its dependencies"
		kubectl --context "$CONTEXT" -n "$NAMESPACE" logs deployment/api -c migrate --tail 20 || true
		kubectl --context "$CONTEXT" -n "$NAMESPACE" logs deployment/api -c api --tail 30 || true
		kubectl --context "$CONTEXT" -n "$NAMESPACE" logs statefulset/postgresql --tail 20 || true
		# Where log rows went: the Collector's export attempts and errors, its
		# logs exporter's queue and counts, and the rows Loki discarded.
		echo "--- OTel Collector"
		kubectl --context "$CONTEXT" -n "$NAMESPACE" logs otel-collector-0 --tail 100 || true
		kubectl --context "$CONTEXT" get --raw "/api/v1/namespaces/$NAMESPACE/pods/otel-collector-0:8888/proxy/metrics" 2>/dev/null |
			grep -E '^otelcol_(exporter|receiver)_[a-z_]*log_records|^otelcol_exporter_(queue_size|in_flight_requests)' || true
		echo "--- Loki discarded samples, by reason"
		kubectl --context "$CONTEXT" get --raw "/api/v1/namespaces/$NAMESPACE/services/loki:3100/proxy/metrics" 2>/dev/null |
			grep '^loki_discarded_samples_total' || true
	fi
	docker rm -f "$SQL_CONTAINER" >/dev/null 2>&1 || true
	docker volume rm "$SQL_CONTAINER-data" >/dev/null 2>&1 || true
	if $own_cluster; then
		kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
		rm -rf ".tmp/kind/$CLUSTER"
	fi
	rm -rf "$WORK"
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

k() { kubectl --context "$CONTEXT" "$@"; }
step() { printf '\n=== %s\n' "$*"; }
pass() { printf 'PASS  %s\n' "$*"; }
fail() { printf 'FAIL  %s\n' "$*" >&2; exit 1; }

# GET through the API server's service proxy: no port-forwards or host ports.
svc_get() { k get --raw "/api/v1/namespaces/$NAMESPACE/services/$1/proxy$2"; }
prom() { svc_get prometheus:9090 "/api/v1/query?query=$(printf '%s' "$1" | jq -sRr @uri)"; }
# Prints the first sample value of a PromQL query, or nothing.
prom_value() { prom "$1" | jq -r '.data.result[0].value[1] // empty'; }
# The pod proxy reaches the collector even while it is unready.
pod_get() { k get --raw "/api/v1/namespaces/$NAMESPACE/pods/db-collector-0:8082/proxy$1"; }

# Operator access to the admin endpoints, the documented way: kubectl
# port-forward (not subject to the NetworkPolicy) plus the admin token. The API
# server's proxy cannot be used: it drops the Authorization header.
# kubectl runs directly, not through k, so $! is kubectl itself and kill
# stops it (a backgrounded function would leave it orphaned). The token goes
# to curl in a header file, never on a command line.
admin_forward() {
	kubectl --context "$CONTEXT" -n "$NAMESPACE" port-forward pod/db-collector-0 :8082 >"$WORK/port-forward.log" 2>&1 &
	port_forward_pid=$!
	wait_for 30 "port-forward to the collector" grep -q '^Forwarding from 127.0.0.1:' "$WORK/port-forward.log"
	admin_port=$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9]*\) .*/\1/p' "$WORK/port-forward.log" | head -n 1)
	(
		umask 077
		k -n "$NAMESPACE" get secret heartbeat-runtime -o json |
			jq -r '"Authorization: Bearer " + (.data["admin-token"] | @base64d)' >"$WORK/admin-header"
	)
}
admin_unforward() {
	kill "$port_forward_pid" 2>/dev/null || true
	port_forward_pid=
}
# admin_status <path> [header]: HTTP status of GET <path> through the port-forward.
admin_status() {
	if [ -n "${2:-}" ]; then
		curl -sS -o /dev/null -w '%{http_code}' --max-time 5 -H "$2" "http://127.0.0.1:$admin_port$1"
	else
		curl -sS -o /dev/null -w '%{http_code}' --max-time 5 "http://127.0.0.1:$admin_port$1"
	fi
}
admin_config() {
	curl -sS --fail --max-time 5 -H "@$WORK/admin-header" "http://127.0.0.1:$admin_port/admin/config"
}

# wait_for <seconds> <description> <command...>: retries until the command succeeds.
wait_for() {
	limit=$1 what=$2
	shift 2
	start=$(date +%s)
	until "$@" >/dev/null 2>&1; do
		[ $(($(date +%s) - start)) -lt "$limit" ] || fail "timed out after ${limit}s waiting for $what"
		sleep 3
	done
	printf '      %s after %ss\n' "$what" $(($(date +%s) - start))
}

collector_pod_uid() { k -n "$NAMESPACE" get pod db-collector-0 -o jsonpath='{.metadata.uid}'; }
collector_restarts() { k -n "$NAMESPACE" get pod db-collector-0 -o jsonpath='{.status.containerStatuses[0].restartCount}'; }
config_version() { admin_config | jq -r '.version // empty'; }
target_up() { [ "$(prom_value "heartbeat_collector_target_up{target=\"$1\"}")" = "$2" ]; }
version_changed_from() { [ "$(config_version)" != "$1" ]; }
reload_error_is_set() { admin_config | jq -e '.last_reload_err != ""'; }
reload_error_is_clear() { admin_config | jq -e '.last_reload_err == ""'; }
# The last reload's result as Prometheus scraped it (1 ok, 0 failed), and
# whether its alert is pending or firing.
reload_successful_is() { value_is 'heartbeat_collector_config_last_reload_successful{job="db-collector"}' "$1"; }
reload_alert_active() {
	svc_get prometheus:9090 /api/v1/alerts | jq -e 'any(.data.alerts[]; .labels.alertname == "HeartbeatCollectorConfigReloadFailed")'
}
reload_alert_gone() {
	svc_get prometheus:9090 /api/v1/alerts | jq -e 'all(.data.alerts[]; .labels.alertname != "HeartbeatCollectorConfigReloadFailed")'
}
rejected_reloads() { prom_value 'sum(heartbeat_collector_config_reloads_total{result="invalid_candidate"})'; }
rejected_reloads_above() { now=$(rejected_reloads); [ -n "$now" ] && gt "$now" "$1"; }
watchdog_active() { svc_get alertmanager:9093 /api/v2/alerts | jq -e 'any(.[]; .labels.alertname == "Watchdog")'; }
last_success() { prom_value "heartbeat_collector_target_last_success_timestamp_seconds{target=\"$1\"}"; }
# gt <a> <b>: numeric a > b, for decimals too.
gt() { awk -v a="$1" -v b="$2" 'BEGIN { exit !(a > b) }'; }
success_after() { now=$(last_success "$1"); [ -n "$now" ] && gt "$now" "$2"; }
replacement_ready() {
	[ "$(collector_pod_uid)" != "$1" ] &&
		[ "$(k -n "$NAMESPACE" get pod db-collector-0 -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" = True ]
}
collector_waiting_on() {
	[ "$(k -n "$NAMESPACE" get pod db-collector-0 -o jsonpath='{.status.containerStatuses[0].state.waiting.reason}')" = "$1" ]
}
collector_image() { k -n "$NAMESPACE" get pod db-collector-0 -o jsonpath='{.status.containerStatuses[0].image}'; }
collector_policy() { k -n "$NAMESPACE" get statefulset db-collector -o jsonpath='{.spec.podManagementPolicy}'; }
# True when db-collector-0 is Ready, not terminating, and on the StatefulSet's
# update revision. kubectl rollout status can report a new StatefulSet done
# while it still has to replace an adopted pod.
collector_on_update_revision() {
	update=$(k -n "$NAMESPACE" get statefulset db-collector -o jsonpath='{.status.updateRevision}')
	[ -n "$update" ] && [ "$(k -n "$NAMESPACE" get pod db-collector-0 \
		-o jsonpath='{.metadata.labels.controller-revision-hash}/{.status.conditions[?(@.type=="Ready")].status}/{.metadata.deletionTimestamp}')" = "$update/True/" ]
}
prometheus_answers() { [ -n "$(prom_value prometheus_tsdb_lowest_timestamp_seconds)" ]; }
# Prints the type Prometheus scraped for a metric (counter, gauge, ...).
metric_type() { svc_get prometheus:9090 "/api/v1/metadata?metric=$1" | jq -r --arg m "$1" '.data[$m][0].type // empty'; }
has_value() { [ -n "$(prom_value "$1")" ]; }
# pod_phase <pod> [namespace], pod_done <pod> [namespace]: a one-shot pod's phase.
pod_phase() { k -n "${2:-$NAMESPACE}" get pod "$1" -o jsonpath='{.status.phase}'; }
pod_done() { case "$(pod_phase "$@")" in Succeeded | Failed) true ;; *) false ;; esac; }
# ephemeral_done <pod> <container>: an ephemeral container of the pod has exited.
ephemeral_done() {
	[ -n "$(k -n "$NAMESPACE" get pod "$1" \
		-o jsonpath="{.status.ephemeralContainerStatuses[?(@.name==\"$2\")].state.terminated.reason}")" ]
}
# Lines Loki returns for a LogQL query over the last hour.
loki_count() {
	svc_get loki:3100 "/loki/api/v1/query_range?limit=1000&query=$(printf '%s' "$1" | jq -sRr @uri)" |
		jq '[.data.result[].values[]] | length'
}
# urldecode: standard input with each %XX decoded, one byte each.
urldecode() {
	LC_ALL=C awk '{
		out = ""
		while (match($0, /%[0-9A-Fa-f][0-9A-Fa-f]/)) {
			hex = toupper(substr($0, RSTART + 1, 2))
			out = out substr($0, 1, RSTART - 1) sprintf("%c", (index("0123456789ABCDEF", substr(hex, 1, 1)) - 1) * 16 + index("0123456789ABCDEF", substr(hex, 2, 1)) - 1)
			$0 = substr($0, RSTART + 3)
		}
		print out $0
	}'
}
loki_count_is() { [ "$(loki_count "$1")" = "$2" ]; }
value_is() { [ "$(prom_value "$1")" = "$2" ]; }
target_fresh() {
	age=$(prom_value "time() - heartbeat_collector_target_last_success_timestamp_seconds{target=\"$1\"}")
	[ -n "$age" ] && [ "$(printf '%.0f' "$age")" -lt 60 ]
}
loki_query() { svc_get loki:3100 "/loki/api/v1/query_range?since=15m&limit=10&query=$(printf '%s' "$1" | jq -sRr @uri)"; }
log_found() { loki_query "$1" | jq -e '.data.result | length > 0'; }
# The number a LogQL instant query returns, or 0 when it returns nothing.
loki_value() {
	svc_get loki:3100 "/loki/api/v1/query?query=$(printf '%s' "$1" | jq -sRr @uri)" | jq -r '.data.result[0].value[1] // "0"'
}
# The size of the OTel Collector's logs export queue, in the queue's sizer
# (requests in the chart's), from its own metrics (port 8888, which nothing
# scrapes; the pod proxy reaches it).
otel_queue_size() {
	k get --raw "/api/v1/namespaces/$NAMESPACE/pods/otel-collector-0:8888/proxy/metrics" |
		awk '/^otelcol_exporter_queue_size[{]/ && /exporter="otlp_http\/logs"/ { printf "%.0f\n", $2; exit }'
}
otel_queue_holds() { size=$(otel_queue_size) && [ -n "$size" ] && gt "$size" 0; }
# The Collector's logs exporter as key=value words: requests queued and in
# flight (in_flight counts a request in its retry backoff too), and log
# records sent and dropped for good (send_failed: Loki refused them with a
# permanent error). ? when the Collector does not answer.
otel_logs_state() {
	k get --raw "/api/v1/namespaces/$NAMESPACE/pods/otel-collector-0:8888/proxy/metrics" |
		awk '/exporter="otlp_http\/logs"/ {
			if (/^otelcol_exporter_queue_size[{]/) queued = $NF + 0
			if (/^otelcol_exporter_in_flight_requests[{]/) in_flight = $NF + 0
			if (/^otelcol_exporter_sent_log_records(_total)?[{]/) sent += $NF
			if (/^otelcol_exporter_send_failed_log_records(_total)?[{]/) dropped += $NF
		} END { printf "queued=%s in_flight=%s sent=%d dropped=%d\n", (queued == "" ? "?" : queued), (in_flight == "" ? "?" : in_flight), sent, dropped }'
}
# The rows Loki discarded since it started, as <reason>=<rows> words, or
# none. Rows refused with a 429 (rate_limited, per_stream_rate_limit) count
# too, though the Collector retries them.
loki_discarded() {
	svc_get loki:3100 /metrics | awk '/^loki_discarded_samples_total[{]/ {
		match($0, /reason="[^"]*"/); rows[substr($0, RSTART + 8, RLENGTH - 9)] += $NF
	} END { for (reason in rows) { printf "%s%s=%d", sep, reason, rows[reason]; sep = " " } print (sep == "" ? "none" : "") }'
}
otel_pod_uid() { k -n "$NAMESPACE" get pod otel-collector-0 -o jsonpath='{.metadata.uid}'; }
otel_replacement_ready() {
	[ "$(otel_pod_uid)" != "$1" ] &&
		[ "$(k -n "$NAMESPACE" get pod otel-collector-0 -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" = True ]
}

# The API's pod (one replica on kind), its readiness, and psql in the bundled
# PostgreSQL as the database's owner, over the pod's Unix socket.
api_pod() { k -n "$NAMESPACE" get pod -l app.kubernetes.io/name=api -o jsonpath='{.items[0].metadata.name}'; }
api_ready_is() { [ "$(k -n "$NAMESPACE" get pod "$(api_pod)" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" = "$1" ]; }
psql_() { k -n "$NAMESPACE" exec postgresql-0 -- psql -U heartbeat -d heartbeat -Atc "$1"; }
schema_version() { psql_ "SELECT version || CASE WHEN dirty THEN ' (dirty)' ELSE '' END FROM schema_migrations"; }
schema_version_is() { [ "$(schema_version)" = "$1" ]; }
outbox_empty() { [ "$(psql_ 'SELECT count(*) FROM audit_outbox')" = 0 ]; }
# The newest migration in db/migrations, as golang-migrate numbers it.
newest_migration() { ls db/migrations | sed -n 's/^0*\([0-9][0-9]*\)_.*\.up\.sql$/\1/p' | sort -n | tail -n 1; }
# A trace of the API's in Tempo, from the last ten minutes, rooted at the
# span of an unrouted request: nothing else asks the API for one.
api_trace() {
	query='{resource.service.name="api" && name="GET unmatched"}'
	svc_get tempo:3200 "/api/search?limit=1&start=$(($(date +%s) - 600))&end=$(($(date +%s) + 60))&q=$(printf '%s' "$query" | jq -sRr @uri)" |
		jq -er '.traces[0].traceID'
}

# ----------------------------------------------------------------------------
step "cluster $CLUSTER, images, SQL Server target"
scripts/kind.sh cluster
scripts/kind.sh images
. "./.tmp/kind/$CLUSTER/images.env"

password="E2e-$(openssl rand -hex 16)"
umask 077
# sa is for setup and the session sampler below; the collector logs in as a
# least-privilege login that kind.sh sqlserver-up creates.
printf 'MSSQL_SA_PASSWORD=%s\nHEARTBEAT_CREDENTIAL_ENV_SQLSERVER_DEV=heartbeat_collector:%s\n' \
	"$password" "E2e-$(openssl rand -hex 16)" >"$SQLSERVER_ENV_FILE"
umask 022
# Points the collector at this run's container, plus a second target that can
# never connect, to show that one failed target does not stop the others.
sed -e "s/host: heartbeat-sqlserver-dev/host: $SQL_CONTAINER/" infra/helm/values/sqlserver-dev.yaml >"$WORK/e2e.yaml"
cat >>"$WORK/e2e.yaml" <<EOF
          - name: unreachable
            environment: local-dev
            host: $CLUSTER-missing-sqlserver
            port: 1433
            database_name: master
            credential_ref: env/sqlserver-dev
EOF
# The test's own OTLP sender (the identifier step) is admitted to the OTel
# Collector as an extra peer, next to kind.yaml's operator range: lists
# replace, they do not merge.
cat >"$WORK/e2e-netpol.yaml" <<EOF
backendNetworkPolicies:
  otelCollector:
    extraPeers:
      - ipBlock:
          cidr: 0.0.0.0/0
          except:
            - 10.244.0.0/16
      - podSelector:
          matchLabels:
            app.kubernetes.io/name: kind-e2e-otlp-sender
EOF
base_values="$WORK/e2e-netpol.yaml ${E2E_EXTRA_VALUES:-}"
export EXTRA_VALUES="$WORK/e2e.yaml $base_values"
scripts/kind.sh sqlserver-up

# ----------------------------------------------------------------------------
step "every workload is ready"
for workload in $(k -n "$NAMESPACE" get deployment,statefulset -o name); do
	k -n "$NAMESPACE" rollout status "$workload" --timeout 5m >/dev/null
	pass "$workload ready"
done

step "the running collector is the image just built"
arch=$(docker exec "$CLUSTER-control-plane" uname -m)
running=$(k -n "$NAMESPACE" get pod db-collector-0 -o jsonpath='{.status.containerStatuses[0].image}')
case "$running" in
*"heartbeat/db-collector:$DB_COLLECTOR_TAG") pass "db-collector runs $DB_COLLECTOR_TAG (content-derived tag, node arch $arch)" ;;
*) fail "db-collector runs $running, expected tag $DB_COLLECTOR_TAG" ;;
esac
for image in $(k -n "$NAMESPACE" get pod "$(api_pod)" -o jsonpath='{.status.containerStatuses[0].image} {.status.initContainerStatuses[0].image}'); do
	case "$image" in
	*"heartbeat/api:$API_TAG") ;;
	*) fail "the API or its migrate init container runs $image, expected tag $API_TAG" ;;
	esac
done
pass "the API and its migrations run $API_TAG"

# ----------------------------------------------------------------------------
step "the API is Ready only after its migrations, at the newest schema"
# The init container applied every migration before the API started, and
# the database is at the newest one this checkout has, not dirty.
newest=$(newest_migration)
[ -n "$newest" ] || fail "no migrations in db/migrations"
pod=$(api_pod)
migrate_status() { k -n "$NAMESPACE" get pod "$pod" -o jsonpath="{.status.initContainerStatuses[0].$1}"; }
[ "$(migrate_status state.terminated.exitCode)" = 0 ] ||
	fail "the migrate init container exited with [$(migrate_status state.terminated.exitCode)]"
migrate_done=$(migrate_status state.terminated.finishedAt)
migrate_restarts=$(migrate_status restartCount)
api_started=$(k -n "$NAMESPACE" get pod "$pod" -o jsonpath='{.status.containerStatuses[0].state.running.startedAt}')
ready_at=$(k -n "$NAMESPACE" get pod "$pod" -o jsonpath='{.status.conditions[?(@.type=="Ready")].lastTransitionTime}')
# RFC 3339 times in UTC sort as text.
[ "$(printf '%s\n%s\n' "$migrate_done" "$api_started" | sort | head -n 1)" = "$migrate_done" ] ||
	fail "the API started at $api_started, before its migrations ended at $migrate_done"
k -n "$NAMESPACE" logs "$pod" -c migrate | grep '"msg":"migrate: schema up to date"' | jq -e --argjson v "$newest" '.to == $v' >/dev/null ||
	fail "the migrate init container did not bring the schema to version $newest: $(k -n "$NAMESPACE" logs "$pod" -c migrate)"
[ "$(schema_version)" = "$newest" ] || fail "schema_migrations is at [$(schema_version)], want $newest"
svc_get api:8081 /readyz | jq -e '.status == "ready"' >/dev/null || fail "the API's /readyz is not ready"
pass "migrations to version $newest ended $migrate_done ($migrate_restarts restarts while PostgreSQL started); API started $api_started, Ready $ready_at"
# Readiness follows the schema: one version behind, the pod leaves the
# Service; restored, it returns, without a restart.
restarts_before=$(k -n "$NAMESPACE" get pod "$pod" -o jsonpath='{.status.containerStatuses[0].restartCount}')
psql_ "UPDATE schema_migrations SET version = version - 1" >/dev/null
wait_for 60 "the API unready on an older schema" api_ready_is False
psql_ "UPDATE schema_migrations SET version = $newest" >/dev/null
wait_for 60 "the API ready again" api_ready_is True
[ "$(k -n "$NAMESPACE" get pod "$pod" -o jsonpath='{.status.containerStatuses[0].restartCount}')" = "$restarts_before" ] ||
	fail "the API restarted while its schema was behind; liveness must not depend on PostgreSQL"
pass "on schema $((newest - 1)) the API left its Service and came back at $newest, same container"

# ----------------------------------------------------------------------------
step "SQL Server target is collected and visible"
wait_for 180 "target up in Prometheus" target_up sqlserver-dev 1
wait_for 60 "fresh last success" target_fresh sqlserver-dev
sessions=$(prom_value "sum(heartbeat_sqlserver_sessions{target=\"sqlserver-dev\"})")
[ -n "$sessions" ] || fail "no heartbeat_sqlserver_sessions for sqlserver-dev"
pass "probe metrics present (sessions=$sessions)"
sysadmin=$(prom_value "heartbeat_collector_target_login_sysadmin{target=\"sqlserver-dev\"}")
[ "$sysadmin" = 0 ] || fail "heartbeat_collector_target_login_sysadmin for sqlserver-dev is '$sysadmin', expected 0"
pass "the collector logs in without sysadmin (least-privilege login)"
# Cumulative SQL Server values are scraped as counters, so rate() applies.
for metric in heartbeat_sqlserver_wait_seconds_total heartbeat_sqlserver_batch_requests_total heartbeat_sqlserver_transactions_total; do
	type=$(metric_type "$metric")
	[ "$type" = counter ] || fail "$metric has type [$type], expected counter"
done
pass "waits and throughput exported as counters"
# One server-wide series per throughput counter (issue #4).
for metric in heartbeat_sqlserver_batch_requests_total heartbeat_sqlserver_transactions_total; do
	count=$(prom_value "count($metric{target=\"sqlserver-dev\"})")
	[ "$count" = 1 ] || fail "$metric has [$count] series for sqlserver-dev, expected 1"
done
wait_for 120 "batch request rate" has_value "rate(heartbeat_sqlserver_batch_requests_total{target=\"sqlserver-dev\"}[1m])"
pass "one series per throughput counter, and rate() returns a value"
# Core CPU, buffer cache and file I/O signals. The scheduler monitor writes its
# first CPU record about a minute after SQL Server starts.
wait_for 180 "CPU utilisation" has_value "heartbeat_sqlserver_cpu_sql_process_ratio{target=\"sqlserver-dev\"}"
for metric in heartbeat_sqlserver_page_life_expectancy_seconds heartbeat_sqlserver_buffer_cache_hit_ratio; do
	[ -n "$(prom_value "$metric{target=\"sqlserver-dev\"}")" ] || fail "no $metric for sqlserver-dev"
done
type=$(metric_type heartbeat_sqlserver_database_file_reads_total)
[ "$type" = counter ] || fail "heartbeat_sqlserver_database_file_reads_total has type [$type], expected counter"
files=$(prom_value 'count(heartbeat_sqlserver_database_file_reads_total{target="sqlserver-dev"})')
wait_for 120 "file read rate" has_value "rate(heartbeat_sqlserver_database_file_reads_total{target=\"sqlserver-dev\"}[1m])"
pass "cpu, buffer_cache and file_io series present ($files database files)"
# Self-observability: Go runtime and process metrics, probe durations, and
# error counters created at 0 for every scheduled probe (4 reasons each).
for query in 'go_goroutines{job="db-collector"}' 'process_resident_memory_bytes{job="db-collector"}' \
	'heartbeat_collector_probe_duration_seconds_count{target="sqlserver-dev"}'; do
	[ -n "$(prom_value "$query")" ] || fail "no $query"
done
probes=$(prom_value 'count(heartbeat_collector_probe_duration_seconds_count{target="sqlserver-dev"})')
errors=$(prom_value 'count(heartbeat_collector_probe_errors_total{target="sqlserver-dev"})')
[ -n "$probes" ] && [ "$errors" = "$((probes * 4))" ] ||
	fail "probe error counters: [$errors] series for [$probes] probes, expected 4 per probe"
pass "collector self-metrics present ($probes probes timed, $errors error counters)"
svc_get grafana:3000 /api/dashboards/uid/sqlserver-overview | jq -e '.dashboard.uid == "sqlserver-overview"' >/dev/null ||
	fail "Grafana has no sqlserver-overview dashboard"
seen=$(svc_get grafana:3000 "/api/datasources/proxy/uid/prometheus/api/v1/query?query=heartbeat_collector_target_up" |
	jq -r '[.data.result[].metric.target] | sort | join(",")')
case ",$seen," in
*,sqlserver-dev,*) pass "Grafana's Prometheus datasource returns the target ($seen)" ;;
*) fail "Grafana does not see sqlserver-dev (saw: $seen)" ;;
esac

step "a failed target is isolated"
wait_for 120 "unreachable target reported down" target_up unreachable 0
target_up sqlserver-dev 1 || fail "healthy target stopped when another failed"
pass "unreachable target down, sqlserver-dev still up"
wait_for 60 "probe errors counted for the unreachable target" has_value \
	'sum(heartbeat_collector_probe_errors_total{target="unreachable"}) > 0'
pass "probe errors counted for the unreachable target"

step "Prometheus alerts reach Alertmanager (Watchdog)"
wait_for 120 "Watchdog in Alertmanager" watchdog_active
pass "Watchdog active in Alertmanager"

step "log identifiers reach Loki as structured metadata, not labels"
# One OTLP/JSON log record into the Collector. Loki's otlp_config indexes only
# service.name, service.namespace, deployment.environment.name and
# outsystems.log_type.
user_id=e2e-user-$run_id session_id=e2e-session-$run_id request_id=e2e-request-$run_id client_ip=203.0.113.42
cat >"$WORK/otlp-log.json" <<EOF
{"resourceLogs": [{
  "resource": {"attributes": [
    {"key": "service.name", "value": {"stringValue": "e2e-identifiers"}},
    {"key": "deployment.environment.name", "value": {"stringValue": "e2e"}}
  ]},
  "scopeLogs": [{"logRecords": [{
    "timeUnixNano": "$(date +%s)000000000",
    "severityText": "INFO",
    "body": {"stringValue": "kind-e2e identifier check"},
    "attributes": [
      {"key": "user_id", "value": {"stringValue": "$user_id"}},
      {"key": "session_id", "value": {"stringValue": "$session_id"}},
      {"key": "request_id", "value": {"stringValue": "$request_id"}},
      {"key": "client_ip", "value": {"stringValue": "$client_ip"}}
    ]
  }]}]
}]}
EOF
# Sent from a pod the Collector's NetworkPolicy admits (e2e-netpol.yaml).
# `kubectl create --raw` through the service proxy does not send the body as
# application/json, so the receiver would answer 415.
prom_image=$(k -n "$NAMESPACE" get deployment prometheus -o jsonpath='{.spec.template.spec.containers[?(@.name=="prometheus-server")].image}')
k -n "$NAMESPACE" delete pod otlp-sender --ignore-not-found --wait=true >/dev/null
k -n "$NAMESPACE" run otlp-sender --image="$prom_image" --restart=Never --labels=app.kubernetes.io/name=kind-e2e-otlp-sender \
	--command -- wget -q -T 10 -O /dev/null --header 'Content-Type: application/json' \
	--post-data "$(cat "$WORK/otlp-log.json")" http://otel-collector:4318/v1/logs >/dev/null
wait_for 120 "OTLP sender finished" pod_done otlp-sender
sender_phase=$(pod_phase otlp-sender)
sender_log=$(k -n "$NAMESPACE" logs otlp-sender 2>&1 || true)
k -n "$NAMESPACE" delete pod otlp-sender --wait=false >/dev/null
[ "$sender_phase" = Succeeded ] || fail "the Collector rejected the OTLP log record: $sender_log"
log_query="{service_name=\"e2e-identifiers\"} | user_id=\"$user_id\""
wait_for 120 "log line in Loki" log_found "$log_query"
stream=$(loki_query "$log_query" | jq -c '.data.result[0].stream')
printf '      %s\n' "$stream"
printf '%s' "$stream" | jq -e --arg u "$user_id" --arg s "$session_id" --arg r "$request_id" --arg c "$client_ip" \
	'.user_id == $u and .session_id == $s and .request_id == $r and .client_ip == $c' >/dev/null ||
	fail "Loki did not return all four identifiers"
pass "found by structured metadata filter; user, session and request IDs and client IP kept"
labels=$(svc_get loki:3100 "/loki/api/v1/labels?since=15m" | jq -r '.data | sort | join(",")')
case ",$labels," in
*,user_id,* | *,session_id,* | *,request_id,* | *,client_ip,*) fail "an identifier became a Loki label ($labels)" ;;
*,service_name,*) pass "Loki labels: $labels" ;;
*) fail "service_name is not a Loki label ($labels)" ;;
esac

# ----------------------------------------------------------------------------
step "OutSystems logs are extracted to Loki (outsystems_logs)"
# The fixture oslog_Error cycle tables and views in their own database,
# readable by the collector's login through SELECT on the two views only, with
# rows from a few minutes ago.
oslog_login=$(sed -n 's/^HEARTBEAT_CREDENTIAL_ENV_SQLSERVER_DEV=\([^:]*\):.*/\1/p' "$SQLSERVER_ENV_FILE")
# The surveyed schema has no defaults for Id and the other NOT NULL columns,
# so the rows name them all. They go into oslog_Error_3, the cycle table the
# oslog_Error view reads, as the platform writes them.
fixture_output=$({
	printf 'CREATE DATABASE heartbeat_oslog_e2e;\nGO\nUSE heartbeat_oslog_e2e;\nGO\n'
	cat services/db-collector/internal/collectors/outsystems/testdata/oslog/error.sql
	printf '\nGO\nCREATE USER [%s] FOR LOGIN [%s];\nGRANT SELECT ON dbo.oslog_Error TO [%s];\nGRANT SELECT ON dbo.oslog_Error_Previous TO [%s];\n' "$oslog_login" "$oslog_login" "$oslog_login" "$oslog_login"
	printf '%s\n' "INSERT INTO dbo.oslog_Error_3 (Id, Instant, Session_Id, User_Id, Espace_Id, Tenant_Id, Message, Module_Name, Server, Espace_Name, Request_Key) VALUES" \
		"('e2e-error-1', DATEADD(SECOND, -180, GETUTCDATE()), '', 0, 1, 0, 'e2e order 1 failed', 'E2eShop', 'e2e', 'E2eShop', 'e2e-1')," \
		"('e2e-error-2', DATEADD(SECOND, -120, GETUTCDATE()), '', 0, 1, 0, 'e2e order 2 failed', 'E2eShop', 'e2e', 'E2eShop', 'e2e-2')," \
		"('e2e-error-3', DATEADD(SECOND, -120, GETUTCDATE()), '', 0, 1, 0, 'e2e order 3 failed', 'E2eShop', 'e2e', 'E2eShop', 'e2e-3');" GO
} | SQLCMDPASSWORD=$password docker exec -i -e SQLCMDPASSWORD "$SQL_CONTAINER" \
	/opt/mssql-tools18/bin/sqlcmd -S localhost -U sa -C -b 2>&1) ||
	fail "loading the oslog_Error fixture failed: $fixture_output"
cp "$WORK/e2e.yaml" "$WORK/e2e-oslogs.yaml"
cat >>"$WORK/e2e-oslogs.yaml" <<EOF
    - id: oslogs-e2e
      kind: outsystems_logs
      enabled: true
      config:
        environment: local-dev
        scrape_interval: 10s
        lag: 10s
        probes:
          - name: error
        targets:
          - name: logging-db
            host: $SQL_CONTAINER
            port: 1433
            database_name: heartbeat_oslog_e2e
            credential_ref: env/sqlserver-dev
EOF
EXTRA_VALUES="$WORK/e2e-oslogs.yaml $base_values" scripts/kind.sh deploy >/dev/null
# kubelet propagates the ConfigMap, then the collector hot-reloads it.
wait_for 240 "3 rows extracted" value_is 'sum(heartbeat_outsystems_extract_rows_total{collector="oslogs-e2e"})' 3
wait_for 120 "3 log lines in Loki" loki_count_is '{service_name="E2eShop"}' 3
loki_count_is '{service_name="E2eShop"} | Request_Key="e2e-2"' 1 ||
	fail "Loki has no line with the Request_Key attribute e2e-2 as structured metadata"
loki_count_is '{service_name="E2eShop", outsystems_log_type="error"}' 3 ||
	fail "the OutSystems rows are not indexed under the label outsystems_log_type=\"error\""
target_up logging-db 1 || fail "the logging-db target is not up"
[ -n "$(prom_value 'heartbeat_outsystems_extract_lag_seconds{collector="oslogs-e2e"}')" ] ||
	fail "no heartbeat_outsystems_extract_lag_seconds for oslogs-e2e"
pass "3 OutSystems error rows in Loki as {service_name=\"E2eShop\", outsystems_log_type=\"error\"}, attributes as structured metadata"

# ----------------------------------------------------------------------------
step "Heartbeat's own logs and traces reach Loki and Tempo, linked by trace ID"
own_logs='{service_namespace="heartbeat",service_name="db-collector",deployment_environment_name="local"}'
wait_for 120 "the collector's own logs in Loki" log_found "$own_logs"
[ "$(prom_value 'sum(heartbeat_telemetry_log_records_total{outcome="exported"}) > 0' | cut -c1)" != "" ] ||
	fail "the collector counts no exported log records"
[ "$(prom_value 'heartbeat_service_info{service="db-collector",environment="local"}')" = 1 ] ||
	fail "no heartbeat_service_info for the db-collector"
# A cycle's trace, from the target that always fails: its warnings carry the
# trace ID, which leads to the trace.
traced_warning() { loki_query "$own_logs | trace_id!=\"\" | detected_level=\"warn\"" | jq -r '[.data.result[].stream.trace_id] | first // empty'; }
trace_has_cycle() {
	trace_id=$(traced_warning)
	[ -n "$trace_id" ] && svc_get tempo:3200 "/api/traces/$trace_id" |
		jq -e '[.batches[].scopeSpans[].spans[].name] | contains(["collector.cycle", "collector.target", "collector.probe"])'
}
wait_for 180 "a warning's trace ID leads to its collection cycle in Tempo" trace_has_cycle
pass "own logs in Loki under service_namespace=heartbeat; trace $trace_id holds the cycle, target and probe spans"
# The API's, wired the same way (ADR 0018).
api_logs='{service_namespace="heartbeat",service_name="api",deployment_environment_name="local"}'
wait_for 120 "the API's own logs in Loki" log_found "$api_logs |= \"readiness changed\""
wait_for 60 "Prometheus scrapes the API" value_is 'up{job="api"}' 1
[ "$(prom_value 'heartbeat_service_info{service="api",environment="local"}')" = 1 ] ||
	fail "no heartbeat_service_info for the API"
[ -n "$(prom_value 'sum(heartbeat_api_http_requests_total{route="/readyz"})')" ] ||
	fail "no heartbeat_api_http_requests_total for the API's readiness probes"
# Health and readiness requests are not traced, so a request for a path no
# route serves makes the trace. From Prometheus, the one pod the API's
# NetworkPolicy admits; the 404 is expected.
k -n "$NAMESPACE" exec deployment/prometheus -c prometheus-server -- \
	wget -q -T 5 -O /dev/null "http://api:8081/api/v1/kind-e2e-$run_id" >/dev/null 2>&1 || true
wait_for 120 "the API's request trace in Tempo" api_trace
pass "the API's logs in Loki, up{job=\"api\"} 1 with its request metrics, and its request trace $(api_trace) in Tempo"

# ----------------------------------------------------------------------------
step "an audit record reaches Loki through the Collector's logs pipeline"
# The first admin, made the documented way: the password on the standard
# input of `kubectl exec -i`, never on a command line. It stays in a file for
# the login step below. The command logs its audit record's id; the API's
# drainer sends the record to the Collector's 4318, the pipeline whose queue
# is on disk (ADR 0016), and Loki keeps it under service_name heartbeat-api,
# outside Heartbeat's own logs.
admin_email=kind-e2e-admin@example.com
admin_password=$WORK/admin-password
(
	umask 077
	openssl rand -hex 16 >"$admin_password"
)
created=$(k -n "$NAMESPACE" exec -i "$(api_pod)" -c api -- \
	/app/api users create-admin --email "$admin_email" <"$admin_password" 2>&1) ||
	fail "api users create-admin failed: $created"
record_id=$(printf '%s\n' "$created" | sed -n 's/.*"audit_record_id":"\([^"]*\)".*/\1/p' | head -n 1)
[ -n "$record_id" ] || fail "api users create-admin logged no audit_record_id: $created"
audit_query="{service_name=\"heartbeat-api\"} | event_name=\"heartbeat.audit\" | log_record_uid=\"$record_id\""
wait_for 120 "audit record $record_id in Loki" log_found "$audit_query"
loki_query "$audit_query" | jq -e '[.data.result[].values[][1] | fromjson | select(.entity_type == "user" and .action == "create")] | length > 0' >/dev/null ||
	fail "the audit record in Loki is not the user's creation: $(loki_query "$audit_query" | jq -c '.data.result[0].values[0][1]')"
wait_for 60 "the audit outbox empty" outbox_empty
pass "audit record $record_id (user create) in Loki under service_name=heartbeat-api; the outbox is empty"

# ----------------------------------------------------------------------------
step "the admin logs in, reads the session and logs out, from the site's origin"
# As a browser would, through `kubectl port-forward` to the API pod, which
# NetworkPolicy does not filter. The chart sets the origin unsafe requests
# must come from (api.siteOrigin); one from elsewhere is refused before the
# password is checked. The password and the session cookie stay in files,
# never on a command line.
site=$(k -n "$NAMESPACE" get deployment api -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="HEARTBEAT_API_SITE_ORIGIN")].value}')
[ -n "$site" ] || fail "the API has no HEARTBEAT_API_SITE_ORIGIN, so nobody can log in"
kubectl --context "$CONTEXT" -n "$NAMESPACE" port-forward "pod/$(api_pod)" :8081 >"$WORK/api-forward.log" 2>&1 &
port_forward_pid=$!
wait_for 30 "port-forward to the API" grep -q '^Forwarding from 127.0.0.1:' "$WORK/api-forward.log"
api_port=$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9]*\) .*/\1/p' "$WORK/api-forward.log" | head -n 1)
(
	umask 077
	jq -n --arg email "$admin_email" --rawfile password "$admin_password" \
		'{email: $email, password: ($password | rtrimstr("\n"))}' >"$WORK/login.json"
)
# api_call <method> <path> [curl options]: the HTTP status; the body is in
# $WORK/api-body.
api_call() {
	method=$1 path=$2
	shift 2
	curl -sS -o "$WORK/api-body" -w '%{http_code}' --max-time 10 -X "$method" "$@" "http://127.0.0.1:$api_port$path"
}
code=$(api_call POST /api/v1/session -H 'Origin: https://elsewhere.example' -H 'Content-Type: application/json' --data "@$WORK/login.json")
[ "$code" = 403 ] || fail "a login from another origin returned $code, want 403"
code=$(api_call POST /api/v1/session -D "$WORK/login-headers" -H "Origin: $site" -H 'Content-Type: application/json' --data "@$WORK/login.json")
[ "$code" = 201 ] || fail "the admin's login returned $code: $(cat "$WORK/api-body")"
grep -qiE '^set-cookie: __Host-heartbeat_session=[^;]+; Path=/; Max-Age=[0-9]+; HttpOnly; Secure; SameSite=Lax' "$WORK/login-headers" ||
	fail "the session cookie is not __Host-, HttpOnly, Secure and SameSite=Lax: $(grep -i '^set-cookie:' "$WORK/login-headers" | sed 's/=[^;]*;/=<token>;/')"
(
	umask 077
	sed -n 's/^[Ss]et-[Cc]ookie: \(__Host-heartbeat_session=[^;]*\);.*/Cookie: \1/p' "$WORK/login-headers" >"$WORK/session-header"
)
code=$(api_call GET /api/v1/session -H "@$WORK/session-header")
[ "$code" = 200 ] || fail "GET /api/v1/session with the cookie returned $code: $(cat "$WORK/api-body")"
jq -e --arg email "$admin_email" '.user.email == $email and .user.roles == ["admin"]' "$WORK/api-body" >/dev/null ||
	fail "GET /api/v1/session is not the admin's session: $(cat "$WORK/api-body")"
session_id=$(jq -r .id "$WORK/api-body")
code=$(api_call DELETE /api/v1/session -H "@$WORK/session-header" -H "Origin: $site")
[ "$code" = 204 ] || fail "the logout returned $code: $(cat "$WORK/api-body")"
code=$(api_call GET /api/v1/session -H "@$WORK/session-header")
[ "$code" = 401 ] || fail "the session still answers $code after the logout, want 401"
kill "$port_forward_pid" 2>/dev/null || true
port_forward_pid=
# The login and the logout reach Loki as audit records of the session.
session_query="{service_name=\"heartbeat-api\"} | event_name=\"heartbeat.audit\" |= \"$session_id\""
session_audited() {
	loki_query "$session_query" | jq -e --arg id "$session_id" \
		'[.data.result[].values[][1] | fromjson | select(.entity_type == "session" and .entity_id == $id) | .action] | sort == ["login", "logout"]'
}
wait_for 120 "the session's login and logout records in Loki" session_audited
pass "login from $site set the __Host- cookie; session $session_id is the admin's; logout ended it (401 after); both audited in Loki"

# ----------------------------------------------------------------------------
step "a telemetry source registered through the API is observed by the collector"
# ADR 0017 §4: the API reads the collector's redacted GET /admin/config with
# the collector's admin token, through the db-collector Service its
# NetworkPolicy admits it to. oslogs-e2e, deployed above, extracts
# environment local-dev, so an OutSystems source registered there is
# observed; one in an environment no collector extracts is registered too,
# and not observed. The admin logs in again: the step above logged out. The
# port-forward logs to a file of its own: the step above's still names the
# port it forwarded, which is closed now.
kubectl --context "$CONTEXT" -n "$NAMESPACE" port-forward "pod/$(api_pod)" :8081 >"$WORK/api-forward-sources.log" 2>&1 &
port_forward_pid=$!
wait_for 30 "port-forward to the API" grep -q '^Forwarding from 127.0.0.1:' "$WORK/api-forward-sources.log"
api_port=$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9]*\) .*/\1/p' "$WORK/api-forward-sources.log" | head -n 1)
code=$(api_call POST /api/v1/session -D "$WORK/login-headers" -H "Origin: $site" -H 'Content-Type: application/json' --data "@$WORK/login.json")
[ "$code" = 201 ] || fail "the admin's second login returned $code: $(cat "$WORK/api-body")"
(
	umask 077
	sed -n 's/^[Ss]et-[Cc]ookie: \(__Host-heartbeat_session=[^;]*\);.*/Cookie: \1/p' "$WORK/login-headers" >"$WORK/session-header"
)
# api_post <path> <json>: the status; the created entity is in $WORK/api-body.
api_post() { api_call POST "$1" -H "@$WORK/session-header" -H "Origin: $site" -H 'Content-Type: application/json' --data "$2"; }
# environment_id <slug>: the environment's ID, or nothing.
environment_id() {
	code=$(api_call GET /api/v1/environments -H "@$WORK/session-header")
	[ "$code" = 200 ] || fail "listing environments returned $code: $(cat "$WORK/api-body")"
	jq -r --arg slug "$1" '.items[] | select(.slug == $slug) | .id' "$WORK/api-body"
}
# register <slug> <application_name>: an application and its agentless
# OutSystems source, in the environment, created first unless it exists
# (local-dev is kind's inventory file's); prints the source's ID.
register() {
	env_id=$(environment_id "$1")
	if [ -z "$env_id" ]; then
		code=$(api_post /api/v1/environments "$(jq -nc --arg slug "$1" '{name: ("E2E " + $slug), slug: $slug, type: "development"}')")
		[ "$code" = 201 ] || fail "creating environment $1 returned $code: $(cat "$WORK/api-body")"
		env_id=$(jq -r .id "$WORK/api-body")
	fi
	code=$(api_post /api/v1/applications "$(jq -nc --arg env "$env_id" --arg name "$2" '{environment_id: $env, name: $name, platform: "outsystems_traditional"}')")
	[ "$code" = 201 ] || fail "creating application $2 returned $code: $(cat "$WORK/api-body")"
	app_id=$(jq -r .id "$WORK/api-body")
	code=$(api_post /api/v1/telemetry-sources "$(jq -nc --arg app "$app_id" --arg name "$2" '{application_id: $app, source_type: "outsystems", ingest_mode: "agentless", config: {application_name: $name}}')")
	[ "$code" = 201 ] || fail "registering the $2 source returned $code: $(cat "$WORK/api-body")"
	jq -e --arg env "$env_id" --arg app "$app_id" '.environment_id == $env and .application_id == $app' "$WORK/api-body" >/dev/null ||
		fail "the $2 source is not in its application's environment: $(cat "$WORK/api-body")"
	jq -r .id "$WORK/api-body"
}
# ADR 0021: kind's values declare local-dev, PatientZero and its source as
# the inventory file (api.inventory), which the API applied at start: the
# rows are the file's, and the API refuses to change them. Rows the API
# makes in that environment, as register's below, stay the API's.
inventory_applied() {
	[ "$(psql_ "SELECT string_agg(managed_by, ',' ORDER BY managed_by) FROM (
		SELECT managed_by FROM environments WHERE slug = 'local-dev'
		UNION ALL SELECT a.managed_by FROM applications a JOIN environments e ON e.id = a.environment_id
			WHERE e.slug = 'local-dev' AND a.name = 'PatientZero'
		UNION ALL SELECT s.managed_by FROM telemetry_sources s JOIN applications a ON a.id = s.application_id
			WHERE a.name = 'PatientZero' AND s.config->>'application_name' = 'PatientZero') r")" = config,config,config ]
}
wait_for 60 "kind's inventory file applied: local-dev, PatientZero and its source managed by the file" inventory_applied
# Prometheus has scraped the API since it applied the file, and it counted
# no invalid file or failed apply.
wait_for 60 "the API's inventory applies scraped, none invalid or failed" \
	value_is 'sum(heartbeat_api_inventory_applies_total{result=~"invalid|error"}) + 0 * sum(heartbeat_api_inventory_applied_timestamp_seconds)' 0
local_dev=$(environment_id local-dev)
code=$(api_call PATCH "/api/v1/environments/$local_dev" -H "@$WORK/session-header" -H "Origin: $site" -H 'Content-Type: application/json' --data '{"description": "changed by kind-e2e"}')
[ "$code" = 409 ] || fail "changing local-dev through the API returned $code, want 409: the inventory file manages it: $(cat "$WORK/api-body")"
inventory_audited() {
	loki_query '{service_name="heartbeat-api"} | event_name="heartbeat.audit" |= "local-dev"' | jq -e \
		'any(.data.result[].values[][1] | fromjson; .entity_type == "environment" and .action == "create" and .actor.type == "config" and (.config_version | startswith("sha256:")))'
}
wait_for 120 "local-dev's creation by the inventory file in Loki" inventory_audited
pass "kind's inventory file is applied: local-dev, PatientZero and its source are managed by the file, created by actor config in Loki; a PATCH of local-dev is 409"

observed_source=$(register local-dev E2eShop)
unwatched_source=$(register e2e-unwatched E2eNobodyReads)
# The API keeps the collector's answer for 15s: a first read made while the
# collector was not reachable says unknown until then. register runs in a
# command substitution; under set -e its fail stops the script.
observation_is() {
	[ "$(api_call GET "/api/v1/telemetry-sources/$1" -H "@$WORK/session-header")" = 200 ] &&
		jq -e --arg status "$2" --argjson collectors "$3" \
			'.observation.status == $status and .observation.collectors == $collectors' "$WORK/api-body" >/dev/null
}
wait_for 60 "the local-dev source observed by oslogs-e2e" observation_is "$observed_source" observed '["oslogs-e2e"]'
observation_is "$unwatched_source" not_observed '[]' ||
	fail "the e2e-unwatched source is not reported unobserved: $(cat "$WORK/api-body")"
# The inventory's info metrics [5.3], as Prometheus scraped them: the file's
# PatientZero managed by config, the API's sources with their observation.
inventory_listed() {
	value_is 'count(max without (instance) (
		heartbeat_api_inventory_application_info{environment="local-dev",application="PatientZero",managed_by="config",status="active"}
		or heartbeat_api_inventory_source_info{environment="local-dev",application_name="E2eShop",managed_by="api",observation="observed"}
		or heartbeat_api_inventory_source_info{environment="e2e-unwatched",application_name="E2eNobodyReads",observation="not_observed"}
	))' 3
}
wait_for 60 "the inventory in the API's info metrics, scraped" inventory_listed
svc_get api:8081 /metrics | grep -E '^heartbeat_api_inventory_[a-z_]+_info[{]' | sed 's/^/      /'
pass "the inventory's info metrics list PatientZero (config) and the API's sources with their observation"

# Deep links [4.9]: the API renders the templates of the integrations.yaml
# the collector runs from, mounted from the same ConfigMap. E2eShop's
# logs-explore link opens Explore on the Loki selector that finds the 3
# fixture rows extracted above; its dashboards and a target's exist in
# Grafana. The links' base URL is the ConfigMap's dashboards.base_url.
base_url=$(k -n "$NAMESPACE" get configmap heartbeat-integrations -o jsonpath='{.data.integrations\.yaml}' |
	awk '/^dashboards:/ { d = 1; next } d && /^  base_url:/ { print $2; exit }')
[ -n "$base_url" ] || fail "the integrations ConfigMap has no dashboards.base_url"
[ "$(api_call GET "/api/v1/telemetry-sources/$observed_source" -H "@$WORK/session-header")" = 200 ] ||
	fail "reading the E2eShop source returned $(cat "$WORK/api-body")"
shop_app=$(jq -r .application_id "$WORK/api-body")
local_dev=$(jq -r .environment_id "$WORK/api-body")
code=$(api_call GET "/api/v1/applications/$shop_app/links" -H "@$WORK/session-header")
[ "$code" = 200 ] || fail "GET /api/v1/applications/{id}/links returned $code: $(cat "$WORK/api-body")"
cp "$WORK/api-body" "$WORK/app-links.json"
explore=$(jq -r '.items[] | select(.name == "logs-explore" and .application_name == "E2eShop") | .url' "$WORK/app-links.json")
case "$explore" in
"$base_url/explore?"*) ;;
*) fail "E2eShop has no logs-explore link on $base_url: $(cat "$WORK/app-links.json")" ;;
esac
explore_expr=$(printf '%s\n' "${explore#*left=}" | sed 's/&.*//' | urldecode | jq -r '.queries[0].expr')
[ "$explore_expr" = '{service_name="E2eShop", deployment_environment_name="local-dev"}' ] ||
	fail "the logs-explore link searches $explore_expr"
loki_count_is "$explore_expr" 3 || fail "the logs-explore query finds $(loki_count "$explore_expr") lines in Loki, want the 3 fixture rows"
code=$(api_post /api/v1/database-targets "$(jq -nc --arg env "$local_dev" '{environment_id: $env, name: "e2e-links", host: "e2e-links.db.internal", credential_ref: "env/e2e-links"}')")
[ "$code" = 201 ] || fail "creating the e2e-links target returned $code: $(cat "$WORK/api-body")"
code=$(api_call GET "/api/v1/database-targets/$(jq -r .id "$WORK/api-body")/links" -H "@$WORK/session-header")
[ "$code" = 200 ] || fail "GET /api/v1/database-targets/{id}/links returned $code: $(cat "$WORK/api-body")"
cp "$WORK/api-body" "$WORK/target-links.json"
jq -e --arg url "$base_url/d/sqlserver-overview?orgId=1&var-environment=local-dev&var-target=e2e-links&from=now-1h&to=now" \
	'[.items[] | select(.name == "sqlserver-target") | .url] == [$url]' "$WORK/target-links.json" >/dev/null ||
	fail "the e2e-links target's links: $(cat "$WORK/target-links.json")"
[ "$(api_call GET /api/v1/dashboards -H "@$WORK/session-header")" = 200 ] || fail "GET /api/v1/dashboards returned $(cat "$WORK/api-body")"
for uid in $(jq -r '.items[].url' "$WORK/app-links.json" "$WORK/target-links.json" "$WORK/api-body" | sed -n 's|.*/d/\([^?/]*\).*|\1|p' | sort -u); do
	svc_get grafana:3000 "/api/dashboards/uid/$uid" | jq -e --arg uid "$uid" '.dashboard.uid == $uid' >/dev/null ||
		fail "a link opens dashboard $uid, which Grafana does not have"
done
pass "the API renders deep links from the integrations ConfigMap: E2eShop's logs-explore finds its 3 rows in Loki; its, the e2e-links target's and the listed dashboards exist in Grafana"

code=$(api_call DELETE /api/v1/session -H "@$WORK/session-header" -H "Origin: $site")
[ "$code" = 204 ] || fail "the logout returned $code"
kill "$port_forward_pid" 2>/dev/null || true
port_forward_pid=
source_audited() {
	loki_query "{service_name=\"heartbeat-api\"} | event_name=\"heartbeat.audit\" |= \"$observed_source\"" | jq -e --arg id "$observed_source" \
		'[.data.result[].values[][1] | fromjson | select(.entity_type == "telemetry_source" and .entity_id == $id) | .action] == ["create"]'
}
wait_for 120 "the source's audit record in Loki" source_audited
pass "an OutSystems source for local-dev is observed by oslogs-e2e through GET /admin/config; one for e2e-unwatched is not; the registration is audited in Loki"

# ----------------------------------------------------------------------------
step "probe assignments approved through the API and by the inventory file are applied by the collector"
# ADR 0014: an operator assigns a probe to a database target, an admin
# approves it, and the API renders it into managedCollectors. The API never
# writes cluster state: `scripts/kind.sh assignments` (make kind-assignments)
# runs the render in its pod and deploys with the file; the collector
# reloads, and the API marks the assignment applied once the collector's GET
# /admin/config runs it as rendered. The target is the e2e SQL Server under
# another name, so its metrics come with target="e2e-assigned". The admin
# is the only user, and holds every role.
#
# ADR 0021: kind's inventory file declares PatientZero's database target,
# patientzero, with three probes the file approves itself; scripts/kind.sh
# points it at this run's SQL Server. The same render holds them, from
# PostgreSQL, and the API refuses to change the target.
kubectl --context "$CONTEXT" -n "$NAMESPACE" port-forward "pod/$(api_pod)" :8081 >"$WORK/api-forward-assignments.log" 2>&1 &
port_forward_pid=$!
wait_for 30 "port-forward to the API" grep -q '^Forwarding from 127.0.0.1:' "$WORK/api-forward-assignments.log"
api_port=$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9]*\) .*/\1/p' "$WORK/api-forward-assignments.log" | head -n 1)
code=$(api_call POST /api/v1/session -D "$WORK/login-headers" -H "Origin: $site" -H 'Content-Type: application/json' --data "@$WORK/login.json")
[ "$code" = 201 ] || fail "the admin's third login returned $code: $(cat "$WORK/api-body")"
(
	umask 077
	sed -n 's/^[Ss]et-[Cc]ookie: \(__Host-heartbeat_session=[^;]*\);.*/Cookie: \1/p' "$WORK/login-headers" >"$WORK/session-header"
)
code=$(api_call GET /api/v1/environments -H "@$WORK/session-header")
[ "$code" = 200 ] || fail "listing environments returned $code"
local_dev=$(jq -r '.items[] | select(.slug == "local-dev") | .id' "$WORK/api-body")
[ -n "$local_dev" ] || fail "no local-dev environment: $(cat "$WORK/api-body")"
# The file's target and its assignments: config-managed, on this run's
# container, approved with no user (migration 0008), before any render.
file_target=$(psql_ "SELECT t.id FROM database_targets t JOIN environments e ON e.id = t.environment_id
	WHERE e.slug = 'local-dev' AND t.name = 'patientzero' AND t.managed_by = 'config' AND t.host = '$SQL_CONTAINER' AND t.status = 'active'")
[ -n "$file_target" ] || fail "no config-managed patientzero target on $SQL_CONTAINER in local-dev: $(psql_ "SELECT name, host, managed_by, status FROM database_targets")"
# file_assignments: each of the file's assignments as <probe> <manager>
# <state> <approved by the file>, sorted.
file_assignments() {
	psql_ "SELECT string_agg(d.name || ' ' || a.managed_by || ' ' || a.state || ' ' || (a.approved_by IS NULL AND a.approved_at IS NOT NULL)::text, ',' ORDER BY d.name)
		FROM probe_assignments a JOIN probe_definitions d ON d.id = a.probe_definition_id WHERE a.database_target_id = '$file_target'"
}
[ "$(file_assignments)" = "blocking config approved true,sessions config approved true,waits config approved true" ] ||
	fail "the file's assignments: $(file_assignments)"
code=$(api_call PATCH "/api/v1/database-targets/$file_target" -H "@$WORK/session-header" -H "Origin: $site" -H 'Content-Type: application/json' --data '{"status": "disabled"}')
[ "$code" = 409 ] || fail "disabling the patientzero target through the API returned $code, want 409: the inventory file manages it: $(cat "$WORK/api-body")"
code=$(api_call DELETE "/api/v1/database-targets/$file_target" -H "@$WORK/session-header" -H "Origin: $site")
[ "$code" = 409 ] || fail "deleting the patientzero target through the API returned $code, want 409: $(cat "$WORK/api-body")"
[ "$(psql_ "SELECT status FROM database_targets WHERE id = '$file_target'")" = active ] || fail "the refused writes changed the patientzero target"
code=$(api_post /api/v1/database-targets "$(jq -nc --arg env "$local_dev" --arg host "$SQL_CONTAINER" \
	'{environment_id: $env, name: "e2e-assigned", host: $host, database_name: "tempdb", credential_ref: "env/sqlserver-dev"}')")
[ "$code" = 201 ] || fail "creating the e2e-assigned target returned $code: $(cat "$WORK/api-body")"
assigned_target=$(jq -r .id "$WORK/api-body")
code=$(api_call GET /api/v1/probe-definitions -H "@$WORK/session-header")
[ "$code" = 200 ] || fail "listing probe definitions returned $code"
sessions_probe=$(jq -r '.items[] | select(.name == "sessions" and .builtin) | .id' "$WORK/api-body")
code=$(api_post /api/v1/probe-assignments "$(jq -nc --arg t "$assigned_target" --arg d "$sessions_probe" \
	'{database_target_id: $t, probe_definition_id: $d, interval_seconds: 20, max_rows: 1000}')")
[ "$code" = 201 ] || fail "creating the assignment returned $code: $(cat "$WORK/api-body")"
jq -e '.state == "draft"' "$WORK/api-body" >/dev/null || fail "a new assignment is not a draft: $(cat "$WORK/api-body")"
assignment=$(jq -r .id "$WORK/api-body")
assigned_collector=$(jq -r .collector_id "$WORK/api-body")
# A draft is not rendered.
code=$(api_call GET /api/v1/probe-assignments/rendered -H "@$WORK/session-header")
[ "$code" = 200 ] && ! grep -q "$assigned_collector" "$WORK/api-body" || fail "the draft is rendered ($code): $(cat "$WORK/api-body")"
code=$(api_call POST "/api/v1/probe-assignments/$assignment/approve" -H "@$WORK/session-header" -H "Origin: $site")
[ "$code" = 200 ] || fail "approving the assignment returned $code: $(cat "$WORK/api-body")"
jq -e '.state == "approved" and .approved_by != null' "$WORK/api-body" >/dev/null || fail "the approval: $(cat "$WORK/api-body")"
code=$(api_call GET /api/v1/probe-assignments/rendered -H "@$WORK/session-header")
[ "$code" = 200 ] && grep -q "id: $assigned_collector" "$WORK/api-body" || fail "the approval is not rendered ($code): $(cat "$WORK/api-body")"
cp "$WORK/api-body" "$WORK/rendered-api.yaml"
# The file's target renders as a collector of its own, api-<target id>-30s,
# with its three probes.
file_collector=api-$file_target-30s
grep -q "id: $file_collector" "$WORK/rendered-api.yaml" || fail "the file's assignments are not rendered as $file_collector: $(cat "$WORK/rendered-api.yaml")"
EXTRA_VALUES="$WORK/e2e-oslogs.yaml $base_values" scripts/kind.sh assignments >/dev/null
# What kind deployed is what the API serves.
cmp -s "$WORK/rendered-api.yaml" ".tmp/kind/$CLUSTER/managed-collectors.yaml" ||
	fail "the file kind.sh deployed differs from GET /api/v1/probe-assignments/rendered"
wait_for 180 "sessions of target e2e-assigned" has_value "sum(heartbeat_sqlserver_sessions{target=\"e2e-assigned\"})"
wait_for 120 "sessions of target patientzero" has_value "sum(heartbeat_sqlserver_sessions{target=\"patientzero\"})"
target_up patientzero 1 || fail "the patientzero target is not up: $(prom 'heartbeat_collector_target_up{target="patientzero"}')"
assignment_is() {
	[ "$(api_call GET "/api/v1/probe-assignments/$assignment" -H "@$WORK/session-header")" = 200 ] &&
		jq -e --arg state "$1" '.state == $state' "$WORK/api-body" >/dev/null
}
# The applied check runs every 30s, on the collector's answer kept 15s.
wait_for 120 "the assignment applied" assignment_is applied
file_applied() { [ "$(file_assignments)" = "blocking config applied true,sessions config applied true,waits config applied true" ]; }
wait_for 120 "the file's assignments applied" file_applied
code=$(api_call DELETE /api/v1/session -H "@$WORK/session-header" -H "Origin: $site")
[ "$code" = 204 ] || fail "the logout returned $code"
kill "$port_forward_pid" 2>/dev/null || true
port_forward_pid=
assignment_audited() {
	loki_query "{service_name=\"heartbeat-api\"} | event_name=\"heartbeat.audit\" |= \"$assignment\"" | jq -e --arg id "$assignment" \
		'[.data.result[].values[][1] | fromjson | select(.entity_type == "probe_assignment" and .entity_id == $id) | .action] | sort == ["apply", "approve", "create"]'
}
wait_for 120 "the assignment's create, approve and apply records in Loki" assignment_audited
file_target_audited() {
	loki_query "{service_name=\"heartbeat-api\"} | event_name=\"heartbeat.audit\" |= \"$file_target\"" | jq -e --arg id "$file_target" \
		'any(.data.result[].values[][1] | fromjson; .entity_type == "database_target" and .entity_id == $id and .action == "create" and .actor.type == "config")'
}
wait_for 120 "the patientzero target's creation by the inventory file in Loki" file_target_audited
# Later steps deploy without it: the state file is what keeps it.
rm -f ".tmp/kind/$CLUSTER/managed-collectors.yaml"
pass "assignment $assignment (sessions on e2e-assigned, every 20s) went draft -> approved -> applied; $assigned_collector runs it and its sessions reach Prometheus; create, approve and apply audited in Loki"
pass "the inventory file's patientzero target and its 3 assignments are config-managed and approved by the file; a PATCH and a DELETE of the target are 409; $file_collector runs them from the same render, its sessions reach Prometheus, all 3 applied; created by actor config in Loki"

# ----------------------------------------------------------------------------
step "unauthenticated endpoints expose no diagnostics"
readyz=$(pod_get /readyz)
[ "$(printf '%s' "$readyz" | jq -c keys)" = '["status"]' ] || fail "/readyz serves more than its status: $readyz"
admin_forward
for header in "" "Authorization: Bearer wrong-token"; do
	code=$(admin_status /admin/config "$header")
	[ "$code" = 401 ] || fail "GET /admin/config with header '$header' returned $code, want 401"
done
admin_config | jq -e '.readiness.status == "ready" and .version != ""' >/dev/null ||
	fail "GET /admin/config with the admin token did not return diagnostics"
pass "/readyz is status only ($readyz); /admin/config answers 401 without the token"

step "NetworkPolicies admit only the components that use each service"
# The flows the policies allow, beyond those the steps above already prove
# (db-collector -> OTel Collector -> Loki, Prometheus -> the collector and
# Alertmanager, Grafana -> Prometheus): Prometheus scrapes every target, and
# Grafana's Loki datasource answers.
collector_url=http://db-collector-headless:8082/healthz
k -n "$NAMESPACE" exec deployment/prometheus -c prometheus-server -- wget -q -T 5 -O /dev/null "$collector_url" ||
	fail "Prometheus cannot reach the collector's port 8082"
for job in db-collector api otel-collector loki alertmanager tempo; do
	wait_for 60 "Prometheus scrapes $job" value_is "up{job=\"$job\"}" 1
done
svc_get grafana:3000 /api/datasources/proxy/uid/loki/loki/api/v1/labels | jq -e '.status == "success"' >/dev/null ||
	fail "Grafana's Loki datasource cannot reach Loki"
svc_get grafana:3000 /api/datasources/proxy/uid/tempo/ready >/dev/null ||
	fail "Grafana's Tempo datasource cannot reach Tempo"
pass "Prometheus scrapes the collector, the API, the OTel Collector, Loki, Tempo and Alertmanager; Grafana queries Loki and Tempo"
# Operators reach Loki through kubectl port-forward, which NetworkPolicy does
# not filter. kubectl runs directly, as in admin_forward.
kubectl --context "$CONTEXT" -n "$NAMESPACE" port-forward svc/loki :3100 >"$WORK/loki-forward.log" 2>&1 &
port_forward_pid=$!
wait_for 30 "port-forward to Loki" grep -q '^Forwarding from 127.0.0.1:' "$WORK/loki-forward.log"
loki_port=$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9]*\) .*/\1/p' "$WORK/loki-forward.log" | head -n 1)
loki_ready=$(curl -sS --max-time 5 "http://127.0.0.1:$loki_port/ready" || true)
kill "$port_forward_pid" 2>/dev/null || true
wait "$port_forward_pid" 2>/dev/null || true
port_forward_pid=
[ "$loki_ready" = ready ] || fail "Loki through kubectl port-forward answered '$loki_ready', want ready"
pass "Loki answers through kubectl port-forward"
# The same image and tool as Prometheus, from pods the policies do not list:
# one in this namespace without the components' labels, and one in another
# namespace with Grafana's labels (a podSelector peer only matches pods in
# the policy's own namespace). kindnet drops the connection, so only
# timeout's kill ends it (wget's own -T is longer): exit 124 or 143. Any
# other failure (DNS, refused) is not the policy. Grafana, which has no
# policy, is the control: it shows that the pod's DNS and network work. sh
# stays PID 1: busybox timeout execs wget in its own process, and PID 1
# ignores the SIGTERM it would send. The pod in this namespace also tries
# Prometheus's and Alertmanager's NodePorts on the node, which kind.yaml opens
# to every source outside the pod subnet: the policy must still see the pod.
# Every URL is tried at once, so the pod waits one timeout, not one per URL.
# PostgreSQL and Valkey do not speak HTTP: reached, wget fails at once
# (exit 1) instead of timing out.
blocked="db-collector-headless:8082/healthz api:8081/healthz postgresql:5432/ valkey:6379/ loki:3100/ready tempo:3200/ready prometheus:9090/-/ready alertmanager:9093/-/ready otel-collector:4318/v1/logs"
probe_script='for url in "$@"; do (timeout 10 wget -q -T 30 -O /dev/null "$url"; echo "$url $?") & done; wait'
node_ip=$(k get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
k create namespace "$other_ns" --dry-run=client -o yaml | k apply -f - >/dev/null
# A pod is rejected until the namespace's default ServiceAccount exists.
wait_for 60 "default ServiceAccount in $other_ns" k -n "$other_ns" get serviceaccount default
k -n "$NAMESPACE" delete pod np-probe --ignore-not-found --wait=true >/dev/null
k -n "$other_ns" delete pod np-probe --ignore-not-found --wait=true >/dev/null
# shellcheck disable=SC2086 # $blocked is a list of words
k -n "$NAMESPACE" run np-probe --image="$prom_image" --restart=Never --labels=app.kubernetes.io/name=np-probe \
	--command -- sh -c "$probe_script" sh $(printf 'http://%s ' $blocked) "http://$node_ip:30090/-/ready" "http://$node_ip:30093/-/ready" \
	http://grafana:3000/api/health >/dev/null
# shellcheck disable=SC2086
k -n "$other_ns" run np-probe --image="$prom_image" --restart=Never \
	--labels=app.kubernetes.io/name=grafana,app.kubernetes.io/instance=heartbeat \
	--command -- sh -c "$probe_script" sh $(printf '%s\n' $blocked | sed "s|^\([^:]*\):|http://\1.$NAMESPACE:|") \
	"http://grafana.$NAMESPACE:3000/api/health" >/dev/null
wait_for 180 "probe pod finished" pod_done np-probe
wait_for 180 "probe pod in $other_ns finished" pod_done np-probe "$other_ns"
results=$({
	k -n "$NAMESPACE" logs np-probe
	k -n "$other_ns" logs np-probe
} 2>&1 | grep '^http' | sort || true)
k delete namespace "$other_ns" --wait=false >/dev/null
k -n "$NAMESPACE" delete pod np-probe --wait=false >/dev/null
printf '%s\n' "$results" | sed 's/^/      /'
[ "$(printf '%s\n' "$results" | grep -c .)" = 22 ] || fail "expected 22 probe results"
printf '%s\n' "$results" | while read -r url code; do
	case "$url:$code" in
	*grafana*:0) ;;
	*grafana*) fail "the probe pod cannot reach Grafana ($url exit $code), so its other results prove nothing" ;;
	*:124 | *:143) ;;
	*) fail "$url from a pod the policies do not list: exit $code, not a connection timeout" ;;
	esac
done
pass "unlisted pods time out on the collector, the API, PostgreSQL, Valkey, Loki, Tempo, Prometheus, Alertmanager and the OTel Collector, NodePorts included"
# The collector's egress (kind.yaml limits it), from an ephemeral container in
# db-collector-0, which shares the pod's network and so its policy: it
# resolves names and reaches the OTel Collector, which answers its GET with a
# 405, and times out on every other pod, Grafana included, whose own ingress
# is open. A stand-in pod with the collector's labels would also join the
# collector's Services, and Prometheus would scrape it. The collector image
# has no shell, hence the Prometheus image. Reaching SQL Server is what the
# collector's targets being up already shows. An ephemeral container stays in
# the pod until the pod is replaced, so each run names its own.
egress_script='for url in "$@"; do (out=$(timeout 10 wget -q -T 30 -O /dev/null "$url" 2>&1); echo "$url $? $out") & done; wait'
egress_probe=egress-probe-$(date +%s)
k -n "$NAMESPACE" debug db-collector-0 --profile=restricted --image="$prom_image" --container="$egress_probe" \
	-- sh -c "$egress_script" sh http://otel-collector:4318/v1/logs http://grafana:3000/api/health \
	http://loki:3100/ready http://prometheus:9090/-/ready >/dev/null
wait_for 180 "egress probe finished" ephemeral_done db-collector-0 "$egress_probe"
results=$(k -n "$NAMESPACE" logs db-collector-0 -c "$egress_probe" 2>&1 | grep '^http' | sort || true)
printf '%s\n' "$results" | sed 's/^/      /'
[ "$(printf '%s\n' "$results" | grep -c .)" = 4 ] || fail "expected 4 egress probe results"
printf '%s\n' "$results" | while read -r url code rest; do
	case "$url" in
	*otel-collector*)
		case "$rest" in
		*405*) ;;
		*) fail "the collector cannot reach the OTel Collector: $url exit $code $rest" ;;
		esac
		;;
	*)
		case "$code" in
		124 | 143) ;;
		*) fail "$url from the collector: exit $code $rest, not a connection timeout" ;;
		esac
		;;
	esac
done
pass "the collector resolves names and reaches the OTel Collector, and times out on Grafana, Loki and Prometheus"

# ----------------------------------------------------------------------------
step "a valid ConfigMap update is hot-reloaded without a restart"
uid_before=$(collector_pod_uid)
version_before=$(config_version)
sed -e 's/scrape_interval: 15s/scrape_interval: 20s/' "$WORK/e2e.yaml" >"$WORK/e2e-reload.yaml"
EXTRA_VALUES="$WORK/e2e-reload.yaml $base_values" scripts/kind.sh deploy >/dev/null
# kubelet propagates ConfigMap updates within about a minute.
wait_for 180 "new config version active" version_changed_from "$version_before"
[ "$(collector_pod_uid)" = "$uid_before" ] || fail "collector pod was replaced by a config change"
version_new=$(config_version)
# One info series: the new version replaces the old one.
wait_for 60 "new version in Prometheus" value_is "heartbeat_collector_config_info{version=\"$version_new\"}" 1
value_is 'count(heartbeat_collector_config_info{job="db-collector"})' 1 ||
	fail "Prometheus has more than one heartbeat_collector_config_info series after a reload"
pass "config $version_before -> $version_new, same pod, and Prometheus shows only the new version"

step "an invalid ConfigMap update is rejected while collection continues, and monitoring shows it"
version_good=$(config_version)
restarts_before=$(collector_restarts)
rejected_before=$(rejected_reloads)
# A misspelled probe name (usability finding F4): Helm's values schema lets it
# through, the collector rejects it. Written straight to the ConfigMap, which
# is restored the same way.
k -n "$NAMESPACE" get configmap heartbeat-integrations -o json >"$WORK/integrations-good.json"
jq -e '.data["integrations.yaml"] | contains("- name: waits")' "$WORK/integrations-good.json" >/dev/null ||
	fail "no waits probe to misspell in the ConfigMap"
jq '.data["integrations.yaml"] |= sub("- name: waits"; "- name: wiats")' "$WORK/integrations-good.json" >"$WORK/integrations-typo.json"
k replace -f "$WORK/integrations-typo.json" >/dev/null
wait_for 180 "reload error reported" reload_error_is_set
admin_config | jq -r '"      last_reload_err: " + .last_reload_err'
admin_config | jq -e '.last_reload_err | contains("unknown SQL Server probe \"wiats\"; probes: ")' >/dev/null ||
	fail "last_reload_err does not name the misspelled probe and the valid ones"
[ "$(config_version)" = "$version_good" ] || fail "invalid config replaced the active version"
wait_for 90 "collection still fresh" target_fresh sqlserver-dev
[ "$(collector_restarts)" = "$restarts_before" ] || fail "collector restarted on invalid config"
pass "invalid config rejected, version $version_good kept, still collecting"
wait_for 60 "failed reload in Prometheus" reload_successful_is 0
# The counter is incremented just after the gauge's snapshot is stored, so a
# scrape can land between the two.
wait_for 60 "rejected reload counted" rejected_reloads_above "${rejected_before:-0}"
rejected_after=$(rejected_reloads)
value_is "heartbeat_collector_config_info{version=\"$version_good\"}" 1 ||
	fail "Prometheus does not show the kept version $version_good"
wait_for 60 "HeartbeatCollectorConfigReloadFailed pending or firing" reload_alert_active
pass "the rejection shows in Prometheus: last_reload_successful 0, $rejected_after rejected, version $version_good, alert active"
# Restore as Helm's field manager: Helm 4 upgrades with server-side apply and
# would otherwise conflict with the out-of-band edit.
jq 'del(.metadata.resourceVersion, .metadata.uid, .metadata.creationTimestamp)' "$WORK/integrations-good.json" |
	k apply --server-side --field-manager=helm --force-conflicts -f - >/dev/null
wait_for 180 "reload error cleared" reload_error_is_clear
wait_for 60 "successful reload in Prometheus" reload_successful_is 1
wait_for 60 "HeartbeatCollectorConfigReloadFailed resolved" reload_alert_gone
pass "the restored config reloads: last_reload_successful 1, alert gone"
admin_unforward

# ----------------------------------------------------------------------------
step "an unavailable SQL Server shows as failed without restarting the collector"
restarts_before=$(collector_restarts)
docker stop "$SQL_CONTAINER" >/dev/null
wait_for 180 "target reported down" target_up sqlserver-dev 0
pod_get /healthz >/dev/null ||
	fail "collector liveness failed during a database outage"
docker start "$SQL_CONTAINER" >/dev/null
wait_for 300 "target recovered" target_up sqlserver-dev 1
[ "$(collector_restarts)" = "$restarts_before" ] || fail "collector restarted during a database outage"
pass "outage reported, recovered, restartCount unchanged ($restarts_before)"

# ----------------------------------------------------------------------------
step "a changed credential in the Secret is used without a restart"
# The credentials Secret is mounted as files the collector reads on every
# connection. A wrong password must take the target down and the right one
# bring it back, through the kubelet's volume update alone. The recovery wait
# covers the volume update (up to about two minutes) plus the failing target's
# retry backoff, which doubles from the scrape interval with each failed cycle.
credential_secret() {
	# The credential reaches kubectl in a 0600 file, never on its command line.
	(umask 077 && printf 'HEARTBEAT_CREDENTIAL_ENV_SQLSERVER_DEV=%s\n' "$1" >"$WORK/credential.env")
	k -n "$NAMESPACE" create secret generic heartbeat-sqlserver-dev-credentials \
		--from-env-file="$WORK/credential.env" --dry-run=client -o yaml | k apply -f - >/dev/null
	rm -f "$WORK/credential.env"
}
restarts_before=$(collector_restarts)
uid_before=$(collector_pod_uid)
credential=$(sed -n 's/^HEARTBEAT_CREDENTIAL_ENV_SQLSERVER_DEV=//p' "$SQLSERVER_ENV_FILE")
credential_secret "${credential%%:*}:Wrong-$(openssl rand -hex 16)"
wait_for 180 "wrong password takes the target down" target_up sqlserver-dev 0
credential_secret "$credential"
wait_for 420 "right password brings it back" target_up sqlserver-dev 1
[ "$(collector_pod_uid)" = "$uid_before" ] || fail "collector pod was replaced by a credential change"
[ "$(collector_restarts)" = "$restarts_before" ] || fail "collector restarted for a credential change"
pass "both Secret changes used by the running collector, same pod, restartCount unchanged ($restarts_before)"

# ----------------------------------------------------------------------------
step "a collector update drains the old pod before the new one polls"
# Samples, every second: running collector containers in the cluster, and
# collector sessions in SQL Server's master database, the sqlserver-dev
# target's (the outsystems_logs step's pool, in its own database, may still
# hold idle connections). One collector holds at most 2 connections per
# target (defaultMaxOpenConns), so more than 2 means two collectors
# overlapped. Pod names and NATed addresses cannot tell old and new apart.
sample_concurrency() {
	max_pods=0
	max_sql=0
	while [ ! -f "$WORK/stop-sampling" ]; do
		pods=$(k -n "$NAMESPACE" get pods -l app.kubernetes.io/name=db-collector -o json |
			jq '[.items[] | select(.status.containerStatuses[0].state.running != null)] | length')
		sql=$(SQLCMDPASSWORD=$password docker exec -e SQLCMDPASSWORD "$SQL_CONTAINER" /opt/mssql-tools18/bin/sqlcmd \
			-S localhost -U sa -C -h -1 -W -Q "SET NOCOUNT ON; SELECT COUNT(*) FROM sys.dm_exec_sessions WHERE program_name = 'HeartbeatDBCollector' AND database_id = DB_ID(N'master')" 2>/dev/null | tr -dc '0-9')
		[ "${pods:-0}" -gt "$max_pods" ] && max_pods=$pods
		[ "${sql:-0}" -gt "$max_sql" ] && max_sql=$sql
		printf '%s %s\n' "$max_pods" "$max_sql" >"$WORK/concurrency"
		sleep 1
	done
}
sample_concurrency &
sampler=$!
last_before=$(last_success sqlserver-dev)
uid_before=$(collector_pod_uid)
start=$(date +%s)
k -n "$NAMESPACE" rollout restart statefulset/db-collector >/dev/null
wait_for 180 "replacement pod ready" replacement_ready "$uid_before"
rollout_s=$(($(date +%s) - start))
wait_for 120 "collection resumed" success_after sqlserver-dev "$last_before"
first_after=$(last_success sqlserver-dev)
touch "$WORK/stop-sampling"
wait "$sampler" || true
read -r max_pods max_sql <"$WORK/concurrency"
gap=$(awk -v a="$first_after" -v b="$last_before" 'BEGIN { printf "%.0f", a - b }')
printf '      rollout %ss, collection gap %ss, max running collectors %s, max collector sessions on SQL Server %s\n' \
	"$rollout_s" "$gap" "$max_pods" "$max_sql"
[ "$max_pods" -le 1 ] || fail "$max_pods collector pods ran at once"
[ "$max_sql" -le 2 ] || fail "$max_sql collector sessions on SQL Server: two collectors overlapped"
pass "singleton update: never more than one collector"

# ----------------------------------------------------------------------------
step "a deploy replaces a collector pod that never became Ready"
# A first install with a wrong image, then the corrected deploy. With the
# OrderedReady policy the StatefulSet would wait for the broken pod forever
# (kubernetes/kubernetes#67250) and the deploy would time out; with Parallel
# the controller replaces it.
helm --kube-context "$CONTEXT" -n "$NAMESPACE" upgrade heartbeat infra/helm/heartbeat \
	--reuse-values --set dbCollector.image.tag=missing >/dev/null
wait_for 120 "collector pod stuck on a missing image" collector_waiting_on ErrImageNeverPull
start=$(date +%s)
EXTRA_VALUES="$WORK/e2e-reload.yaml $base_values" scripts/kind.sh deploy >/dev/null
case "$(collector_image)" in
*"heartbeat/db-collector:$DB_COLLECTOR_TAG") ;;
*) fail "db-collector runs $(collector_image) after the corrected deploy, expected tag $DB_COLLECTOR_TAG" ;;
esac
pass "the corrected deploy replaced the stuck pod in $(($(date +%s) - start))s, with no manual pod delete"

step "a StatefulSet created with OrderedReady moves to Parallel without a restart"
# Chart 0.1.0 created the StatefulSet with OrderedReady, which cannot change
# in place. Recreate it that way, as Helm rendered it, and deploy: kind.sh
# deletes it once with --cascade=orphan, the documented upgrade step, and the
# new StatefulSet adopts the running pod. The earlier rollout restarts left a
# restartedAt annotation that Helm does not render; the recreated template
# drops it, so its rollout runs first and the deploy changes no template.
k -n "$NAMESPACE" get statefulset db-collector -o json |
	jq '.spec.podManagementPolicy = "OrderedReady" | del(.status) | .metadata |= {name, namespace, labels, annotations} |
		del(.spec.template.metadata.annotations["kubectl.kubernetes.io/restartedAt"])' >"$WORK/statefulset-ordered.json"
k -n "$NAMESPACE" delete statefulset db-collector --cascade=orphan --wait >/dev/null
k -n "$NAMESPACE" create -f "$WORK/statefulset-ordered.json" >/dev/null
[ "$(collector_policy)" = OrderedReady ] || fail "could not recreate the StatefulSet with OrderedReady"
wait_for 180 "collector on the OrderedReady StatefulSet's revision" collector_on_update_revision
uid_before=$(collector_pod_uid)
k -n "$NAMESPACE" get statefulset db-collector -o json | jq -S .spec.template >"$WORK/template-before.json"
EXTRA_VALUES="$WORK/e2e-reload.yaml $base_values" scripts/kind.sh deploy >/dev/null
[ "$(collector_policy)" = Parallel ] || fail "db-collector StatefulSet is $(collector_policy) after the deploy, not Parallel"
if [ "$(collector_pod_uid)" != "$uid_before" ]; then
	k -n "$NAMESPACE" get statefulset db-collector -o json | jq -S .spec.template >"$WORK/template-after.json"
	diff "$WORK/template-before.json" "$WORK/template-after.json" || true
	fail "the collector pod was replaced by the policy change (pod template diff above, before -> after)"
fi
pass "recreated as Parallel; the collector pod kept running"

# ----------------------------------------------------------------------------
step "stateful restarts keep persistent data"
lowest=$(prom_value 'prometheus_tsdb_lowest_timestamp_seconds')
silence_id=$(k -n "$NAMESPACE" exec alertmanager-0 -c alertmanager -- amtool silence add alertname=E2eProbe \
	--alertmanager.url=http://localhost:9093 --duration=2h --author=kind-e2e --comment="persistence check")
k -n "$NAMESPACE" delete pod -l app.kubernetes.io/name=prometheus --wait=true >/dev/null
k -n "$NAMESPACE" delete pod alertmanager-0 --wait=true >/dev/null
k -n "$NAMESPACE" rollout status deployment/prometheus --timeout 3m >/dev/null
k -n "$NAMESPACE" rollout status statefulset/alertmanager --timeout 3m >/dev/null
wait_for 120 "Prometheus answering" prometheus_answers
lowest_after=$(prom_value 'prometheus_tsdb_lowest_timestamp_seconds')
gt "$lowest_after" "$lowest" &&
	fail "Prometheus lost its data on restart (lowest timestamp $lowest -> $lowest_after)"
svc_get alertmanager:9093 /api/v2/silences | jq -e --arg id "$silence_id" 'any(.[]; .id == $id)' >/dev/null ||
	fail "Alertmanager lost its silence on restart"
pass "Prometheus TSDB and Alertmanager silences survived pod replacement"
# PostgreSQL's data is on its claim: a new pod finds the schema, and the API,
# unready meanwhile, is ready again without a restart of its own.
pod=$(api_pod)
restarts_before=$(k -n "$NAMESPACE" get pod "$pod" -o jsonpath='{.status.containerStatuses[0].restartCount}')
uid_before=$(k -n "$NAMESPACE" get pod postgresql-0 -o jsonpath='{.metadata.uid}')
k -n "$NAMESPACE" delete pod postgresql-0 --wait=true >/dev/null
k -n "$NAMESPACE" rollout status statefulset/postgresql --timeout 3m >/dev/null
[ "$(k -n "$NAMESPACE" get pod postgresql-0 -o jsonpath='{.metadata.uid}')" != "$uid_before" ] || fail "postgresql-0 was not replaced"
wait_for 60 "schema still at version $newest" schema_version_is "$newest"
wait_for 60 "the API ready again" api_ready_is True
[ "$(k -n "$NAMESPACE" get pod "$pod" -o jsonpath='{.status.containerStatuses[0].restartCount}')" = "$restarts_before" ] ||
	fail "the API restarted with PostgreSQL"
pass "PostgreSQL's schema survived pod replacement; the API reconnected without a restart"

# ----------------------------------------------------------------------------
step "a Collector restart during a log backlog loses no rows"
# The db-collector moves its watermark once the OTel Collector has queued a
# batch, so the request in that queue exists nowhere else. With Loki stopped,
# two collectors extract a backlog each, of 6,000 rows spanning 45 minutes
# and three hours; the Collector is restarted, and once Loki is back it must
# hold every row of both, each once. Loki rejects for good a row more than an
# hour behind the newest of its stream, so the three-hour backlog fails if a
# restart sends a request out of order, as a queue of more than one request
# does (values.yaml, otlp_http/logs). With the chart's one-request queue the
# db-collector waits for the Collector instead: its targets stay up and
# heartbeat_outsystems_export_blocked is 1.
backlog_rows=6000
backlogs="45m:2700 3h:10800"
cp "$WORK/e2e.yaml" "$WORK/e2e-backlog.yaml"
for backlog in $backlogs; do
	name=${backlog%%:*} span_s=${backlog#*:}
	cat >>"$WORK/e2e-backlog.yaml" <<EOF
    - id: oslogs-backlog-$name
      kind: outsystems_logs
      enabled: true
      config:
        environment: local-dev
        scrape_interval: 10s
        lag: 10s
        initial_lookback: $((span_s / 60 + 10))m
        probes:
          - name: error
        targets:
          - name: backlog-db-$name
            host: $SQL_CONTAINER
            port: 1433
            database_name: heartbeat_oslog_backlog_$name
            credential_ref: env/sqlserver-dev
EOF
done
# Deployed before Loki stops: a Helm upgrade would start it again. Until the
# databases below exist, the targets only fail to connect.
EXTRA_VALUES="$WORK/e2e-backlog.yaml $base_values" scripts/kind.sh deploy >/dev/null
k -n "$NAMESPACE" scale statefulset/loki --replicas=0 >/dev/null
k -n "$NAMESPACE" wait --for=delete pod/loki-0 --timeout=3m >/dev/null
for backlog in $backlogs; do
	name=${backlog%%:*} span_s=${backlog#*:}
	# Long text in every row, as error rows with stacks have: about 3 KB, so
	# each backlog is several 4 MiB requests, and both together fit in the
	# 64 MiB queue earlier charts had (to show what that queue loses).
	fixture_output=$({
		printf 'CREATE DATABASE heartbeat_oslog_backlog_%s;\nGO\nUSE heartbeat_oslog_backlog_%s;\nGO\n' "$name" "$name"
		cat services/db-collector/internal/collectors/outsystems/testdata/oslog/error.sql
		printf '\nGO\n'
		cat <<EOF
WITH numbers AS (
    SELECT TOP ($backlog_rows) ROW_NUMBER() OVER (ORDER BY (SELECT NULL)) AS i
    FROM sys.all_objects a CROSS JOIN sys.all_objects b
)
INSERT INTO dbo.oslog_Error_3 (Id, Instant, Session_Id, User_Id, Espace_Id, Tenant_Id, Message, Stack, Module_Name, Server, EnvironmentInformation, Espace_Name, Request_Key)
SELECT CONCAT('backlog-', i),
    DATEADD(MILLISECOND, -CAST(60000 + CAST($span_s AS bigint) * 1000 * ($backlog_rows - i) / $backlog_rows AS int), GETUTCDATE()),
    '', 0, 1, 0, CONCAT('e2e backlog row ', i, ' ', REPLICATE('m', 1000)), REPLICATE('s', 1000),
    'E2eBacklog$name', 'e2e', REPLICATE('e', 1000), 'E2eBacklog$name', CONCAT('backlog-', i)
FROM numbers;
GO
EOF
		# Last: a poll before this cannot read the database at all. One between the
		# grant and the rows would find none, move the watermark past them, and
		# the backlog would never be read.
		printf 'CREATE USER [%s] FOR LOGIN [%s];\nGRANT SELECT ON dbo.oslog_Error TO [%s];\nGRANT SELECT ON dbo.oslog_Error_Previous TO [%s];\nGO\n' "$oslog_login" "$oslog_login" "$oslog_login" "$oslog_login"
	} | SQLCMDPASSWORD=$password docker exec -i -e SQLCMDPASSWORD "$SQL_CONTAINER" \
		/opt/mssql-tools18/bin/sqlcmd -S localhost -U sa -C -b 2>&1) ||
		fail "loading the $name backlog fixture failed: $fixture_output"
done
backlog_read() { prom_value "sum(heartbeat_outsystems_extract_rows_total{collector=\"oslogs-backlog-$1\"})"; }
backlog_blocked() { value_is "max(heartbeat_outsystems_export_blocked{collector=\"oslogs-backlog-$1\"})" 1; }
# Settled when the Collector took every row (a queue that holds them all) or
# when the db-collector waits for it on both backlogs (the one-request queue).
backlog_settled() {
	all=true blocked=true
	for backlog in $backlogs; do
		name=${backlog%%:*}
		[ "$(backlog_read "$name")" = "$backlog_rows" ] || all=false
		backlog_blocked "$name" || blocked=false
	done
	$all || $blocked
}
wait_for 300 "the db-collector delivered every row or waits for the Collector" backlog_settled
wait_for 60 "a request in the Collector's queue" otel_queue_holds
for backlog in $backlogs; do
	name=${backlog%%:*}
	read_before=$(backlog_read "$name")
	if [ "$read_before" != "$backlog_rows" ]; then
		backlog_blocked "$name" || fail "the $name backlog stopped at $read_before rows without waiting for the Collector"
		target_up "backlog-db-$name" 1 || fail "the backlog-db-$name target is down while it waits for the Collector"
	fi
	printf '      %s backlog: %s rows delivered with Loki down; export blocked: %s; target up: %s\n' "$name" "$read_before" \
		"$(prom_value "max(heartbeat_outsystems_export_blocked{collector=\"oslogs-backlog-$name\"})")" \
		"$(prom_value "heartbeat_collector_target_up{target=\"backlog-db-$name\"}")"
done
queued_before=$(otel_queue_size)
uid_before=$(otel_pod_uid)
k -n "$NAMESPACE" delete pod otel-collector-0 --wait=true >/dev/null
wait_for 180 "Collector replacement ready" otel_replacement_ready "$uid_before"
queued_after=$(otel_queue_size)
printf '      Collector queue: %s before the restart, %s after\n' "$queued_before" "${queued_after:-?}"
k -n "$NAMESPACE" scale statefulset/loki --replicas=1 >/dev/null
k -n "$NAMESPACE" rollout status statefulset/loki --timeout 5m >/dev/null
# Distinct rows by their Request_Key, and lines in total. Delivery is at
# least once (ADR 0008), so a row can be in Loki twice: a request the
# stopped Collector kept on its disk queue though the db-collector never
# saw it accepted is sent by the replacement and again by the db-collector's
# retry. Loki drops many of those copies but not all (run 41847786348:
# a request of 1,266 rows sent twice, 73 of its lines kept twice). The check
# is no row lost, and no line twice beyond what the Collector sent twice.
# The total keeps only service_name, so it is one series rather than one per
# row, cheap to poll.
distinct_query() { echo "count(sum by (Request_Key) (count_over_time({service_name=\"E2eBacklog$1\"}[$(($2 / 60 + 30))m])))"; }
total_query() { echo "sum(count_over_time({service_name=\"E2eBacklog$1\"} | keep service_name [$(($2 / 60 + 30))m]))"; }
# The delivery as key=value words: per backlog the rows the Collector
# accepted, whether the db-collector waits for it and the lines Loki holds,
# then the Collector's logs exporter (otel_logs_state).
delivery_state() {
	for backlog in $backlogs; do
		name=${backlog%%:*} span_s=${backlog#*:}
		printf '%s_read=%s %s_blocked=%s %s_loki=%s ' "$name" "$(backlog_read "$name")" \
			"$name" "$(prom_value "max(heartbeat_outsystems_export_blocked{collector=\"oslogs-backlog-$name\"}) or vector(0)")" \
			"$name" "$(loki_value "$(total_query "$name" "$span_s")")"
	done
	otel_logs_state
}
# state_value <state> <key>
state_value() { printf '%s\n' "$1" | tr ' ' '\n' | sed -n "s/^$2=//p"; }
# Drained: the Collector accepted every row, the db-collector waits for none,
# and the Collector holds no request, queued or in flight. Every row is then
# in Loki or lost.
drained() {
	for backlog in $backlogs; do
		name=${backlog%%:*}
		[ "$(state_value "$1" "${name}_read")" = "$backlog_rows" ] && [ "$(state_value "$1" "${name}_blocked")" = 0 ] || return 1
	done
	[ "$(state_value "$1" queued)" = 0 ] && [ "$(state_value "$1" in_flight)" = 0 ]
}
# Delivered: drained, and Loki holds a line for every row of both backlogs.
delivered() {
	drained "$1" || return 1
	for backlog in $backlogs; do
		gt "$backlog_rows" "$(state_value "$1" "${backlog%%:*}_loki")" && return 1
	done
	true
}
# Waits on the delivery, not on a fixed time: it ends complete once
# delivered, stalled once no word of the state has changed for 90s (a
# drained delivery with rows missing in Loki ends this way too), or at a
# 900s cap while it keeps moving. A delivery takes under a minute (6 to 38s
# over 20 CI runs). Each change of the state is kept, to show the course of
# a delivery that fails.
start=$(date +%s) since=$start last= ended=
while :; do
	state=$(delivery_state 2>/dev/null)
	now=$(date +%s)
	if [ "$state" != "$last" ]; then
		last=$state since=$now
		printf '      %ss: %s\n' $((now - start)) "$state" >>"$WORK/backlog-delivery.txt"
	fi
	if delivered "$state"; then
		ended=complete
	elif [ $((now - since)) -ge 90 ]; then
		ended="stalled 90s"
	elif [ $((now - start)) -ge 900 ]; then
		ended="cap 900s"
	else
		sleep 5
		continue
	fi
	break
done
printf '      delivery ended after %ss (%s): %s\n' $(($(date +%s) - start)) "$ended" "$state"
printf '      Loki discarded, by reason: %s\n' "$(loki_discarded)"
failed= missing= twice=0 rows_all=0
for backlog in $backlogs; do
	name=${backlog%%:*} span_s=${backlog#*:}
	read_rows=$(backlog_read "$name")
	distinct=$(loki_value "$(distinct_query "$name" "$span_s")")
	total=$(loki_value "$(total_query "$name" "$span_s")")
	printf '      %s backlog: db-collector read %s rows; Loki holds %s distinct rows in %s lines\n' "$name" "$read_rows" "$distinct" "$total"
	rows_all=$((rows_all + backlog_rows))
	if [ "$read_rows" != "$backlog_rows" ]; then
		failed="$failed; the db-collector read $read_rows of the $backlog_rows rows of the $name backlog"
	elif [ "$distinct" != "$read_rows" ]; then
		failed="$failed; Loki holds $distinct of the $read_rows rows of the $name backlog" missing=true
	elif ! gt "$total" "$((distinct - 1))"; then
		failed="$failed; Loki counted '$total' lines for the $distinct rows of the $name backlog"
	else
		twice=$((twice + total - distinct))
	fi
done
# Every line held twice was sent twice: the Collector's logs exporter sent
# at least as many records beyond the rows of the backlogs (audit records
# share the exporter, so this only errs towards passing). More lines twice
# would be rows the db-collector read twice, which its row count above
# catches, or a copy made after the Collector.
sent=$(state_value "$state" sent)
sent_twice=$((${sent:-0} - rows_all))
printf '      lines held twice: %s; records the Collector sent beyond the rows: %s\n' "$twice" "$sent_twice"
if [ -z "$failed" ] && [ "$twice" -gt 0 ] && [ "$twice" -gt "$sent_twice" ]; then
	failed="Loki holds $twice lines twice, but the Collector sent only $sent_twice records more than the rows of the backlogs"
fi
if [ -n "$failed" ]; then
	echo "      the delivery at each change:"
	cat "$WORK/backlog-delivery.txt"
	# Drained, a row missing in Loki is lost: neither collector holds it.
	drained "$state" ||
		fail "the delivery had not finished when the wait ended ($ended), so no row is known lost: ${failed#; }"
	[ -z "$missing" ] ||
		fail "rows lost: nothing is left to deliver in the db-collector or the Collector, yet ${failed#; }"
	fail "${failed#; }"
fi
pass "the Collector restarted during two backlogs (45 minutes and three hours); Loki holds all $backlog_rows rows of each ($twice lines held twice, within the $sent_twice records the Collector sent beyond the rows)"

printf '\nAll kind e2e checks passed on %s.\n' "$CLUSTER"
