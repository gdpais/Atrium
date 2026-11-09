#!/bin/sh
# Fetches the Atrium chart's pinned dependencies into
# infra/helm/atrium/charts/ (gitignored), exactly as Chart.lock lists them.
# Helm needs a named repository for each dependency URL, so add them first.
set -eu
cd "$(dirname "$0")/.."

chart=infra/helm/atrium

helm repo add prometheus-community https://prometheus-community.github.io/helm-charts --force-update >/dev/null
helm repo add grafana-community https://grafana-community.github.io/helm-charts --force-update >/dev/null
helm repo add open-telemetry https://open-telemetry.github.io/opentelemetry-helm-charts --force-update >/dev/null
# build (not update) installs the versions in Chart.lock and fails if the lock
# no longer matches Chart.yaml.
helm dependency build "$chart"
