#!/bin/sh
# Verifies the local toolchain against the pins in the Makefile.
# Reports every problem before failing, so one run lists all fixes needed.
set -u

: "${KIND_VERSION:?}" "${KIND_NODE_IMAGE:?}" "${HELM_MIN_VERSION:?}"

failures=0
ok() { printf 'ok    %s\n' "$1"; }
fail() { printf 'FAIL  %s\n' "$1"; failures=$((failures + 1)); }

# "v1.36.4" -> "1 36 4"
split_version() { echo "$1" | sed -E 's/^v?([0-9]+)\.([0-9]+)\.([0-9]+).*/\1 \2 \3/'; }

if docker info >/dev/null 2>&1; then
	ok "docker daemon reachable"
else
	fail "docker daemon not reachable: start Docker Desktop"
fi

if command -v kind >/dev/null 2>&1; then
	have=$(kind version | awk '{print $2}')
	# Patch releases are compatible; minor releases change node image support.
	if [ "${have%.*}" = "${KIND_VERSION%.*}" ]; then
		ok "kind $have"
	else
		fail "kind $have: want ${KIND_VERSION%.*}.x (brew install kind, or https://kind.sigs.k8s.io/docs/user/quick-start/#installation)"
	fi
else
	fail "kind not installed: want $KIND_VERSION"
fi

node_tag=${KIND_NODE_IMAGE#*:}
node_tag=${node_tag%@*}
set -- $(split_version "$node_tag")
node_minor=$2
if command -v kubectl >/dev/null 2>&1; then
	have=$(kubectl version --client -o json 2>/dev/null | sed -nE 's/.*"gitVersion": *"([^"]+)".*/\1/p' | head -n 1)
	set -- $(split_version "$have")
	skew=$(($2 - node_minor))
	# kubectl supports one minor version of skew with the API server.
	if [ "$1" -eq 1 ] && [ "$skew" -ge -1 ] && [ "$skew" -le 1 ]; then
		ok "kubectl $have (cluster $node_tag)"
	else
		fail "kubectl $have: want 1.$((node_minor - 1))-1.$((node_minor + 1)) for cluster $node_tag (brew install kubectl, and put it ahead of $(command -v kubectl) in PATH)"
	fi
else
	fail "kubectl not installed: want 1.$node_minor"
fi

if command -v helm >/dev/null 2>&1; then
	have=$(helm version --template '{{.Version}}')
	set -- $(split_version "$have")
	h_major=$1 h_minor=$2
	set -- $(split_version "$HELM_MIN_VERSION")
	if [ "$h_major" -eq "$1" ] && [ "$h_minor" -ge "$2" ]; then
		ok "helm $have"
	else
		fail "helm $have: want >= $HELM_MIN_VERSION within major $1 (brew install helm)"
	fi
else
	fail "helm not installed: want >= $HELM_MIN_VERSION"
fi

# go.mod's go directive makes the go command fetch the pinned toolchain itself.
if command -v go >/dev/null 2>&1; then
	ok "go $(go env GOVERSION) (go.mod requires $(sed -n 's/^go //p' go.mod))"
else
	fail "go not installed"
fi

# Node only builds and checks the Web UI (apps/web, ADR 0020); the API image
# builds the UI in its own Node stage, so the cluster loop works without it.
want_node=$(cat apps/web/.nvmrc)
if command -v node >/dev/null 2>&1; then
	have=$(node --version)
	if [ "${have#v}" = "$want_node" ] || [ "$(echo "${have#v}" | cut -d. -f1)" = "${want_node%%.*}" ]; then
		ok "node $have (apps/web/.nvmrc: $want_node)"
	else
		fail "node $have: want ${want_node%%.*}.x, ideally $want_node (apps/web/.nvmrc; nvm install, or brew install node@${want_node%%.*})"
	fi
else
	printf 'skip  node not installed: needed only to work on the Web UI (apps/web/.nvmrc: %s)\n' "$want_node"
fi

if [ "$failures" -gt 0 ]; then
	printf '%d tool check(s) failed\n' "$failures"
	exit 1
fi
