#!/bin/sh
# Checks that a tag may be published as a release (ADR 0006):
# - it is a vX.Y.Z tag;
# - its commit is on master;
# - the chart version at that commit matches the tag;
# - the required CI checks passed on that commit: ci-ok (ADR 0007), or for a
#   commit from before ci-ok existed, the three checks master required then.
#   A release tag is usually pushed together with its merge commit, so this
#   waits for running checks.
#
# Usage: GH_TOKEN=... GITHUB_REPOSITORY=owner/repo scripts/release-verify.sh <tag>
# Writes version= and sha= to $GITHUB_OUTPUT when it is set.
set -eu

tag=${1:?usage: scripts/release-verify.sh <tag>}
: "${GITHUB_REPOSITORY:?}"
timeout_seconds=${RELEASE_VERIFY_TIMEOUT:-5400}

if ! printf '%s\n' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$'; then
	echo "release tags look like v1.2.3, got $tag" >&2
	exit 1
fi
version=${tag#v}

# The tag must already be fetched (actions/checkout with fetch-depth: 0 does).
git fetch --quiet origin master
sha=$(git rev-parse "refs/tags/$tag^{commit}")
if ! git merge-base --is-ancestor "$sha" origin/master; then
	echo "$tag points at $sha, which is not on master" >&2
	exit 1
fi

chart_version=$(git show "$sha:infra/helm/atrium/Chart.yaml" | sed -n 's/^version: *\([^ #]*\).*/\1/p')
if [ "$chart_version" != "$version" ]; then
	echo "Chart.yaml at $tag says version $chart_version, expected $version" >&2
	exit 1
fi

# ci-ok gives CI's verdict in one check. On master every job runs, so it
# covers the full suite.
if git cat-file -e "$sha:.github/workflows/ci.yml" 2>/dev/null; then
	required_checks="ci-ok"
else
	required_checks="test|Helm chart|kind end-to-end"
fi

deadline=$(($(date +%s) + timeout_seconds))
while :; do
	pending=""
	failed=""
	old_ifs=$IFS
	IFS='|'
	for name in $required_checks; do
		# The latest run of each check counts, so a successful re-run replaces a failure.
		result=$(gh api "repos/$GITHUB_REPOSITORY/commits/$sha/check-runs?per_page=100" \
			--jq "[.check_runs[] | select(.name == \"$name\")] | sort_by(.started_at) | last | if . == null then \"missing\" elif .status != \"completed\" then \"running\" else .conclusion end")
		case $result in
		success) ;;
		missing | running) pending="$pending '$name'" ;;
		*) failed="$failed '$name' ($result)" ;;
		esac
	done
	IFS=$old_ifs
	if [ -n "$failed" ]; then
		echo "required checks failed on $sha:$failed" >&2
		exit 1
	fi
	if [ -z "$pending" ]; then
		break
	fi
	if [ "$(date +%s)" -ge "$deadline" ]; then
		echo "timed out waiting for checks on $sha:$pending" >&2
		exit 1
	fi
	echo "waiting for$pending on $sha"
	sleep 30
done

echo "$tag ($sha) can be released"
if [ -n "${GITHUB_OUTPUT:-}" ]; then
	printf 'version=%s\nsha=%s\n' "$version" "$sha" >>"$GITHUB_OUTPUT"
fi
