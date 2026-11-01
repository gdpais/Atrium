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
for job in db-collector otel-collector loki alertmanager; do
	wait_for 60 "Prometheus scrapes $job" value_is "up{job=\"$job\"}" 1
done
svc_get grafana:3000 /api/datasources/proxy/uid/loki/loki/api/v1/labels | jq -e '.status == "success"' >/dev/null ||
	fail "Grafana's Loki datasource cannot reach Loki"
pass "Prometheus scrapes the collector, the OTel Collector, Loki and Alertmanager; Grafana queries Loki"
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
blocked="db-collector-headless:8082/healthz loki:3100/ready prometheus:9090/-/ready alertmanager:9093/-/ready otel-collector:4318/v1/logs"
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
[ "$(printf '%s\n' "$results" | grep -c .)" = 14 ] || fail "expected 14 probe results"
printf '%s\n' "$results" | while read -r url code; do
	case "$url:$code" in
	*grafana*:0) ;;
	*grafana*) fail "the probe pod cannot reach Grafana ($url exit $code), so its other results prove nothing" ;;
	*:124 | *:143) ;;
	*) fail "$url from a pod the policies do not list: exit $code, not a connection timeout" ;;
	esac
done
pass "unlisted pods time out on the collector, Loki, Prometheus, Alertmanager and the OTel Collector, NodePorts included"
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
		printf '\nGO\nCREATE USER [%s] FOR LOGIN [%s];\nGRANT SELECT ON dbo.oslog_Error TO [%s];\nGRANT SELECT ON dbo.oslog_Error_Previous TO [%s];\n' "$oslog_login" "$oslog_login" "$oslog_login" "$oslog_login"
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
# Distinct rows by their Request_Key, and lines in total: a batch the
# Collector sent again after the restart (delivery is at least once) would
# count twice in the total if Loki kept both copies. The total keeps only
# service_name, so it is one series rather than one per row, cheap to poll.
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
failed= missing=
for backlog in $backlogs; do
	name=${backlog%%:*} span_s=${backlog#*:}
	read_rows=$(backlog_read "$name")
	distinct=$(loki_value "$(distinct_query "$name" "$span_s")")
	total=$(loki_value "$(total_query "$name" "$span_s")")
	printf '      %s backlog: db-collector read %s rows; Loki holds %s distinct rows in %s lines\n' "$name" "$read_rows" "$distinct" "$total"
	if [ "$read_rows" != "$backlog_rows" ]; then
		failed="$failed; the db-collector read $read_rows of the $backlog_rows rows of the $name backlog"
	elif [ "$distinct" != "$read_rows" ]; then
		failed="$failed; Loki holds $distinct of the $read_rows rows of the $name backlog" missing=true
	elif [ "$total" != "$read_rows" ]; then
		failed="$failed; Loki holds $total lines for the $read_rows rows of the $name backlog: a row was stored twice"
	fi
done
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
pass "the Collector restarted during two backlogs (45 minutes and three hours); Loki holds all $backlog_rows rows of each, once"

printf '\nAll kind e2e checks passed on %s.\n' "$CLUSTER"
