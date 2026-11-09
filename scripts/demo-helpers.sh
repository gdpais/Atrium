# shellcheck shell=bash
# Shell helpers for the live demo (docs/guides/live-demo.md). Source this file
# from the repository root in bash or zsh; it defines functions and changes
# nothing until you call one:
#
#   . scripts/demo-helpers.sh
#
#   hb_sql            run SQL from stdin as the collector's login
#   hb_sql_sa         run SQL from stdin as sa (setup and workload only)
#   hb_probe NAME     run probe NAME exactly as the collector sends it, as its login
#   hb_metrics REGEX  collector /metrics samples matching REGEX, comments dropped
#   hb_prom PROMQL    Prometheus instant query, one "labels value" line per series
#   hb_admin [JQ ARGS] GET /admin/config through jq (default: target states)
#   hb_reload         POST /admin/config/reload
#   hb_logs [N]       last N (default 20) collector log lines, compacted
#   hb_k ARGS         kubectl in the kind cluster's atrium namespace
#
# Defaults match make kind-up and make sqlserver-dev-up; override with
# HB_COLLECTOR, HB_PROMETHEUS, HB_ADMIN_TOKEN, HB_SQL_CONTAINER, HB_ENV_FILE
# and HB_CONTEXT. Passwords come from the env file and reach sqlcmd through
# the environment, never on a command line.

HB_COLLECTOR=${HB_COLLECTOR:-http://localhost:8082}
HB_PROMETHEUS=${HB_PROMETHEUS:-http://localhost:9090}
HB_ADMIN_TOKEN=${HB_ADMIN_TOKEN:-local-admin-token}
HB_SQL_CONTAINER=${HB_SQL_CONTAINER:-atrium-sqlserver-dev}
HB_ENV_FILE=${HB_ENV_FILE:-.env.sqlserver-dev}
HB_CONTEXT=${HB_CONTEXT:-kind-atrium}

_hb_env() { grep "^$1=" "$HB_ENV_FILE" | head -n 1 | cut -d= -f2-; }

# _hb_sqlcmd LOGIN PASSWORD [sqlcmd args]: -W trims padding, -s '|' separates
# columns, -b fails on a SQL error, and -I sets QUOTED_IDENTIFIER ON like the
# collector's driver does (the cpu probe's XML methods need it).
_hb_sqlcmd() {
	local login=$1 password=$2
	shift 2
	SQLCMDPASSWORD=$password docker exec -i -e SQLCMDPASSWORD "$HB_SQL_CONTAINER" \
		/opt/mssql-tools18/bin/sqlcmd -S localhost -U "$login" -C -b -I -W -s '|' "$@"
}

# shellcheck disable=SC2119,SC2120 # callers may pass sqlcmd options; hb_probe passes none
hb_sql() {
	local cred
	cred=$(_hb_env ATRIUM_CREDENTIAL_ENV_SQLSERVER_DEV)
	[ -n "$cred" ] || { echo "hb_sql: no collector credential in $HB_ENV_FILE" >&2; return 1; }
	_hb_sqlcmd "${cred%%:*}" "${cred#*:}" "$@"
}

hb_sql_sa() { _hb_sqlcmd sa "$(_hb_env MSSQL_SA_PASSWORD)" "$@"; }

hb_probe() {
	[ -n "${1:-}" ] || { echo "usage: hb_probe NAME (go run ./services/db-collector/cmd/probe-catalog lists them)" >&2; return 2; }
	local batch
	batch=$(GOCACHE=${GOCACHE:-$(pwd)/.tmp/gocache} go run ./services/db-collector/cmd/probe-catalog -sql "$1") || return
	printf '%s\n' "$batch" | hb_sql
}

hb_metrics() { curl -sS --max-time 5 "$HB_COLLECTOR/metrics" | grep -v '^#' | grep -E -- "${1:-.}"; }

hb_prom() {
	curl -sS --max-time 10 -G "$HB_PROMETHEUS/api/v1/query" --data-urlencode "query=$1" |
		jq -r 'if .status != "success" then "error: \(.error)"
			elif (.data.result | length) == 0 then "(no series)"
			else .data.result[] | "\(.metric | del(.__name__) | to_entries | map("\(.key)=\(.value)") | join(" ") | if . == "" then "{}" else . end)  \(.value[1])" end'
}

_hb_admin_summary='{version, status: .readiness.status, reasons: .readiness.reasons, targets: [.readiness.collectors[].targets[] | {name, state, consecutive_failures, error}]}'

hb_admin() {
	[ "$#" -gt 0 ] || set -- "$_hb_admin_summary"
	curl -sS --max-time 5 -H "Authorization: Bearer $HB_ADMIN_TOKEN" "$HB_COLLECTOR/admin/config" | jq "$@"
}

hb_reload() {
	curl -sS --max-time 35 -X POST -H "Authorization: Bearer $HB_ADMIN_TOKEN" -w '\nHTTP %{http_code}\n' "$HB_COLLECTOR/admin/config/reload"
}

hb_logs() {
	kubectl --context "$HB_CONTEXT" -n atrium logs db-collector-0 --tail "${1:-20}" |
		jq -c 'del(.time) | with_entries(select(.value != null))' 2>/dev/null
}

hb_k() { kubectl --context "$HB_CONTEXT" -n atrium "$@"; }
