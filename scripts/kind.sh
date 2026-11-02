#!/bin/sh
# Local kind workflow for Heartbeat (ADR 0003). The Makefile calls this; run
# `make help` for the targets.
#
# Every kubectl and helm call names the kind context explicitly, so this script
# never acts on whatever cluster your current context points at.
#
# Usage: scripts/kind.sh <command>
#   up               create the cluster if needed, build and load images, deploy
#   down             delete the kind cluster (and only it)
#   cluster          create the cluster if it does not exist
#   images           build the service images, tag them by content, load them
#                    into kind
#   deploy           rebuild images if the Go sources are newer, create the
#                    local Secrets and helm upgrade --install
#   status           show pods and services
#   sqlserver-up     start the Docker SQL Server target and deploy against it
#   sqlserver-down   remove the SQL Server container and its volume
#
# Environment (defaults in brackets):
#   KIND_NODE_IMAGE      node image, pinned in the Makefile (required for cluster)
#   KIND_CLUSTER         cluster name [heartbeat]
#   KIND_CONFIG          cluster config; empty for none [infra/kind/cluster.yaml]
#   PROFILE              full | minimal [full]
#   EXTRA_VALUES         more values files, space-separated []
#   HEARTBEAT_NAMESPACE  namespace [heartbeat]
#   SQLSERVER_CONTAINER  Docker SQL Server container [heartbeat-sqlserver-dev]
#   SQLSERVER_ENV_FILE   SA password and collector credential file [.env.sqlserver-dev]
#   SQLSERVER_HOST_PORT  loopback host port for SQL Server; empty for none [11433]
#   SQLSERVER_MEMORY_LIMIT_MB  SQL Server's memory cap in MB, at least 2048 [2048]
set -eu
cd "$(dirname "$0")/.."

CLUSTER=${KIND_CLUSTER:-heartbeat}
KIND_CONFIG=${KIND_CONFIG-infra/kind/cluster.yaml}
PROFILE=${PROFILE:-full}
EXTRA_VALUES=${EXTRA_VALUES:-}
NAMESPACE=${HEARTBEAT_NAMESPACE:-heartbeat}
SQLSERVER_CONTAINER=${SQLSERVER_CONTAINER:-heartbeat-sqlserver-dev}
SQLSERVER_ENV_FILE=${SQLSERVER_ENV_FILE:-.env.sqlserver-dev}
SQLSERVER_HOST_PORT=${SQLSERVER_HOST_PORT-11433}
SQLSERVER_MEMORY_LIMIT_MB=${SQLSERVER_MEMORY_LIMIT_MB:-2048}
SQLSERVER_IMAGE=mcr.microsoft.com/mssql/server:2022-latest
RELEASE=heartbeat
CHART=infra/helm/heartbeat
VALUES_DIR=infra/helm/values
STATE_DIR=.tmp/kind/$CLUSTER
CONTEXT=kind-$CLUSTER

kubectl_() { kubectl --context "$CONTEXT" "$@"; }
log() { printf '==> %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

cluster_exists() { kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; }

cmd_cluster() {
	if cluster_exists; then
		log "kind cluster $CLUSTER exists"
		return
	fi
	: "${KIND_NODE_IMAGE:?set KIND_NODE_IMAGE (the Makefile pins it)}"
	log "creating kind cluster $CLUSTER"
	if [ -n "$KIND_CONFIG" ]; then
		kind create cluster --name "$CLUSTER" --image "$KIND_NODE_IMAGE" --config "$KIND_CONFIG" --wait 120s
	else
		kind create cluster --name "$CLUSTER" --image "$KIND_NODE_IMAGE" --wait 120s
	fi
}

# Maps `uname -m` on the node to Docker's architecture names.
node_arch() {
	machine=$(docker exec "$CLUSTER-control-plane" uname -m)
	case "$machine" in
	x86_64) echo amd64 ;;
	aarch64 | arm64) echo arm64 ;;
	*) die "unsupported node architecture $machine" ;;
	esac
}

# Builds for the node's own architecture and tags each image by its content
# (image ID), so a code change always yields a new tag and a stale image
# cannot hide behind a reused one like :local. Each entry is
# <image>:<Dockerfile>; the build context is the repository root.
SERVICE_IMAGES="db-collector:services/db-collector/Dockerfile api:apps/api/Dockerfile"
cmd_images() {
	cluster_exists || die "kind cluster $CLUSTER does not exist; run make kind-up"
	arch=$(node_arch)
	mkdir -p "$STATE_DIR"
	: >"$STATE_DIR/images.env.tmp"
	for entry in $SERVICE_IMAGES; do
		svc=${entry%%:*}
		log "building heartbeat/$svc for linux/$arch"
		# --provenance=false keeps a single-platform image that kind can load.
		docker build --quiet --platform "linux/$arch" --provenance=false \
			-t "heartbeat/$svc:build" -f "${entry#*:}" . >/dev/null
		id=$(docker image inspect -f '{{.Id}}' "heartbeat/$svc:build")
		built_arch=$(docker image inspect -f '{{.Architecture}}' "heartbeat/$svc:build")
		[ "$built_arch" = "$arch" ] || die "heartbeat/$svc is $built_arch, node is $arch"
		tag=dev-$(printf '%s' "${id#sha256:}" | cut -c1-12)
		docker tag "heartbeat/$svc:build" "heartbeat/$svc:$tag"
		log "loading heartbeat/$svc:$tag into $CLUSTER"
		kind load docker-image --name "$CLUSTER" "heartbeat/$svc:$tag" >/dev/null
		var=$(printf '%s' "$svc" | tr 'a-z-' 'A-Z_')_TAG
		printf '%s=%s\n' "$var" "$tag" >>"$STATE_DIR/images.env.tmp"
	done
	mv "$STATE_DIR/images.env.tmp" "$STATE_DIR/images.env"
}

# Applies a Secret built with --dry-run, so reruns update it in place.
apply_secret() {
	name=$1
	shift
	kubectl_ -n "$NAMESPACE" create secret generic "$name" "$@" --dry-run=client -o yaml | kubectl_ apply -f - >/dev/null
}

# Prints a Secret's key, or nothing when the Secret or key does not exist.
secret_value() {
	kubectl_ -n "$NAMESPACE" get secret "$1" --ignore-not-found -o "jsonpath={.data.$2}" | base64 -d
}

# Applies a Secret from key=value lines on standard input, through a 0600
# file, so a password never reaches a command line.
apply_secret_env() {
	name=$1
	env_file=$(mktemp "${TMPDIR:-/tmp}/heartbeat-$name.XXXXXX")
	cat >"$env_file"
	apply_secret "$name" --from-env-file="$env_file"
	rm -f "$env_file"
}

# The bundled PostgreSQL's and Valkey's passwords (ADR 0015), and the API's
# logins to both. Generated once and then kept: PostgreSQL reads its password
# only when it creates the database, so a new one would lock the API out of
# the existing volume. The API's Secret is derived from the other two on
# every deploy, so the three always agree.
cmd_api_secrets() {
	postgres_password=$(secret_value heartbeat-postgresql password)
	if [ -z "$postgres_password" ]; then
		postgres_password=$(openssl rand -hex 24)
		printf 'password=%s\n' "$postgres_password" | apply_secret_env heartbeat-postgresql
	fi
	valkey_password=$(secret_value heartbeat-valkey password)
	if [ -z "$valkey_password" ]; then
		valkey_password=$(openssl rand -hex 24)
		printf 'password=%s\n' "$valkey_password" | apply_secret_env heartbeat-valkey
	fi
	printf 'HEARTBEAT_CREDENTIAL_SECRET_API_POSTGRES=heartbeat:%s\nHEARTBEAT_CREDENTIAL_SECRET_API_REDIS=default:%s\n' \
		"$postgres_password" "$valkey_password" | apply_secret_env heartbeat-api-credentials
}

# Local-only Secrets. Production creates these outside the chart (ADR 0005).
cmd_secrets() {
	kubectl_ create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl_ apply -f - >/dev/null
	apply_secret heartbeat-runtime --from-literal=admin-token=local-admin-token
	apply_secret grafana-admin --from-literal=admin-user=admin --from-literal=admin-password=admin
	cmd_api_secrets
}

chart_deps() {
	if ! helm dependency list "$CHART" 2>/dev/null | awk 'NR > 1 && NF { if ($NF != "ok") bad = 1 } END { exit bad }'; then
		log "fetching chart dependencies"
		scripts/chart-deps.sh
	fi
}

# True when the services' sources changed after the last `images` run (a pull
# or a branch switch), so their tags in images.env are older than the code.
# The API embeds db/migrations. An image images.env has no tag for (one added
# since) counts as stale too.
images_stale() {
	for entry in $SERVICE_IMAGES; do
		var=$(printf '%s' "${entry%%:*}" | tr 'a-z-' 'A-Z_')_TAG
		grep -q "^$var=" "$STATE_DIR/images.env" || return 0
	done
	[ -n "$(find services apps db internal go.mod go.sum -newer "$STATE_DIR/images.env" 2>/dev/null | head -n 1)" ]
}

helm_upgrade() {
	helm upgrade --install "$RELEASE" "$CHART" \
		--kube-context "$CONTEXT" --namespace "$NAMESPACE" \
		"$@" \
		--set "dbCollector.image.tag=$DB_COLLECTOR_TAG" \
		--set "api.image.tag=$API_TAG" \
		--wait --timeout 10m
}

# podManagementPolicy cannot change on an existing StatefulSet, and the chart
# moved the collector from OrderedReady to Parallel. A cluster deployed before
# that deletes the StatefulSet once and leaves its pod running
# (--cascade=orphan); the upgrade creates it again and the new StatefulSet
# adopts the pod, so the collector restarts only if its template changed.
# The values are rendered first, so a deploy that would fail on them leaves
# the StatefulSet alone. Takes helm_upgrade's arguments.
migrate_collector_policy() {
	policy=$(kubectl_ -n "$NAMESPACE" get statefulset db-collector --ignore-not-found -o jsonpath='{.spec.podManagementPolicy}')
	case "$policy" in
	"" | Parallel) return 0 ;;
	esac
	helm template "$RELEASE" "$CHART" --namespace "$NAMESPACE" "$@" \
		--set "dbCollector.image.tag=$DB_COLLECTOR_TAG" --set "api.image.tag=$API_TAG" >/dev/null
	log "recreating StatefulSet db-collector with podManagementPolicy Parallel (was $policy); its pod keeps running until the deploy replaces it"
	kubectl_ -n "$NAMESPACE" delete statefulset db-collector --cascade=orphan --wait >/dev/null
}

cmd_deploy() {
	cluster_exists || die "kind cluster $CLUSTER does not exist; run make kind-up"
	# A deploy pairs the current chart with the image: rebuild when the code is
	# newer than the image. Unchanged content builds from cache to the same tag.
	if [ ! -f "$STATE_DIR/images.env" ]; then
		cmd_images
	elif images_stale; then
		log "service sources changed since the last image build; rebuilding"
		cmd_images
	fi
	# shellcheck disable=SC1090
	. "./$STATE_DIR/images.env"
	chart_deps
	cmd_secrets
	set -- -f "$VALUES_DIR/kind.yaml"
	case "$PROFILE" in
	full) ;;
	minimal) set -- "$@" -f "$VALUES_DIR/minimal.yaml" ;;
	*) die "PROFILE must be full or minimal, not $PROFILE" ;;
	esac
	# sqlserver-up leaves a marker so later deploys keep the SQL Server target.
	if [ -f "$STATE_DIR/sqlserver-dev" ]; then
		set -- "$@" -f "$VALUES_DIR/sqlserver-dev.yaml"
	fi
	for file in $EXTRA_VALUES; do
		set -- "$@" -f "$file"
	done
	migrate_collector_policy "$@"
	log "deploying release $RELEASE ($PROFILE) to $CONTEXT/$NAMESPACE"
	helm_upgrade "$@"
	cmd_status
}

cmd_status() {
	kubectl_ -n "$NAMESPACE" get pods,svc
}

cmd_up() {
	cmd_cluster
	cmd_images
	cmd_deploy
}

cmd_down() {
	if cluster_exists; then
		kind delete cluster --name "$CLUSTER"
	else
		log "kind cluster $CLUSTER does not exist"
	fi
	rm -rf "$STATE_DIR"
}

# SQL Server does not start with less than about 2 GB.
sqlserver_memory_valid() {
	case "$SQLSERVER_MEMORY_LIMIT_MB" in
	"" | *[!0-9]*) die "SQLSERVER_MEMORY_LIMIT_MB must be a number of MB, not $SQLSERVER_MEMORY_LIMIT_MB" ;;
	esac
	[ "$SQLSERVER_MEMORY_LIMIT_MB" -ge 2048 ] || die "SQLSERVER_MEMORY_LIMIT_MB must be at least 2048 (SQL Server needs 2 GB), not $SQLSERVER_MEMORY_LIMIT_MB"
}

# A container keeps the environment it was created with, so one created
# before the cap, or with another, is only reported. Its databases are on
# the volume, so recreating the container keeps them.
sqlserver_memory_check() {
	cap=$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$SQLSERVER_CONTAINER" | sed -n 's/^MSSQL_MEMORY_LIMIT_MB=//p')
	[ "$cap" = "$SQLSERVER_MEMORY_LIMIT_MB" ] && return 0
	if [ -n "$cap" ]; then cap="a $cap MB memory cap"; else cap="no memory cap"; fi
	log "warning: $SQLSERVER_CONTAINER has $cap, not $SQLSERVER_MEMORY_LIMIT_MB MB, and keeps it until it is recreated; its databases stay on the ${SQLSERVER_CONTAINER}-data volume: docker rm -f $SQLSERVER_CONTAINER, then make sqlserver-dev-up"
}

env_value() {
	grep "^$1=" "$SQLSERVER_ENV_FILE" | head -n 1 | cut -d= -f2-
}

# SQL Server stays a Docker container outside the cluster (ADR 0003). It joins
# the kind network, so pods reach it by container name, and publishes 1433
# only on the host's loopback (or not at all with SQLSERVER_HOST_PORT=).
# The collector logs in as a least-privilege login that this creates with sa;
# the sa password never leaves the env file and the container.
cmd_sqlserver_up() {
	[ -f "$SQLSERVER_ENV_FILE" ] || die "$SQLSERVER_ENV_FILE is missing; run make sqlserver-dev-init"
	password=$(env_value MSSQL_SA_PASSWORD)
	credential=$(env_value HEARTBEAT_CREDENTIAL_ENV_SQLSERVER_DEV)
	[ -n "$password" ] && [ -n "$credential" ] || die "$SQLSERVER_ENV_FILE must set MSSQL_SA_PASSWORD and HEARTBEAT_CREDENTIAL_ENV_SQLSERVER_DEV"
	collector_login=${credential%%:*}
	if [ "$collector_login" = "$credential" ] || [ -z "$collector_login" ] ||
		[ "$(printf '%s' "$collector_login" | tr 'A-Z' 'a-z')" = sa ]; then
		die "HEARTBEAT_CREDENTIAL_ENV_SQLSERVER_DEV in $SQLSERVER_ENV_FILE must be <login>:<password> for a dedicated login, not sa; run make sqlserver-dev-init to add one"
	fi
	sqlserver_memory_valid
	cmd_cluster
	if [ -z "$(docker ps -aq --filter "name=^${SQLSERVER_CONTAINER}\$")" ]; then
		log "starting $SQLSERVER_CONTAINER (memory capped at $SQLSERVER_MEMORY_LIMIT_MB MB)"
		set --
		if [ -n "$SQLSERVER_HOST_PORT" ]; then
			set -- -p "127.0.0.1:$SQLSERVER_HOST_PORT:1433"
		fi
		# The password goes in through the environment, not the command line.
		# Uncapped, SQL Server sizes itself to 80% of the Docker VM, so a load
		# flood or a second kind cluster ran the VM out of memory and got it
		# killed; capped, it pages its buffer pool instead.
		MSSQL_SA_PASSWORD=$password MSSQL_MEMORY_LIMIT_MB=$SQLSERVER_MEMORY_LIMIT_MB docker run -d --name "$SQLSERVER_CONTAINER" \
			--platform linux/amd64 --network kind "$@" \
			-e ACCEPT_EULA=Y -e MSSQL_PID=Developer -e MSSQL_SA_PASSWORD -e MSSQL_MEMORY_LIMIT_MB \
			-v "${SQLSERVER_CONTAINER}-data:/var/opt/mssql" \
			--health-cmd 'SQLCMDPASSWORD="$MSSQL_SA_PASSWORD" /opt/mssql-tools18/bin/sqlcmd -S localhost -U sa -C -Q "SELECT 1" >/dev/null 2>&1 || exit 1' \
			--health-interval 10s --health-timeout 5s --health-retries 20 --health-start-period 30s \
			"$SQLSERVER_IMAGE" >/dev/null
	else
		sqlserver_memory_check
		docker start "$SQLSERVER_CONTAINER" >/dev/null
	fi
	log "waiting for $SQLSERVER_CONTAINER to become healthy"
	i=0
	until [ "$(docker inspect -f '{{.State.Health.Status}}' "$SQLSERVER_CONTAINER")" = healthy ]; do
		i=$((i + 1))
		[ "$i" -le 60 ] || die "$SQLSERVER_CONTAINER did not become healthy"
		sleep 5
	done
	log "granting $collector_login only the documented collector permissions"
	MSSQL_SA_PASSWORD=$password HEARTBEAT_COLLECTOR_LOGIN=$collector_login HEARTBEAT_COLLECTOR_PASSWORD=${credential#*:} \
		scripts/sqlserver-login.sh "$SQLSERVER_CONTAINER"
	cmd_secrets
	# The credential reaches kubectl in a 0600 file, never on its command line.
	secret_env=$(mktemp "${TMPDIR:-/tmp}/heartbeat-sqlserver-credential.XXXXXX")
	trap 'rm -f "$secret_env"' EXIT
	trap 'exit 130' INT TERM
	printf 'HEARTBEAT_CREDENTIAL_ENV_SQLSERVER_DEV=%s\n' "$credential" >"$secret_env"
	apply_secret heartbeat-sqlserver-dev-credentials --from-env-file="$secret_env"
	rm -f "$secret_env"
	mkdir -p "$STATE_DIR"
	touch "$STATE_DIR/sqlserver-dev"
	cmd_deploy
	# The collector reads the credential from the mounted Secret on every
	# connection, so a new password needs no restart; the first deploy with
	# sqlserver-dev.yaml rolls the pod to mount it.
	kubectl_ -n "$NAMESPACE" rollout status statefulset/db-collector --timeout 3m
}

cmd_sqlserver_down() {
	docker rm -f "$SQLSERVER_CONTAINER" >/dev/null 2>&1 || true
	docker volume rm "${SQLSERVER_CONTAINER}-data" >/dev/null 2>&1 || true
	log "removed $SQLSERVER_CONTAINER and its volume"
	if [ -f "$STATE_DIR/sqlserver-dev" ]; then
		rm -f "$STATE_DIR/sqlserver-dev"
		if cluster_exists; then
			log "redeploying without the SQL Server target"
			cmd_deploy
		fi
	fi
}

case "${1:-}" in
up) cmd_up ;;
down) cmd_down ;;
cluster) cmd_cluster ;;
images) cmd_images ;;
deploy) cmd_deploy ;;
status) cmd_status ;;
sqlserver-up) cmd_sqlserver_up ;;
sqlserver-down) cmd_sqlserver_down ;;
*) sed -n '2,/^set -eu/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//' >&2; exit 2 ;;
esac
