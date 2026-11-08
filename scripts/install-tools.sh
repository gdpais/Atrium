#!/bin/sh
# Installs the pinned kind, kubectl, Helm 4 and Helm 3 binaries into a
# directory (CI uses this; locally, install them with your package manager and
# run `make tools-check`). Versions come from the Makefile; every download is
# checked against its published SHA-256.
#
# Usage: KIND_VERSION=... KUBECTL_VERSION=... HELM_VERSION=... HELM3_VERSION=... \
#          scripts/install-tools.sh <dir>
# Helm 3 is installed as helm3; it is only used to check that the chart still
# renders with Helm 3 (ADR 0005).
set -eu

dir=${1:?usage: scripts/install-tools.sh <dir>}
: "${KIND_VERSION:?}" "${KUBECTL_VERSION:?}" "${HELM_VERSION:?}" "${HELM3_VERSION:?}"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) echo "unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$dir"

# download <url> <file>: curl retries a timeout, a refused connection and a
# 408, 429, 500, 502, 503 or 504 answer, waiting 1, 2, 4 ... 64 seconds
# between tries (about two minutes in all, longer if the host sends
# Retry-After), so a short outage at a release host doesn't fail the job. A
# stalled try gives up after five minutes. Other errors, such as a 404, fail
# at once. A failure names the URL, which curl's -s hides.
download() {
	curl -fsSL --retry 7 --retry-connrefused --connect-timeout 20 --max-time 300 \
		-o "$2" "$1" || { echo "download failed: $1" >&2; exit 1; }
}

# fetch <url> <checksum-url> <file>: downloads and verifies against the first
# field of the checksum file.
fetch() {
	download "$1" "$tmp/$3"
	download "$2" "$tmp/$3.sha256"
	want=$(awk '{print $1; exit}' "$tmp/$3.sha256")
	have=$( (sha256sum "$tmp/$3" 2>/dev/null || shasum -a 256 "$tmp/$3") | awk '{print $1}')
	[ "$want" = "$have" ] || { echo "checksum mismatch for $1" >&2; exit 1; }
}

fetch "https://github.com/kubernetes-sigs/kind/releases/download/$KIND_VERSION/kind-$os-$arch" \
	"https://github.com/kubernetes-sigs/kind/releases/download/$KIND_VERSION/kind-$os-$arch.sha256sum" kind
install -m 0755 "$tmp/kind" "$dir/kind"

fetch "https://dl.k8s.io/release/$KUBECTL_VERSION/bin/$os/$arch/kubectl" \
	"https://dl.k8s.io/release/$KUBECTL_VERSION/bin/$os/$arch/kubectl.sha256" kubectl
install -m 0755 "$tmp/kubectl" "$dir/kubectl"

for pair in "helm $HELM_VERSION" "helm3 $HELM3_VERSION"; do
	set -- $pair
	fetch "https://get.helm.sh/helm-$2-$os-$arch.tar.gz" \
		"https://get.helm.sh/helm-$2-$os-$arch.tar.gz.sha256sum" "$1.tar.gz"
	mkdir -p "$tmp/$1"
	tar -xzf "$tmp/$1.tar.gz" -C "$tmp/$1"
	install -m 0755 "$tmp/$1/$os-$arch/helm" "$dir/$1"
done

"$dir/kind" version
"$dir/kubectl" version --client
"$dir/helm" version --short
"$dir/helm3" version --short
