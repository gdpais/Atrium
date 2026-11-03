#!/bin/sh
# Decides which of CI's slower jobs a change needs (ADR 0007). Reads the
# changed paths, one per line, on stdin and prints one flag per line:
#
#   db_collector=true|false   the SQL Server probe tests and PatientZero's live test
#   chart=true|false          helm lint and the rendered profiles, Helm 4 and 3;
#                             also lints and renders PatientZero's chart
#   kind_e2e=true|false       the acceptance checks on a temporary kind cluster
#
# The Go tests, vet, rules and docs checks are cheap and always run, so they
# have no flag. A path this script does not know turns every flag on: new
# directories run everything until they are added below. CI_RUN_ALL=true turns
# every flag on as well (pushes to master and manual runs).
#
# Usage: git diff --name-only --no-renames A B | scripts/ci-changes.sh
set -eu

db_collector=false
chart=false
kind_e2e=false
all=${CI_RUN_ALL:-false}

while IFS= read -r path; do
	[ -n "$path" ] || continue
	case $path in
	# Checked by the always-run job only: docs, the docs generator, release
	# bookkeeping and the live demo's shell helpers, which only the docs use.
	*.md | docs/* | tools/docsite/* | LICENSE | .gitignore | renovate.json | \
		version.txt | release-please-config.json | .release-please-manifest.json | \
		scripts/demo-helpers.sh) ;;
	services/db-collector/*)
		db_collector=true
		kind_e2e=true
		;;
	infra/helm/*)
		chart=true
		kind_e2e=true
		;;
	tests/chart_*) chart=true ;;
	infra/kind/* | scripts/kind.sh | scripts/kind-e2e.sh) kind_e2e=true ;;
	scripts/sqlserver-test.sh) db_collector=true ;;
	# PatientZero's chart: make chart-check lints and renders it. Only
	# make patientzero-up installs it, so no other job reads it.
	tools/patientzero/chart/*) chart=true ;;
	# Its load Job's pod labels are the peer the chart's NetworkPolicy
	# admits; make chart-check checks that they match.
	tools/patientzero/scripts/patientzero.sh)
		db_collector=true
		chart=true
		;;
	# PatientZero (ADR 0010), its own module: the always-run job tests it and
	# make test-sqlserver runs its live test. No other job builds or deploys
	# it.
	tools/patientzero/*) db_collector=true ;;
	# Creates the collector's login for the probe tests and for kind.
	scripts/sqlserver-login.sh)
		db_collector=true
		kind_e2e=true
		;;
	scripts/install-tools.sh | scripts/tools-check.sh | scripts/chart-deps.sh)
		chart=true
		kind_e2e=true
		;;
	# Read only by the Go tests, which always run.
	packages/telemetry-contracts/*) ;;
	# The API, and the migrations its image embeds: the always-run job tests
	# them, checks the generated code (make generate-check) and runs the
	# PostgreSQL and Valkey integration tests (make test-integration); the
	# chart deploys the image, which kind_e2e builds and migrates with.
	apps/api/* | db/migrations/*) kind_e2e=true ;;
	# Everything else can affect every job: go.mod, go.sum, internal/, the
	# config schema, the other tests and scripts, Makefile, .github/,
	# .dockerignore, config/ and anything new.
	*) all=true ;;
	esac
done

if [ "$all" = true ]; then
	db_collector=true
	chart=true
	kind_e2e=true
fi

printf 'db_collector=%s\nchart=%s\nkind_e2e=%s\n' "$db_collector" "$chart" "$kind_e2e"
