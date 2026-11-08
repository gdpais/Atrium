//go:build chart

package tests

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"heartbeat/internal/config"
)

// Renders the Heartbeat chart for every supported values combination and
// checks the rules from ADR 0003 and ADR 0005. Needs helm and the chart
// dependencies (make chart-deps); run with `make chart-check`. HELM selects
// the binary, so CI can repeat the run with Helm 3.

type chartProfile struct {
	name   string
	values []string
	// set holds --set overrides applied after the values files.
	set []string
	// inline is a values document applied last.
	inline string
	// flags are more helm template arguments, such as
	// --skip-schema-validation.
	flags []string
	// sameRenderAs names another profile whose render this one must match
	// byte for byte.
	sameRenderAs string
	// collectorIDs are the ids of the collectors in the rendered
	// integrations.yaml, in order; nil skips the check.
	collectorIDs []string
	// Objects (kind/name) the profile must and must not render.
	present, absent []string
	// collectorPeers are the sources the db-collector NetworkPolicy must
	// admit to port 8082, in peerString form; none means it denies all
	// ingress. Checked whenever the profile renders the policy.
	collectorPeers []string
	// collectorEgress are the destinations the db-collector NetworkPolicy
	// allows when it limits egress, in "TCP/1433 -> peerString" form; nil
	// means it must not limit egress.
	collectorEgress []string
	// backendPolicies are the bundled engines' NetworkPolicies the profile
	// must render, by name, each as the port and peer pairs it admits in
	// "TCP/3100 <- peerString" form; no pairs means it denies all ingress.
	// Any other engine policy must be absent. nil skips the check.
	backendPolicies map[string][]string
	// collectorReceivers are the OTel Collector's receivers; none means only
	// otlp.
	collectorReceivers []string
	// selfLogsOff means telemetry.enabled is false: the collector must send
	// its own logs nowhere.
	selfLogsOff bool
	// stateEmptyDir means the collector's state is an emptyDir instead of the
	// db-collector-state claim (dbCollector.persistence.enabled=false).
	stateEmptyDir bool
	// credentialsSecret is the Secret the collector must mount as its
	// credentials directory (dbCollector.credentials.existingSecret); empty
	// means none.
	credentialsSecret string
	// queueEmptyDir means the OTel Collector's export queue is an emptyDir
	// instead of the otel-collector-queue claim
	// (otelCollectorQueue.persistence.enabled=false).
	queueEmptyDir bool
	// goMemLimit is the collector's GOMEMLIMIT; empty means 204MiB, 80% of
	// the default 256Mi limit, and "-" means none.
	goMemLimit string
	// configReloadAlertOff means prometheusRules.configReloadAlert.enabled
	// is false: the rules ConfigMap must not hold the reload alert.
	configReloadAlertOff bool
	// wantError, when set, is part of the error helm must fail with; nothing
	// else is checked.
	wantError string
	// notes are lines the NOTES.txt that helm install prints must contain,
	// and notesAbsent text it must not.
	notes, notesAbsent []string
	// apiPeers are the sources the API's NetworkPolicy must admit to port
	// 8081, in peerString form; empty means it denies all ingress, nil skips
	// the check.
	apiPeers []string
	// apiDeps is where the API must find PostgreSQL and Redis; nil means the
	// bundled postgresql and valkey, without TLS.
	apiDeps *apiDependencies
	// apiSite is the API's HEARTBEAT_API_SITE_ORIGIN; empty means unset.
	apiSite string
	// engineExtraPeers are the peers the bundled postgresql and valkey admit
	// besides the API, by engine, in peerString form.
	engineExtraPeers map[string][]string
}

// bundledPrometheusPeer is the bundled Prometheus server as a NetworkPolicy
// peer of the release "heartbeat".
const bundledPrometheusPeer = "pods{app.kubernetes.io/component=server,app.kubernetes.io/instance=heartbeat,app.kubernetes.io/name=prometheus}"

// The other bundled components as NetworkPolicy peers, and the kind values'
// operator range (the host through a loopback NodePort).
const (
	dbCollectorPeer   = "pods{app.kubernetes.io/instance=heartbeat,app.kubernetes.io/name=db-collector}"
	apiPeer           = "pods{app.kubernetes.io/instance=heartbeat,app.kubernetes.io/name=api}"
	grafanaPeer       = "pods{app.kubernetes.io/instance=heartbeat,app.kubernetes.io/name=grafana}"
	otelCollectorPeer = "pods{app.kubernetes.io/instance=heartbeat,app.kubernetes.io/name=otelCollector}"
	lokiPeer          = "pods{app.kubernetes.io/instance=heartbeat,app.kubernetes.io/name=loki}"
	alertmanagerPeer  = "pods{app.kubernetes.io/instance=heartbeat,app.kubernetes.io/name=alertmanager}"
	kindHostPeer      = "ip 0.0.0.0/0 except 10.244.0.0/16"
	kubeDNSPeer       = "ns{kubernetes.io/metadata.name=kube-system} pods{k8s-app=kube-dns}"
)

// kindCollectorEgress is what the kind values let the collector reach: DNS,
// the OTel Collector and, on SQL Server's port, everything but the pods.
var kindCollectorEgress = []string{
	"UDP/53 -> " + kubeDNSPeer, "TCP/53 -> " + kubeDNSPeer,
	"TCP/4318 -> " + otelCollectorPeer, "TCP/4319 -> " + otelCollectorPeer,
	"TCP/1433 -> " + kindHostPeer,
}

// defaultBackendPolicies are the engine policies with every component on:
// each engine admits only the components that use it, on their ports.
var defaultBackendPolicies = map[string][]string{
	"otel-collector": slices.Concat(otlpSenders, []string{"TCP/8889 <- " + bundledPrometheusPeer}),
	"prometheus":     {"TCP/9090 <- " + grafanaPeer},
	"alertmanager":   {"TCP/9093 <- " + bundledPrometheusPeer},
	"loki": {
		"TCP/3100 <- " + otelCollectorPeer, "TCP/3100 <- " + grafanaPeer, "TCP/3100 <- " + bundledPrometheusPeer,
		"TCP/9095 <- " + lokiPeer, "TCP/7946 <- " + lokiPeer,
	},
	"tempo": {"TCP/4318 <- " + otelCollectorPeer, "TCP/3200 <- " + grafanaPeer, "TCP/3200 <- " + bundledPrometheusPeer},
}

// otlpSenders are the OTel Collector's OTLP clients: the db-collector and the
// API, on OTLP/HTTP (spans, the collector's OutSystems rows) and their own
// logs' port (ADR 0018).
var otlpSenders = []string{
	"TCP/4318 <- " + dbCollectorPeer, "TCP/4319 <- " + dbCollectorPeer,
	"TCP/4318 <- " + apiPeer, "TCP/4319 <- " + apiPeer,
}

// kindBackendPolicies adds the kind values' operator access to the engines
// with a loopback NodePort; Loki has none.
var kindBackendPolicies = withPairs(defaultBackendPolicies, map[string][]string{
	"otel-collector": {"TCP/4317 <- " + kindHostPeer, "TCP/4318 <- " + kindHostPeer, "TCP/4319 <- " + kindHostPeer, "TCP/8889 <- " + kindHostPeer},
	"prometheus":     {"TCP/9090 <- " + kindHostPeer},
	"alertmanager":   {"TCP/9093 <- " + kindHostPeer},
})

// withPairs returns policies with more pairs added to some of them, and the
// policies named in drop removed.
func withPairs(policies map[string][]string, add map[string][]string, drop ...string) map[string][]string {
	out := map[string][]string{}
	for name, pairs := range policies {
		if !slices.Contains(drop, name) {
			out[name] = slices.Concat(pairs, add[name])
		}
	}
	return out
}

// withoutPeers returns policies without the pairs that admit one of peers.
func withoutPeers(policies map[string][]string, peers ...string) map[string][]string {
	out := map[string][]string{}
	for name, pairs := range policies {
		out[name] = slices.DeleteFunc(slices.Clone(pairs), func(pair string) bool {
			_, peer, _ := strings.Cut(pair, " <- ")
			return slices.Contains(peers, peer)
		})
	}
	return out
}

var chartProfiles = []chartProfile{
	{
		name:            "defaults",
		present:         slices.Concat([]string{"StatefulSet/db-collector", "Deployment/prometheus", "StatefulSet/alertmanager", "Deployment/grafana", "StatefulSet/loki", "StatefulSet/otel-collector", "PersistentVolumeClaim/otel-collector-queue", "NetworkPolicy/db-collector"}, apiObjects),
		absent:          []string{"PersistentVolumeClaim/valkey-data", "ConfigMap/heartbeat-inventory"},
		collectorPeers:  []string{bundledPrometheusPeer},
		apiPeers:        []string{bundledPrometheusPeer},
		backendPolicies: defaultBackendPolicies,
		collectorIDs:    []string{"sqlserver-default"},
		notes: []string{
			"- heartbeat-runtime (key admin-token)",
			"- grafana-admin (keys admin-user, admin-password)",
			"- heartbeat-api-credentials (keys HEARTBEAT_CREDENTIAL_SECRET_API_POSTGRES, HEARTBEAT_CREDENTIAL_SECRET_API_REDIS)",
			"- heartbeat-postgresql (key password)",
			"- heartbeat-valkey (key password)",
			"kubectl -n heartbeat get secret heartbeat-runtime heartbeat-api-credentials heartbeat-postgresql heartbeat-valkey grafana-admin\n",
			"kubectl -n heartbeat rollout status statefulset/db-collector",
			"kubectl -n heartbeat rollout status deployment/api",
			"The bundled PostgreSQL and Valkey are for kind and CI",
			"kubectl -n heartbeat port-forward svc/grafana 3000:3000",
			"No SQL Server target is configured yet",
			"https://github.com/gdpais/heartbeat/blob/v",
			"/docs/guides/install-from-release.md",
		},
		// The collector's SQL Server logins are not mounted by default.
		notesAbsent: []string{"HEARTBEAT_CREDENTIAL_<REF>", "WARNING"},
	},
	{
		name:     "kind",
		values:   []string{"kind.yaml"},
		apiSite:  "http://localhost:8081",
		present:  slices.Concat([]string{"StatefulSet/db-collector", "StatefulSet/loki", "StatefulSet/otel-collector", "PersistentVolumeClaim/otel-collector-queue", "NetworkPolicy/db-collector", "ConfigMap/heartbeat-inventory"}, apiObjects),
		apiPeers: []string{bundledPrometheusPeer},
		// The host through the loopback NodePort; pods stay limited to Prometheus.
		collectorPeers:  []string{bundledPrometheusPeer, kindHostPeer},
		collectorEgress: kindCollectorEgress,
		backendPolicies: kindBackendPolicies,
	},
	{
		name:           "kind-minimal",
		values:         []string{"kind.yaml", "minimal.yaml"},
		apiSite:        "http://localhost:8081",
		present:        slices.Concat([]string{"StatefulSet/db-collector", "Deployment/prometheus", "StatefulSet/alertmanager", "Deployment/grafana", "NetworkPolicy/db-collector"}, apiObjects),
		apiPeers:       []string{bundledPrometheusPeer},
		absent:         []string{"StatefulSet/loki", "StatefulSet/otel-collector", "PersistentVolumeClaim/otel-collector-queue"},
		collectorPeers: []string{bundledPrometheusPeer, kindHostPeer},
		// Without the OTel Collector there is nothing to export to.
		collectorEgress: []string{"UDP/53 -> " + kubeDNSPeer, "TCP/53 -> " + kubeDNSPeer, "TCP/1433 -> " + kindHostPeer},
		// No policy for the engines that are off.
		backendPolicies: withPairs(kindBackendPolicies, nil, "otel-collector", "loki", "tempo"),
	},
	{
		name:              "kind-sqlserver-dev",
		values:            []string{"kind.yaml", "sqlserver-dev.yaml"},
		apiSite:           "http://localhost:8081",
		present:           []string{"StatefulSet/db-collector", "NetworkPolicy/db-collector"},
		collectorPeers:    []string{bundledPrometheusPeer, "ip 0.0.0.0/0 except 10.244.0.0/16"},
		collectorEgress:   kindCollectorEgress,
		credentialsSecret: "heartbeat-sqlserver-dev-credentials",
		notes: []string{
			"- heartbeat-sqlserver-dev-credentials (a HEARTBEAT_CREDENTIAL_<REF> key per credential_ref)",
			"get secret heartbeat-runtime heartbeat-sqlserver-dev-credentials heartbeat-api-credentials heartbeat-postgresql heartbeat-valkey grafana-admin\n",
			"WARNING: dbCollector.sqlserver.trustServerCertificate is true",
		},
		notesAbsent: []string{"No SQL Server target"},
	},
	{
		name:    "production-example",
		values:  []string{"production.example.yaml"},
		apiSite: "https://heartbeat.example.internal",
		present: []string{"StatefulSet/db-collector", "Ingress/grafana", "NetworkPolicy/db-collector", "Deployment/api", "NetworkPolicy/api"},
		// RDS and ElastiCache instead of the bundled engines (ADR 0015).
		absent:   bundledEngineObjects,
		apiPeers: []string{bundledPrometheusPeer},
		apiDeps: &apiDependencies{
			postgresHost: "heartbeat.<id>.<region>.rds.amazonaws.com", postgresPort: "5432", postgresDatabase: "heartbeat",
			sslMode: "verify-full", sslRootCert: "/etc/heartbeat/rds-ca/global-bundle.pem",
			redisAddr: "master.heartbeat.<id>.<region>.cache.amazonaws.com:6379", redisTLS: "true",
		},
		collectorPeers:    []string{bundledPrometheusPeer, "ip 10.100.0.0/16"},
		collectorEgress:   []string{"UDP/53 -> " + kubeDNSPeer, "TCP/53 -> " + kubeDNSPeer, "TCP/4318 -> " + otelCollectorPeer, "TCP/4319 -> " + otelCollectorPeer, "TCP/1433 -> ip 10.20.0.0/16"},
		backendPolicies:   defaultBackendPolicies,
		credentialsSecret: "heartbeat-sqlserver-credentials",
	},
	{
		// A shared Prometheus in another namespace replaces the bundled one.
		name: "external-prometheus",
		set: []string{
			"prometheus.enabled=false", "grafana.enabled=false",
			`dbCollector.networkPolicy.scrapers[0].namespaceSelector.matchLabels.kubernetes\.io/metadata\.name=monitoring`,
			`dbCollector.networkPolicy.scrapers[0].podSelector.matchLabels.app\.kubernetes\.io/name=prometheus`,
		},
		present:        []string{"StatefulSet/db-collector", "NetworkPolicy/db-collector"},
		absent:         []string{"Deployment/prometheus", "Deployment/grafana"},
		collectorPeers: []string{"ns{kubernetes.io/metadata.name=monitoring} pods{app.kubernetes.io/name=prometheus}"},
		// Without the bundled Grafana its Secret and port-forward are not listed.
		notes:       []string{"kubectl -n heartbeat get secret heartbeat-runtime heartbeat-api-credentials heartbeat-postgresql heartbeat-valkey\n"},
		notesAbsent: []string{"grafana"},
		backendPolicies: withPairs(withoutPeers(defaultBackendPolicies, bundledPrometheusPeer, grafanaPeer), nil,
			"prometheus", "alertmanager"),
		// The shared Prometheus is added to api.networkPolicy.peers as it is
		// to the collector's scrapers; without it the API admits no one.
		apiPeers: []string{},
	},
	{
		// Another observability platform (ADR 0011): no bundled engines or
		// content; the Collector scrapes the collector and sends logs and
		// metrics over OTLP.
		name:               "external-platform-example",
		values:             []string{"external-platform.example.yaml"},
		present:            []string{"StatefulSet/db-collector", "StatefulSet/otel-collector", "PersistentVolumeClaim/otel-collector-queue", "NetworkPolicy/db-collector"},
		absent:             []string{"Deployment/prometheus", "StatefulSet/alertmanager", "Deployment/grafana", "StatefulSet/loki", "ConfigMap/heartbeat-prometheus-rules", "ConfigMap/heartbeat-dashboards"},
		collectorPeers:     []string{"pods{app.kubernetes.io/name=otelCollector,component=statefulset-collector}"},
		collectorReceivers: []string{"otlp", "otlp/self", "prometheus"},
		// Only the db-collector sends to the Collector; nothing scrapes it.
		backendPolicies: map[string][]string{"otel-collector": otlpSenders},
		// The Collector scrapes the API in Prometheus' place.
		apiPeers: []string{"pods{app.kubernetes.io/name=otelCollector,component=statefulset-collector}"},
	},
	{
		// No peer at all must deny all ingress, never render an empty `from`
		// (which would admit every source).
		name:    "collector-policy-without-peers",
		set:     []string{"prometheus.enabled=false", "grafana.enabled=false"},
		present: []string{"StatefulSet/db-collector", "NetworkPolicy/db-collector"},
		backendPolicies: withPairs(withoutPeers(defaultBackendPolicies, bundledPrometheusPeer, grafanaPeer), nil,
			"prometheus", "alertmanager"),
	},
	{
		// The bundled peer follows the subchart's name, like its selector.
		name:           "prometheus-name-override",
		set:            []string{"prometheus.nameOverride=prom"},
		present:        []string{"Deployment/prometheus", "NetworkPolicy/db-collector"},
		collectorPeers: []string{"pods{app.kubernetes.io/component=server,app.kubernetes.io/instance=heartbeat,app.kubernetes.io/name=prom}"},
		backendPolicies: map[string][]string{
			"otel-collector": slices.Concat(otlpSenders, []string{"TCP/8889 <- pods{app.kubernetes.io/component=server,app.kubernetes.io/instance=heartbeat,app.kubernetes.io/name=prom}"}),
			"prometheus":     {"TCP/9090 <- " + grafanaPeer},
			"alertmanager":   {"TCP/9093 <- pods{app.kubernetes.io/component=server,app.kubernetes.io/instance=heartbeat,app.kubernetes.io/name=prom}"},
			"loki": {
				"TCP/3100 <- " + otelCollectorPeer, "TCP/3100 <- " + grafanaPeer,
				"TCP/3100 <- pods{app.kubernetes.io/component=server,app.kubernetes.io/instance=heartbeat,app.kubernetes.io/name=prom}",
				"TCP/9095 <- " + lokiPeer, "TCP/7946 <- " + lokiPeer,
			},
			"tempo": {
				"TCP/4318 <- " + otelCollectorPeer, "TCP/3200 <- " + grafanaPeer,
				"TCP/3200 <- pods{app.kubernetes.io/component=server,app.kubernetes.io/instance=heartbeat,app.kubernetes.io/name=prom}",
			},
		},
	},
	{
		// Only an empty podSelector admits every pod in the namespace.
		name:      "collector-peer-empty-pod-selector",
		inline:    "dbCollector: {networkPolicy: {operators: [{podSelector: {}}]}}",
		wantError: "values don't meet the specifications of the schema",
	},
	{
		name:      "collector-peer-empty-match-labels",
		inline:    "dbCollector: {networkPolicy: {scrapers: [{namespaceSelector: {matchLabels: {}}}]}}",
		wantError: "values don't meet the specifications of the schema",
	},
	{
		name:            "backend-policies-disabled",
		set:             []string{"backendNetworkPolicies.enabled=false"},
		present:         []string{"StatefulSet/loki", "NetworkPolicy/db-collector"},
		collectorPeers:  []string{bundledPrometheusPeer},
		backendPolicies: map[string][]string{},
	},
	{
		// Extra peers are admitted to each engine's ports, the Collector's
		// OTLP ports and exporter included.
		name: "backend-extra-peers",
		inline: "backendNetworkPolicies:\n" +
			"  otelCollector: {extraPeers: [{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: apps}}, podSelector: {matchLabels: {app: sender}}}]}\n" +
			"  prometheus: {extraPeers: [{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: monitoring}}, podSelector: {matchLabels: {app.kubernetes.io/name: grafana}}}]}\n" +
			"  alertmanager: {extraPeers: [{ipBlock: {cidr: 10.100.0.0/16}}]}\n" +
			"  loki: {extraPeers: [{podSelector: {matchLabels: {app: log-reader}}}]}\n",
		collectorPeers: []string{bundledPrometheusPeer},
		backendPolicies: withPairs(defaultBackendPolicies, map[string][]string{
			"otel-collector": {
				"TCP/4317 <- ns{kubernetes.io/metadata.name=apps} pods{app=sender}",
				"TCP/4318 <- ns{kubernetes.io/metadata.name=apps} pods{app=sender}",
				"TCP/4319 <- ns{kubernetes.io/metadata.name=apps} pods{app=sender}",
				"TCP/8889 <- ns{kubernetes.io/metadata.name=apps} pods{app=sender}",
			},
			"prometheus":   {"TCP/9090 <- ns{kubernetes.io/metadata.name=monitoring} pods{app.kubernetes.io/name=grafana}"},
			"alertmanager": {"TCP/9093 <- ip 10.100.0.0/16"},
			"loki":         {"TCP/3100 <- pods{app=log-reader}"},
		}),
	},
	{
		// An engine nothing uses denies all ingress, never an empty from.
		name:            "prometheus-without-grafana",
		set:             []string{"grafana.enabled=false"},
		collectorPeers:  []string{bundledPrometheusPeer},
		backendPolicies: withoutPeers(defaultBackendPolicies, grafanaPeer),
	},
	{
		// A disabled scrape job admits no Prometheus, and the Collector's
		// exporter port is closed with the exporter.
		name: "backend-scrapes-disabled",
		set: []string{
			"prometheus.scrapeConfigs.loki.enabled=false", "prometheus.scrapeConfigs.otel-collector.enabled=false",
			"prometheus.scrapeConfigs.tempo.enabled=false", "otelCollector.ports.prom-exporter.enabled=false",
		},
		collectorPeers: []string{bundledPrometheusPeer},
		backendPolicies: map[string][]string{
			"otel-collector": otlpSenders,
			"prometheus":     defaultBackendPolicies["prometheus"],
			"alertmanager":   defaultBackendPolicies["alertmanager"],
			"loki":           withoutPeers(defaultBackendPolicies, bundledPrometheusPeer)["loki"],
			"tempo":          withoutPeers(defaultBackendPolicies, bundledPrometheusPeer)["tempo"],
		},
	},
	{
		// Alertmanager replicas gossip on the cluster port.
		name:           "alertmanager-replicas",
		set:            []string{"prometheus.alertmanager.replicaCount=3"},
		collectorPeers: []string{bundledPrometheusPeer},
		backendPolicies: withPairs(defaultBackendPolicies, map[string][]string{
			"alertmanager": {"TCP/9094 <- " + alertmanagerPeer, "UDP/9094 <- " + alertmanagerPeer},
		}),
	},
	{
		name:      "backend-peer-empty-pod-selector",
		inline:    "backendNetworkPolicies: {loki: {extraPeers: [{podSelector: {}}]}}",
		wantError: "values don't meet the specifications of the schema",
	},
	{
		name:      "backend-policy-unknown-engine",
		inline:    "backendNetworkPolicies: {grafana: {extraPeers: []}}",
		wantError: "values don't meet the specifications of the schema",
	},
	{
		name:    "collector-policy-disabled",
		set:     []string{"dbCollector.networkPolicy.enabled=false"},
		present: []string{"StatefulSet/db-collector"},
		absent:  []string{"NetworkPolicy/db-collector"},
	},
	{
		name:           "collector-state-without-volume",
		set:            []string{"dbCollector.persistence.enabled=false"},
		present:        []string{"StatefulSet/db-collector"},
		collectorPeers: []string{bundledPrometheusPeer},
		stateEmptyDir:  true,
	},
	{
		name:           "collector-memory-limit",
		set:            []string{"dbCollector.resources.limits.memory=1Gi"},
		present:        []string{"StatefulSet/db-collector"},
		collectorPeers: []string{bundledPrometheusPeer},
		goMemLimit:     "819MiB",
	},
	{
		name:           "collector-go-memory-limit-override",
		set:            []string{"dbCollector.goMemLimit=300MiB"},
		present:        []string{"StatefulSet/db-collector"},
		collectorPeers: []string{bundledPrometheusPeer},
		goMemLimit:     "300MiB",
	},
	{
		name:           "collector-without-memory-limit",
		set:            []string{"dbCollector.resources.limits.memory=null"},
		present:        []string{"StatefulSet/db-collector"},
		collectorPeers: []string{bundledPrometheusPeer},
		goMemLimit:     "-",
	},
	{
		name:      "collector-memory-limit-unparsable",
		set:       []string{"dbCollector.resources.limits.memory=256m"},
		wantError: "dbCollector.resources.limits.memory",
	},
	{
		// A longer shutdown delay, with the grace period raised to cover it.
		name:           "api-longer-shutdown-delay",
		inline:         "api:\n  extraEnv:\n    - name: HEARTBEAT_API_SHUTDOWN_DELAY\n      value: 20s\n  terminationGracePeriodSeconds: 38\n",
		present:        []string{"Deployment/api"},
		collectorPeers: []string{bundledPrometheusPeer},
	},
	{
		name:      "api-site-origin-with-path",
		set:       []string{"api.siteOrigin=https://heartbeat.example.com/app"},
		wantError: "values don't meet the specifications of the schema",
	},
	{
		name:      "api-site-origin-without-scheme",
		set:       []string{"api.siteOrigin=heartbeat.example.com"},
		wantError: "values don't meet the specifications of the schema",
	},
	{
		// Go rejects SI units such as M in GOMEMLIMIT.
		name:      "collector-go-memory-limit-invalid",
		set:       []string{"dbCollector.goMemLimit=300M"},
		wantError: "values don't meet the specifications of the schema",
	},
	{
		name:      "collector-state-without-size",
		set:       []string{"dbCollector.persistence.size="},
		wantError: "values don't meet the specifications of the schema",
	},
	{
		name: "otel-queue-without-volume",
		inline: "otelCollectorQueue: {persistence: {enabled: false}}\n" +
			"otelCollector: {extraVolumes: [{name: export-queue, emptyDir: {sizeLimit: 1Gi}}]}\n",
		present:        []string{"StatefulSet/otel-collector"},
		absent:         []string{"PersistentVolumeClaim/otel-collector-queue"},
		collectorPeers: []string{bundledPrometheusPeer},
		queueEmptyDir:  true,
	},
	{
		// Turning persistence off must not leave the pod waiting for a claim
		// the chart no longer creates.
		name:      "otel-queue-claim-without-persistence",
		set:       []string{"otelCollectorQueue.persistence.enabled=false"},
		wantError: "make otelCollector.extraVolumes' export-queue volume an emptyDir",
	},
	{
		name:      "otel-queue-persistence-without-claim",
		inline:    "otelCollector: {extraVolumes: [{name: export-queue, emptyDir: {}}]}\n",
		wantError: "needs the export-queue volume on the otel-collector-queue claim",
	},
	{
		name:      "otel-queue-without-size",
		set:       []string{"otelCollectorQueue.persistence.size="},
		wantError: "values don't meet the specifications of the schema",
	},
	{
		// A misspelt top-level key fails instead of being ignored (v0.1.0
		// deployed the collector anyway).
		name:      "unknown-top-level-key",
		set:       []string{"dbColector.enabled=false"},
		wantError: "values don't meet the specifications of the schema",
	},
	{
		// Egress needs the databases' addresses: with a target and none, the
		// install fails instead of cutting the collector off.
		name:      "collector-egress-without-targets",
		values:    []string{"kind.yaml", "sqlserver-dev.yaml"},
		set:       []string{"dbCollector.networkPolicy.egress.targets=null"},
		wantError: "dbCollector.networkPolicy.egress.enabled needs egress.targets",
	},
	{
		// The databases' ports come from the enabled collectors' targets, each
		// once, never from a disabled collector's; extra rules are kept as
		// given, one without ports allowing every port.
		name: "collector-egress-ports-and-extra-rules",
		inline: "dbCollector:\n" +
			"  networkPolicy:\n" +
			"    egress:\n" +
			"      enabled: true\n" +
			"      targets: [{ipBlock: {cidr: 10.20.0.0/16}}]\n" +
			"      dns: []\n" +
			"      extraRules:\n" +
			"        - {to: [{ipBlock: {cidr: 10.30.0.1/32}}], ports: [{protocol: TCP, port: 3128}]}\n" +
			"        - {to: [{ipBlock: {cidr: 10.30.0.2/32}}]}\n" +
			"integrations:\n" +
			"  collectors:\n" +
			"    - id: sql\n" +
			"      kind: sqlserver\n" +
			"      enabled: true\n" +
			"      config:\n" +
			"        environment: prod\n" +
			"        probes: [{name: waits}]\n" +
			"        targets:\n" +
			"          - {name: a, host: a.example.internal, port: 1433, database_name: master, credential_ref: env/a}\n" +
			"          - {name: b, host: b.example.internal, port: 14330, database_name: master, credential_ref: env/a}\n" +
			"          - {name: c, host: c.example.internal, port: 1433, database_name: master, credential_ref: env/a}\n" +
			"    - id: old\n" +
			"      kind: sqlserver\n" +
			"      enabled: false\n" +
			"      config:\n" +
			"        environment: prod\n" +
			"        probes: [{name: waits}]\n" +
			"        targets:\n" +
			"          - {name: d, host: d.example.internal, port: 15000, database_name: master, credential_ref: env/a}\n",
		present:        []string{"NetworkPolicy/db-collector"},
		collectorPeers: []string{bundledPrometheusPeer},
		collectorEgress: []string{
			"TCP/4318 -> " + otelCollectorPeer, "TCP/4319 -> " + otelCollectorPeer, "TCP/1433 -> ip 10.20.0.0/16", "TCP/14330 -> ip 10.20.0.0/16",
			"TCP/3128 -> ip 10.30.0.1/32", "any -> ip 10.30.0.2/32",
		},
	},
	{
		// The OTel Collector rule follows its OTLP/HTTP container port, as its
		// own policy does. Only a disabled collector has targets, so no target
		// peers are needed.
		name: "collector-egress-otlp-port-and-disabled-collector",
		inline: "otelCollector: {ports: {otlp-http: {containerPort: 14318}}}\n" +
			"dbCollector: {networkPolicy: {egress: {enabled: true}}}\n" +
			"integrations:\n" +
			"  collectors:\n" +
			"    - id: old\n" +
			"      kind: sqlserver\n" +
			"      enabled: false\n" +
			"      config:\n" +
			"        environment: prod\n" +
			"        probes: [{name: waits}]\n" +
			"        targets:\n" +
			"          - {name: d, host: d.example.internal, port: 1433, database_name: master, credential_ref: env/a}\n",
		present:         []string{"NetworkPolicy/db-collector"},
		collectorPeers:  []string{bundledPrometheusPeer},
		collectorEgress: []string{"UDP/53 -> " + kubeDNSPeer, "TCP/53 -> " + kubeDNSPeer, "TCP/14318 -> " + otelCollectorPeer, "TCP/4319 -> " + otelCollectorPeer},
	},
	{
		// Without its own logs the db-collector has no use for 4319, in either
		// direction.
		name:            "telemetry-off",
		inline:          "telemetry: {enabled: false}\ndbCollector: {networkPolicy: {egress: {enabled: true}}}\n",
		present:         []string{"NetworkPolicy/db-collector"},
		collectorPeers:  []string{bundledPrometheusPeer},
		collectorEgress: []string{"UDP/53 -> " + kubeDNSPeer, "TCP/53 -> " + kubeDNSPeer, "TCP/4318 -> " + otelCollectorPeer},
		backendPolicies: map[string][]string{
			"otel-collector": {"TCP/4318 <- " + dbCollectorPeer, "TCP/4318 <- " + apiPeer, "TCP/8889 <- " + bundledPrometheusPeer},
			"prometheus":     defaultBackendPolicies["prometheus"],
			"alertmanager":   defaultBackendPolicies["alertmanager"],
			"loki":           defaultBackendPolicies["loki"],
			"tempo":          defaultBackendPolicies["tempo"],
		},
		selfLogsOff: true,
	},
	{
		// A misspelt port key would be dropped, widening the rule to every port.
		name:      "collector-egress-extra-rule-port-typo",
		inline:    "dbCollector: {networkPolicy: {egress: {enabled: true, extraRules: [{to: [{ipBlock: {cidr: 10.30.0.1/32}}], ports: [{protocol: TCP, portt: 3128}]}]}}}\n",
		wantError: "values don't meet the specifications of the schema",
	},
	{
		// A misspelt key inside a component fails too: v0.1.0 ignored
		// dbCollector.enabeld and deployed the collector anyway (TODO 0.4).
		name:      "unknown-collector-key",
		set:       []string{"dbCollector.enabeld=false"},
		wantError: "values don't meet the specifications of the schema",
	},
	{
		name:      "unknown-collector-nested-key",
		inline:    "dbCollector: {persistance: {enabled: false}}",
		wantError: "values don't meet the specifications of the schema",
	},
	{
		// resources.limit (no s) rendered next to the default limits, so the
		// raise to 1Gi never applied and the collector kept 256Mi.
		name:      "unknown-collector-resources-key",
		inline:    "dbCollector: {resources: {limit: {memory: 1Gi}}}",
		wantError: "values don't meet the specifications of the schema",
	},
	{
		// A misspelt security field was ignored: the defaults still applied,
		// but a field they do not set, such as appArmorProfile, never reached
		// the pods.
		name:      "unknown-security-context-key",
		inline:    "podSecurityContext: {runAsNonroot: true}",
		wantError: "values don't meet the specifications of the schema",
	},
	{
		name:      "unknown-container-security-context-key",
		inline:    "containerSecurityContext: {readOnlyRootFileSystem: true}",
		wantError: "values don't meet the specifications of the schema",
	},
	{
		// A digest copied with its @ would render repository@@sha256:...
		name:      "invalid-collector-digest",
		inline:    "dbCollector: {image: {digest: \"@sha256:0000000000000000000000000000000000000000000000000000000000000000\"}}",
		wantError: "values don't meet the specifications of the schema",
	},
	{
		name:      "unknown-dashboards-key",
		set:       []string{"grafanaDashboards.enable=false"},
		wantError: "values don't meet the specifications of the schema",
	},
	{
		name:      "unknown-queue-key",
		inline:    "otelCollectorQueue: {persistance: {enabled: false}}",
		wantError: "values don't meet the specifications of the schema",
	},
	{
		// Every field the chart reads is still accepted.
		name: "every-collector-value",
		inline: "dbCollector:\n" +
			"  image: {repository: registry.example.internal/db-collector, tag: \"1.2.3\", digest: \"sha256:0000000000000000000000000000000000000000000000000000000000000000\", pullPolicy: Always}\n" +
			"  adminToken: {existingSecret: admin, key: token}\n" +
			"  credentials: {existingSecret: creds}\n" +
			"  configWatchInterval: 10s\n" +
			"  sqlserver: {trustServerCertificate: false}\n" +
			"  terminationGracePeriodSeconds: 60\n" +
			"  service: {type: ClusterIP, port: 8082}\n" +
			"  resources: {requests: {cpu: 100m, memory: 128Mi}, limits: {memory: 512Mi}}\n" +
			"  extraEnv: [{name: X, value: \"1\"}]\n" +
			"  podAnnotations: {a: b}\n" +
			"  nodeSelector: {kubernetes.io/os: linux}\n" +
			"  tolerations: [{key: k, operator: Exists}]\n" +
			"  affinity: {}\n" +
			"podSecurityContext: {runAsNonRoot: true, runAsUser: 65532, fsGroup: 65532, seccompProfile: {type: RuntimeDefault}, supplementalGroups: [1]}\n" +
			"containerSecurityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: [ALL]}}\n",
		present:           []string{"StatefulSet/db-collector"},
		collectorPeers:    []string{bundledPrometheusPeer},
		goMemLimit:        "409MiB",
		credentialsSecret: "creds",
	},
	{
		// Helm's global values still reach the subcharts, for example a
		// registry mirror.
		name:           "global-values",
		set:            []string{"global.imageRegistry=registry.example.internal"},
		present:        []string{"StatefulSet/db-collector", "Deployment/grafana"},
		collectorPeers: []string{bundledPrometheusPeer},
	},
	{
		// The reload alert is turned off with a value, not by editing rule
		// files; every other rule stays.
		name:                 "config-reload-alert-disabled",
		set:                  []string{"prometheusRules.configReloadAlert.enabled=false"},
		present:              []string{"ConfigMap/heartbeat-prometheus-rules"},
		collectorPeers:       []string{bundledPrometheusPeer},
		configReloadAlertOff: true,
	},
	{
		// Without the db-collector job the bundled Prometheus has nothing
		// to find the collector under, so the missing-target alert is left
		// out instead of firing for good.
		name:           "collector-scrape-disabled",
		set:            []string{"prometheus.scrapeConfigs.db-collector.enabled=false"},
		present:        []string{"StatefulSet/db-collector", "ConfigMap/heartbeat-prometheus-rules"},
		collectorPeers: []string{bundledPrometheusPeer},
	},
	{
		// Under another job name absent(up{job="db-collector"}) matches
		// nothing, so the rule is left out too.
		name:           "collector-job-renamed",
		set:            []string{"prometheus.scrapeConfigs.db-collector.job_name=heartbeat-collector"},
		present:        []string{"StatefulSet/db-collector", "ConfigMap/heartbeat-prometheus-rules"},
		collectorPeers: []string{bundledPrometheusPeer},
	},
	{
		name:      "config-reload-alert-misspelled",
		inline:    "prometheusRules: {configReloadAlrt: {enabled: false}}",
		wantError: "values don't meet the specifications of the schema",
	},
	{
		// No managedCollectors (ADR 0014) renders exactly what the chart
		// rendered before the key existed.
		name:           "managed-collectors-empty",
		inline:         "managedCollectors: []\n",
		sameRenderAs:   "defaults",
		collectorIDs:   []string{"sqlserver-default"},
		collectorPeers: []string{bundledPrometheusPeer},
	},
	{
		name:           "managed-collectors-null",
		set:            []string{"managedCollectors=null"},
		sameRenderAs:   "defaults",
		collectorPeers: []string{bundledPrometheusPeer},
	},
	{
		// The API's collectors follow the hand-written ones, and their
		// targets count like any other: egress opens their ports, and NOTES
		// no longer says no target is configured.
		name: "managed-collectors-appended",
		inline: managedCollectorsValues +
			"dbCollector: {networkPolicy: {egress: {enabled: true, targets: [{ipBlock: {cidr: 10.20.0.0/16}}]}}}\n",
		present:        []string{"NetworkPolicy/db-collector"},
		collectorIDs:   []string{"sqlserver-default", "api-finance-prod-60s", "api-hr-prod-300s"},
		collectorPeers: []string{bundledPrometheusPeer},
		collectorEgress: []string{
			"UDP/53 -> " + kubeDNSPeer, "TCP/53 -> " + kubeDNSPeer, "TCP/4318 -> " + otelCollectorPeer, "TCP/4319 -> " + otelCollectorPeer,
			"TCP/1433 -> ip 10.20.0.0/16", "TCP/14331 -> ip 10.20.0.0/16",
		},
		notesAbsent: []string{"No SQL Server target"},
	},
	{
		// Managed targets need the egress peers as much as hand-written ones.
		name:      "managed-collectors-egress-without-targets",
		inline:    managedCollectorsValues + "dbCollector: {networkPolicy: {egress: {enabled: true}}}\n",
		wantError: "dbCollector.networkPolicy.egress.enabled needs egress.targets",
	},
	{
		// The api- prefix is reserved for the API's collectors.
		name:      "hand-written-collector-api-prefix",
		inline:    "integrations: {collectors: [{id: api-finance-prod-60s, kind: sqlserver, enabled: true}]}\n",
		wantError: "at '/integrations/collectors/0/id'",
	},
	{
		// The collector trims ids, so leading spaces do not hide the prefix.
		name:      "hand-written-collector-api-prefix-after-spaces",
		inline:    "integrations: {collectors: [{id: \"  api-finance-prod-60s\", kind: sqlserver, enabled: true}]}\n",
		wantError: "at '/integrations/collectors/0/id'",
	},
	{
		name:      "managed-collector-without-api-prefix",
		inline:    "managedCollectors: [{id: finance-prod-60s, kind: sqlserver, enabled: true}]\n",
		wantError: "at '/managedCollectors/0/id'",
	},
	{
		// The API renders sqlserver collectors only; outsystems_logs
		// collectors stay hand-written (ADR 0017).
		name:      "managed-collector-not-sqlserver",
		inline:    "managedCollectors: [{id: api-logs-60s, kind: outsystems_logs, enabled: true}]\n",
		wantError: "at '/managedCollectors/0/kind'",
	},
	{
		// Managed collectors are checked like hand-written ones: a misspelt
		// key fails.
		name:      "managed-collector-unknown-key",
		inline:    "managedCollectors: [{id: api-finance-prod-60s, kind: sqlserver, enabled: true, confg: {}}]\n",
		wantError: "at '/managedCollectors/0'",
	},
	{
		// The collector would reject the file at its next reload, or fail to
		// start; the chart fails first.
		name: "managed-collectors-duplicate-id",
		inline: "managedCollectors:\n" +
			"  - {id: api-finance-prod-60s, kind: sqlserver, enabled: true}\n" +
			"  - {id: api-hr-prod-300s, kind: sqlserver, enabled: true}\n" +
			"  - {id: api-finance-prod-60s, kind: sqlserver, enabled: false}\n",
		wantError: `collector id "api-finance-prod-60s" is listed twice, at managedCollectors[0] and managedCollectors[2]`,
	},
	{
		name: "hand-written-collectors-duplicate-id",
		inline: "integrations:\n" +
			"  collectors:\n" +
			"    - {id: sql, kind: sqlserver, enabled: true}\n" +
			"    - {id: \" sql\", kind: sqlserver, enabled: false}\n",
		wantError: `collector id "sql" is listed twice, at integrations.collectors[0] and integrations.collectors[1]`,
	},
	{
		// Across the lists the prefix rule already keeps ids apart.
		name: "collectors-duplicate-id-across-lists",
		inline: "integrations: {collectors: [{id: api-finance-prod-60s, kind: sqlserver, enabled: true}]}\n" +
			"managedCollectors: [{id: api-finance-prod-60s, kind: sqlserver, enabled: true}]\n",
		wantError: "at '/integrations/collectors/0/id'",
	},
	{
		// Without the schema the templates still refuse it.
		name: "collectors-duplicate-id-across-lists-without-schema",
		inline: "integrations: {collectors: [{id: api-finance-prod-60s, kind: sqlserver, enabled: true}]}\n" +
			"managedCollectors: [{id: api-finance-prod-60s, kind: sqlserver, enabled: true}]\n",
		flags:     []string{"--skip-schema-validation"},
		wantError: `collector id "api-finance-prod-60s" is listed twice, at integrations.collectors[0] and managedCollectors[0]`,
	},
	{
		// External servers get TLS without being asked for it (ADR 0015):
		// verify-full and Redis TLS on.
		name:           "api-external-dependencies",
		collectorPeers: []string{bundledPrometheusPeer},
		set: []string{
			"postgresql.enabled=false", "valkey.enabled=false",
			"api.postgres.host=db.example.internal", "api.redis.addr=cache.example.internal:6380",
		},
		present:  []string{"Deployment/api", "NetworkPolicy/api"},
		absent:   bundledEngineObjects,
		apiPeers: []string{bundledPrometheusPeer},
		apiDeps: &apiDependencies{
			postgresHost: "db.example.internal", postgresPort: "5432", postgresDatabase: "heartbeat",
			sslMode: "verify-full", redisAddr: "cache.example.internal:6380", redisTLS: "true",
		},
		notesAbsent: []string{"heartbeat-postgresql", "heartbeat-valkey", "The bundled"},
	},
	{
		// What the values say wins over the defaults, for either kind of
		// server.
		name:           "api-dependencies-explicit",
		collectorPeers: []string{bundledPrometheusPeer},
		set: []string{
			"valkey.enabled=false", "api.postgres.sslMode=require", "api.postgres.port=6543", "api.postgres.database=ignored",
			"api.redis.addr=cache.example.internal:6379", "api.redis.tls=false",
		},
		present: []string{"Deployment/api", "StatefulSet/postgresql"},
		absent:  []string{"StatefulSet/valkey", "NetworkPolicy/valkey"},
		// The bundled PostgreSQL listens on 5432 and has its own database.
		apiDeps: &apiDependencies{
			postgresHost: "postgresql", postgresPort: "5432", postgresDatabase: "heartbeat",
			sslMode: "require", redisAddr: "cache.example.internal:6379", redisTLS: "false",
		},
		notes: []string{"The bundled PostgreSQL is for kind and CI"},
	},
	{
		name:      "api-without-postgresql",
		set:       []string{"postgresql.enabled=false"},
		wantError: "api.postgres.host is required when postgresql.enabled is false",
	},
	{
		name:      "api-without-valkey",
		set:       []string{"valkey.enabled=false"},
		wantError: "api.redis.addr is required when valkey.enabled is false",
	},
	{
		// Without the API, and with its scrape job off, nothing else
		// changes: the bundled engines are off too, and the OTel Collector
		// admits only the db-collector.
		name: "api-disabled",
		set: []string{
			"api.enabled=false", "postgresql.enabled=false", "valkey.enabled=false",
			"prometheus.scrapeConfigs.api.enabled=false",
		},
		present:         []string{"StatefulSet/db-collector"},
		absent:          slices.Concat(apiObjects, []string{"Service/api", "Service/api-headless"}),
		collectorPeers:  []string{bundledPrometheusPeer},
		backendPolicies: withoutPeers(defaultBackendPolicies, apiPeer),
		notesAbsent:     []string{"deployment/api", "heartbeat-api-credentials", "The bundled"},
	},
	{
		// The bundled engines without the API admit no one.
		name:            "engines-without-api",
		collectorPeers:  []string{bundledPrometheusPeer},
		set:             []string{"api.enabled=false", "prometheus.scrapeConfigs.api.enabled=false"},
		present:         []string{"StatefulSet/postgresql", "StatefulSet/valkey", "NetworkPolicy/postgresql", "NetworkPolicy/valkey"},
		absent:          []string{"Deployment/api", "NetworkPolicy/api"},
		backendPolicies: withoutPeers(defaultBackendPolicies, apiPeer),
	},
	{
		// The API's policy follows the api scrape job: off, Prometheus is not
		// admitted.
		name:           "api-scrape-disabled",
		collectorPeers: []string{bundledPrometheusPeer},
		set:            []string{"prometheus.scrapeConfigs.api.enabled=false"},
		present:        []string{"Deployment/api", "NetworkPolicy/api"},
		apiPeers:       []string{},
	},
	{
		name:           "api-extra-peers",
		collectorPeers: []string{bundledPrometheusPeer},
		inline:         "api: {networkPolicy: {peers: [{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: ingress}}, podSelector: {matchLabels: {app: ingress}}}]}}\n",
		present:        []string{"NetworkPolicy/api"},
		apiPeers:       []string{bundledPrometheusPeer, "ns{kubernetes.io/metadata.name=ingress} pods{app=ingress}"},
	},
	{
		name:           "api-policy-disabled",
		collectorPeers: []string{bundledPrometheusPeer},
		set:            []string{"api.networkPolicy.enabled=false", "postgresql.networkPolicy.enabled=false", "valkey.networkPolicy.enabled=false"},
		present:        []string{"Deployment/api"},
		absent:         []string{"NetworkPolicy/api", "NetworkPolicy/postgresql", "NetworkPolicy/valkey"},
	},
	{
		// Valkey keeps an append-only file on a claim when asked to.
		name:           "valkey-persistence",
		collectorPeers: []string{bundledPrometheusPeer},
		set:            []string{"valkey.persistence.enabled=true"},
		present:        []string{"StatefulSet/valkey", "PersistentVolumeClaim/valkey-data"},
	},
	{
		// Under another job name absent(up{job="api"}) matches nothing.
		name:           "api-job-renamed",
		set:            []string{"prometheus.scrapeConfigs.api.job_name=heartbeat-api"},
		collectorPeers: []string{bundledPrometheusPeer},
		present:        []string{"Deployment/api", "ConfigMap/heartbeat-prometheus-rules"},
	},
	{
		name:      "unknown-api-key",
		set:       []string{"api.enabeld=false"},
		wantError: "values don't meet the specifications of the schema",
	},
	{
		name:      "unknown-api-nested-key",
		inline:    "api: {postgres: {hostname: db.example.internal}}",
		wantError: "values don't meet the specifications of the schema",
	},
	{
		name:      "unknown-postgresql-key",
		inline:    "postgresql: {persistance: {size: 2Gi}}",
		wantError: "values don't meet the specifications of the schema",
	},
	{
		name:      "unknown-valkey-key",
		inline:    "valkey: {auth: {pasword: password}}",
		wantError: "values don't meet the specifications of the schema",
	},
	{
		// libpq's prefer and allow fall back to plain text; the API refuses
		// them too.
		name:      "api-ssl-mode-prefer",
		set:       []string{"api.postgres.sslMode=prefer"},
		wantError: "values don't meet the specifications of the schema",
	},
	{
		name:      "api-postgres-host-with-port",
		set:       []string{"api.postgres.host=db.example.internal:5432"},
		wantError: "values don't meet the specifications of the schema",
	},
	{
		name:      "api-redis-addr-url",
		set:       []string{"api.redis.addr=rediss://cache.example.internal:6379"},
		wantError: "values don't meet the specifications of the schema",
	},
	{
		// A password in a -c parameter name would reach the command line.
		name:      "postgresql-parameter-name",
		inline:    "postgresql: {parameters: {\"password=x -c shared_buffers\": 1MB}}",
		wantError: "values don't meet the specifications of the schema",
	},
	{
		// Every field the chart reads is still accepted.
		name:           "every-api-value",
		collectorPeers: []string{bundledPrometheusPeer},
		inline: "api:\n" +
			"  image: {repository: registry.example.internal/api, tag: \"1.2.3\", digest: \"sha256:0000000000000000000000000000000000000000000000000000000000000000\", pullPolicy: Always}\n" +
			"  replicas: 2\n" +
			"  credentials: {existingSecret: api-creds}\n" +
			"  postgres: {host: db.example.internal, port: 5433, database: hb, sslMode: verify-ca, sslRootCert: /etc/ca/ca.pem}\n" +
			"  redis: {addr: cache.example.internal:6379, tls: true}\n" +
			"  terminationGracePeriodSeconds: 30\n" +
			"  service: {port: 8081}\n" +
			"  networkPolicy: {enabled: true, peers: []}\n" +
			"  resources: {requests: {cpu: 100m, memory: 128Mi}, limits: {memory: 512Mi}}\n" +
			"  goMemLimit: 400MiB\n" +
			"  migrate: {resources: {requests: {cpu: 10m}}}\n" +
			"  extraEnv: [{name: X, value: \"1\"}]\n" +
			"  extraVolumes: [{name: ca, configMap: {name: ca}}]\n" +
			"  extraVolumeMounts: [{name: ca, mountPath: /etc/ca, readOnly: true}]\n" +
			"  podAnnotations: {a: b}\n" +
			"  nodeSelector: {kubernetes.io/os: linux}\n" +
			"  tolerations: [{key: k, operator: Exists}]\n" +
			"  affinity: {}\n" +
			"postgresql:\n" +
			"  image: {repository: postgres, tag: \"18.6\", pullPolicy: Always}\n" +
			"  auth: {username: hb, database: hb, existingSecret: pg, passwordKey: pw}\n" +
			"  parameters: {shared_buffers: 64MB, max_connections: 20, log_connections: true}\n" +
			"  persistence: {size: 2Gi, storageClassName: standard}\n" +
			"  networkPolicy: {enabled: true, extraPeers: [{podSelector: {matchLabels: {app: psql}}}]}\n" +
			"  resources: {limits: {memory: 512Mi}}\n" +
			"  nodeSelector: {}\n" +
			"  tolerations: []\n" +
			"  affinity: {}\n" +
			"valkey:\n" +
			"  image: {repository: valkey/valkey, tag: \"9.1.3\", pullPolicy: Always}\n" +
			"  auth: {existingSecret: vk, passwordKey: pw}\n" +
			"  maxmemory: 1gb\n" +
			"  persistence: {enabled: true, size: 2Gi, storageClassName: standard}\n" +
			"  networkPolicy: {enabled: true, extraPeers: []}\n" +
			"  resources: {limits: {memory: 1280Mi}}\n" +
			"  nodeSelector: {}\n" +
			"  tolerations: []\n" +
			"  affinity: {}\n",
		present:          slices.Concat(apiObjects, []string{"PersistentVolumeClaim/valkey-data"}),
		apiPeers:         []string{bundledPrometheusPeer},
		engineExtraPeers: map[string][]string{"postgresql": {"pods{app=psql}"}},
		apiDeps: &apiDependencies{
			postgresHost: "db.example.internal", postgresPort: "5433", postgresDatabase: "hb",
			sslMode: "verify-ca", sslRootCert: "/etc/ca/ca.pem", redisAddr: "cache.example.internal:6379", redisTLS: "true",
		},
	},
}

// managedCollectorsValues is a managedCollectors list as the Heartbeat API
// renders it from approved probe assignments (ADR 0014): one sqlserver
// collector per target and interval, with the id api-<target>-<interval>s.
const managedCollectorsValues = "managedCollectors:\n" +
	"  - id: api-finance-prod-60s\n" +
	"    kind: sqlserver\n" +
	"    enabled: true\n" +
	"    config:\n" +
	"      environment: prod\n" +
	"      scrape_interval: 60s\n" +
	"      probes: [{name: waits}, {name: blocking}]\n" +
	"      targets:\n" +
	"        - {name: finance-prod, host: finance-sql.example.internal, port: 1433, database_name: FinanceDB, credential_ref: env/finance-prod}\n" +
	"  - id: api-hr-prod-300s\n" +
	"    kind: sqlserver\n" +
	"    enabled: true\n" +
	"    config:\n" +
	"      environment: prod\n" +
	"      scrape_interval: 300s\n" +
	"      probes: [{name: storage}]\n" +
	"      targets:\n" +
	"        - {name: hr-prod, host: 10.20.0.7, port: 14331, database_name: HR, credential_ref: env/hr-prod}\n"

// Cluster-scoped kinds: the chart must install into a namespace it does not
// own, with no cluster-wide side effects.
var clusterScopedKinds = map[string]bool{
	"Namespace": true, "ClusterRole": true, "ClusterRoleBinding": true,
	"CustomResourceDefinition": true, "PersistentVolume": true, "StorageClass": true,
	"PriorityClass": true, "MutatingWebhookConfiguration": true,
	"ValidatingWebhookConfiguration": true, "APIService": true, "IngressClass": true,
}

type object struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name        string            `yaml:"name"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
	Data map[string]string `yaml:"data"`
	Spec yaml.Node         `yaml:"spec"`
	// Rules are a Role's.
	Rules []struct {
		Resources []string `yaml:"resources"`
	} `yaml:"rules"`
	// AutomountServiceAccountToken is a ServiceAccount's.
	AutomountServiceAccountToken *bool `yaml:"automountServiceAccountToken"`
}

func (o object) id() string { return o.Kind + "/" + o.Metadata.Name }

func helmTemplate(t *testing.T, profile chartProfile) []byte {
	t.Helper()
	out, stderr, err := runHelmTemplate(t, profile)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, stderr)
	}
	return out
}

// runHelmTemplate renders profile and returns stdout, stderr and the error.
func runHelmTemplate(t *testing.T, profile chartProfile) ([]byte, string, error) {
	t.Helper()
	return runHelm(t, profile, "template")
}

// runHelm runs a helm command (template, or install --dry-run=client) on the
// chart with profile's values and returns stdout, stderr and the error.
func runHelm(t *testing.T, profile chartProfile, command ...string) ([]byte, string, error) {
	t.Helper()
	root := repoRoot(t)
	helm := os.Getenv("HELM")
	if helm == "" {
		helm = "helm"
	}
	args := slices.Concat(command, []string{"heartbeat", filepath.Join(root, "infra/helm/heartbeat"), "--namespace", "heartbeat"}, profile.flags)
	for _, file := range profile.values {
		args = append(args, "-f", filepath.Join(root, "infra/helm/values", file))
	}
	for _, set := range profile.set {
		args = append(args, "--set", set)
	}
	if profile.inline != "" {
		path := filepath.Join(t.TempDir(), "inline.yaml")
		if err := os.WriteFile(path, []byte(profile.inline), 0o600); err != nil {
			t.Fatal(err)
		}
		args = append(args, "-f", path)
	}
	var stderr bytes.Buffer
	cmd := exec.Command(helm, args...)
	// An empty kubeconfig: no command here may reach a cluster.
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kubeconfig, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		err = fmt.Errorf("%s %s: %w", helm, strings.Join(args, " "), err)
	}
	return out, stderr.String(), err
}

// isHelm3 reports whether HELM (or helm) is Helm 3.
func isHelm3(t *testing.T) bool {
	t.Helper()
	helm := os.Getenv("HELM")
	if helm == "" {
		helm = "helm"
	}
	out, err := exec.Command(helm, "version", "--short").Output()
	if err != nil {
		t.Fatalf("%s version: %v", helm, err)
	}
	return strings.HasPrefix(strings.TrimSpace(string(out)), "v3.")
}

func parseObjects(t *testing.T, rendered []byte) map[string]object {
	t.Helper()
	// An upstream template emits a trailing tab after a scalar; strict YAML
	// parsers reject it, the API server does not.
	rendered = bytes.ReplaceAll(rendered, []byte("\t\n"), []byte("\n"))
	objects := map[string]object{}
	decoder := yaml.NewDecoder(bytes.NewReader(rendered))
	for {
		var obj object
		err := decoder.Decode(&obj)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("parse rendered chart: %v", err)
		}
		if obj.Kind == "" {
			continue
		}
		if _, dup := objects[obj.id()]; dup {
			t.Errorf("%s rendered twice", obj.id())
		}
		objects[obj.id()] = obj
	}
	return objects
}

func TestChartProfiles(t *testing.T) {
	for _, profile := range chartProfiles {
		t.Run(profile.name, func(t *testing.T) {
			if profile.wantError != "" {
				_, stderr, err := runHelmTemplate(t, profile)
				if err == nil || !strings.Contains(stderr, profile.wantError) {
					t.Fatalf("expected helm to fail with %q, got %v\n%s", profile.wantError, err, stderr)
				}
				return
			}
			first := helmTemplate(t, profile)
			// Argo CD renders the chart itself on every sync; output must not
			// change between renders.
			if second := helmTemplate(t, profile); !bytes.Equal(first, second) {
				t.Fatal("rendering is not deterministic")
			}
			if profile.sameRenderAs != "" {
				i := slices.IndexFunc(chartProfiles, func(p chartProfile) bool { return p.name == profile.sameRenderAs })
				if i < 0 {
					t.Fatalf("no profile %q", profile.sameRenderAs)
				}
				if !bytes.Equal(first, helmTemplate(t, chartProfiles[i])) {
					t.Errorf("render differs from profile %s", profile.sameRenderAs)
				}
			}
			objects := parseObjects(t, first)

			for _, id := range profile.present {
				if _, ok := objects[id]; !ok {
					t.Errorf("expected %s", id)
				}
			}
			for _, id := range profile.absent {
				if _, ok := objects[id]; ok {
					t.Errorf("unexpected %s", id)
				}
			}
			for id, obj := range objects {
				if clusterScopedKinds[obj.Kind] {
					t.Errorf("cluster-scoped object %s", id)
				}
				if hook := obj.Metadata.Annotations["helm.sh/hook"]; hook != "" {
					t.Errorf("%s is a Helm hook (%s); Argo CD maps hooks differently", id, hook)
				}
			}

			checkIntegrations(t, objects)
			checkNoSecretReaders(t, objects)
			checkCollectorIDs(t, objects, profile.collectorIDs)
			checkCollectorSingleton(t, objects)
			checkStatefulSetPolicies(t, objects)
			checkCollectorGoMemLimit(t, objects, profile.goMemLimit)
			checkInClusterEndpoints(t, objects)
			checkCollectorNetworkPolicy(t, objects, profile.collectorPeers)
			checkCollectorEgress(t, objects, profile.collectorEgress)
			checkBackendNetworkPolicies(t, objects, profile.backendPolicies)
			checkCollectorState(t, objects, profile.stateEmptyDir)
			checkCollectorCredentials(t, objects, profile.credentialsSecret)
			checkCollectorPipelines(t, objects, profile.collectorReceivers)
			checkLogDelivery(t, objects)
			checkSelfLogs(t, objects, profile.selfLogsOff)
			checkExportQueue(t, objects, profile.queueEmptyDir)
			checkLogIdentifiers(t, objects)
			checkLokiLabelPolicy(t, objects)
			checkLokiRetention(t, objects)
			checkTempoWAL(t, objects)
			checkGrafanaGzip(t, objects)
			checkNotes(t, profile)
			checkConfigReloadAlert(t, objects, profile.configReloadAlertOff)
			checkCollectorMissingAlert(t, objects)
			checkAPIMissingAlert(t, objects)
			checkAPI(t, objects, profile.apiDeps)
			checkAPISessions(t, objects, profile.apiSite)
			checkAPINetworkPolicy(t, objects, profile.apiPeers)
			checkBundledEngines(t, objects, profile.engineExtraPeers)
		})
	}
}

// The credentials Secret must reach the collector as a directory of files
// named by HEARTBEAT_CREDENTIALS_DIR, never as environment variables: the
// collector reads the files on every connection, so a rotated key needs no
// restart (ADR 0002). A subPath mount would never see the kubelet's updates.
func checkCollectorCredentials(t *testing.T, objects map[string]object, secret string) {
	t.Helper()
	sts, ok := objects["StatefulSet/db-collector"]
	if !ok {
		return
	}
	var spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Env []struct {
						Name  string `yaml:"name"`
						Value string `yaml:"value"`
					} `yaml:"env"`
					EnvFrom      []any `yaml:"envFrom"`
					VolumeMounts []struct {
						Name      string `yaml:"name"`
						MountPath string `yaml:"mountPath"`
						SubPath   string `yaml:"subPath"`
						ReadOnly  bool   `yaml:"readOnly"`
					} `yaml:"volumeMounts"`
				} `yaml:"containers"`
				Volumes []struct {
					Name   string `yaml:"name"`
					Secret *struct {
						SecretName  string `yaml:"secretName"`
						DefaultMode int    `yaml:"defaultMode"`
						Optional    bool   `yaml:"optional"`
					} `yaml:"secret"`
				} `yaml:"volumes"`
			} `yaml:"spec"`
		} `yaml:"template"`
	}
	if err := sts.Spec.Decode(&spec); err != nil {
		t.Fatal(err)
	}
	container := spec.Template.Spec.Containers[0]
	if len(container.EnvFrom) > 0 {
		t.Error("db-collector loads envFrom; credentials must come from the mounted directory")
	}
	var dir string
	for _, env := range container.Env {
		if env.Name == "HEARTBEAT_CREDENTIALS_DIR" {
			dir = env.Value
		}
	}
	var mounts []string
	for _, mount := range container.VolumeMounts {
		if mount.Name == "credentials" {
			mounts = append(mounts, mount.MountPath)
			if mount.MountPath != dir || mount.SubPath != "" || !mount.ReadOnly {
				t.Errorf("credentials mount at %q (subPath %q, readOnly %v); want a read-only directory at HEARTBEAT_CREDENTIALS_DIR (%q)",
					mount.MountPath, mount.SubPath, mount.ReadOnly, dir)
			}
		}
	}
	var volumes []string
	for _, volume := range spec.Template.Spec.Volumes {
		if volume.Name != "credentials" {
			continue
		}
		if volume.Secret == nil {
			t.Error("credentials volume is not a Secret")
			continue
		}
		volumes = append(volumes, volume.Secret.SecretName)
		// Readable by the pod's fsGroup only, and a missing Secret holds the
		// pod back like the envFrom it replaced.
		if volume.Secret.DefaultMode != 0o440 || volume.Secret.Optional {
			t.Errorf("credentials volume defaultMode %#o, optional %v; want 0440, not optional", volume.Secret.DefaultMode, volume.Secret.Optional)
		}
	}
	if secret == "" {
		if dir != "" || len(mounts) > 0 || len(volumes) > 0 {
			t.Errorf("no credentials Secret, but HEARTBEAT_CREDENTIALS_DIR %q, mounts %v, volumes %v", dir, mounts, volumes)
		}
		return
	}
	if dir == "" || len(mounts) != 1 || !slices.Equal(volumes, []string{secret}) {
		t.Errorf("credentials: HEARTBEAT_CREDENTIALS_DIR %q, mounts %v, Secret volumes %v; want one directory mount of %s", dir, mounts, volumes, secret)
	}
}

// helm install prints NOTES.txt. It must name the Secrets the release needs,
// which the chart never creates (ADR 0005), and how to check the install:
// v0.1.0 printed nothing, and a first install sat in
// CreateContainerConfigError with no hint why. helm template does not render
// NOTES, so this is a client-side dry run, which contacts no cluster.
func checkNotes(t *testing.T, profile chartProfile) {
	t.Helper()
	if len(profile.notes) == 0 && len(profile.notesAbsent) == 0 {
		return
	}
	out, stderr, err := runHelm(t, profile, "install", "--dry-run=client")
	if err != nil && strings.Contains(stderr, "cluster unreachable") && isHelm3(t) {
		// Helm 3 may look for a cluster even in a client-side dry run; the
		// Helm 4 run checks the notes.
		t.Log("NOTES not checked: Helm 3 cannot render them without a cluster")
		return
	}
	if err != nil {
		t.Fatalf("helm install --dry-run=client: %v\n%s", err, stderr)
	}
	_, notes, ok := strings.Cut(string(out), "\nNOTES:\n")
	if !ok {
		t.Fatalf("helm install printed no NOTES:\n%s", out)
	}
	for _, want := range profile.notes {
		if !strings.Contains(notes, want) {
			t.Errorf("NOTES do not contain %q:\n%s", want, notes)
		}
	}
	for _, unwanted := range profile.notesAbsent {
		if strings.Contains(notes, unwanted) {
			t.Errorf("NOTES contain %q:\n%s", unwanted, notes)
		}
	}
}

// checkConfigReloadAlert checks that the rules ConfigMap, when rendered,
// holds HeartbeatCollectorConfigReloadFailed unless off, and the chart's
// other alerts either way.
func checkConfigReloadAlert(t *testing.T, objects map[string]object, off bool) {
	t.Helper()
	rules, ok := objects["ConfigMap/heartbeat-prometheus-rules"]
	if !ok {
		return
	}
	alerts := map[string]bool{}
	for file, content := range rules.Data {
		var doc struct {
			Groups []struct {
				Rules []struct {
					Alert string `yaml:"alert"`
				} `yaml:"rules"`
			} `yaml:"groups"`
		}
		mustUnmarshal(t, content, &doc)
		for _, group := range doc.Groups {
			for _, rule := range group.Rules {
				if rule.Alert != "" {
					alerts[rule.Alert] = true
				}
			}
		}
		if file == "heartbeat-config-reload.rules.yml" && off {
			t.Errorf("rules ConfigMap holds %s with prometheusRules.configReloadAlert.enabled=false", file)
		}
	}
	if got := alerts["HeartbeatCollectorConfigReloadFailed"]; got == off {
		t.Errorf("HeartbeatCollectorConfigReloadFailed in the rules ConfigMap: %v, want %v", got, !off)
	}
	if !alerts["HeartbeatServiceDown"] || !alerts["Watchdog"] {
		t.Errorf("rules ConfigMap lost the chart's other alerts: %v", alerts)
	}
}

// checkCollectorMissingAlert checks that the rules ConfigMap holds the
// db-collector's missing-target alert exactly when the profile renders the
// db-collector and the bundled Prometheus scrapes it as job db-collector:
// absent() fires for good wherever nothing matches.
func checkCollectorMissingAlert(t *testing.T, objects map[string]object) {
	t.Helper()
	const file = "heartbeat-collector-missing.rules.yml"
	rules, ok := objects["ConfigMap/heartbeat-prometheus-rules"]
	if !ok {
		return
	}
	scraped := false
	if cm, ok := objects["ConfigMap/prometheus"]; ok {
		var prom struct {
			ScrapeConfigs []struct {
				JobName string `yaml:"job_name"`
			} `yaml:"scrape_configs"`
		}
		mustUnmarshal(t, cm.Data["prometheus.yml"], &prom)
		for _, job := range prom.ScrapeConfigs {
			scraped = scraped || job.JobName == "db-collector"
		}
	}
	_, collector := objects["StatefulSet/db-collector"]
	want := collector && scraped
	content, got := rules.Data[file]
	if got != want {
		t.Fatalf("rules ConfigMap holds %s: %v, want %v (db-collector rendered: %v, scraped by the bundled Prometheus: %v)",
			file, got, want, collector, scraped)
	}
	if got && !strings.Contains(content, `absent(up{job="db-collector"})`) {
		t.Errorf("%s does not alert on absent(up{job=\"db-collector\"}):\n%s", file, content)
	}
}

// No Role may read the namespace's Secrets: they hold the API's, SQL
// Server's and Grafana's credentials and the collector's admin token, and
// none of the chart's workloads reads them through the Kubernetes API.  The
// Loki subchart rendered one for its rules sidecar, which the chart does not
// run, and Loki mounts no service account token.
func checkNoSecretReaders(t *testing.T, objects map[string]object) {
	t.Helper()
	for id, obj := range objects {
		if obj.Kind != "Role" {
			continue
		}
		for _, rule := range obj.Rules {
			if slices.Contains(rule.Resources, "secrets") || slices.Contains(rule.Resources, "*") {
				t.Errorf("%s can read Secrets: %v", id, rule.Resources)
			}
		}
	}
	// A profile on an external platform renders no Loki.
	sa, ok := objects["ServiceAccount/loki"]
	if ok && (sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken) {
		t.Error("ServiceAccount/loki automounts its token; Loki never calls the Kubernetes API")
	}
}

// The rendered integrations.yaml must load with the same Go loader the
// services use.
func checkIntegrations(t *testing.T, objects map[string]object) {
	t.Helper()
	cm, ok := objects["ConfigMap/heartbeat-integrations"]
	if !ok {
		t.Fatal("missing ConfigMap/heartbeat-integrations")
	}
	path := filepath.Join(t.TempDir(), "integrations.yaml")
	if err := os.WriteFile(path, []byte(cm.Data["integrations.yaml"]), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadRuntimeConfig(path)
	if err != nil {
		t.Errorf("rendered integrations.yaml does not load: %v", err)
		return
	}
	// The API renders links from it (TODO 4.9): a template typo fails here.
	if err := config.ValidateLinkTemplates(cfg.Dashboards); err != nil {
		t.Errorf("rendered integrations.yaml has a bad link template: %v", err)
	}
}

// checkCollectorIDs checks the ids of the collectors in the rendered
// integrations.yaml, in order: the hand-written integrations.collectors, then
// managedCollectors (ADR 0014). nil skips the check.
func checkCollectorIDs(t *testing.T, objects map[string]object, want []string) {
	t.Helper()
	if want == nil {
		return
	}
	var doc struct {
		Collectors []struct {
			ID string `yaml:"id"`
		} `yaml:"collectors"`
	}
	mustUnmarshal(t, objects["ConfigMap/heartbeat-integrations"].Data["integrations.yaml"], &doc)
	got := []string{}
	for _, collector := range doc.Collectors {
		got = append(got, collector.ID)
	}
	if !slices.Equal(got, want) {
		t.Errorf("integrations.yaml collectors = %q, want %q", got, want)
	}
}

func checkCollectorSingleton(t *testing.T, objects map[string]object) {
	t.Helper()
	sts, ok := objects["StatefulSet/db-collector"]
	if !ok {
		return
	}
	var spec struct {
		Replicas int `yaml:"replicas"`
		Template struct {
			Spec struct {
				Containers []struct {
					Image string `yaml:"image"`
				} `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	}
	if err := sts.Spec.Decode(&spec); err != nil {
		t.Fatal(err)
	}
	if spec.Replicas != 1 {
		t.Errorf("db-collector replicas = %d, want 1 (singleton, ADR 0003)", spec.Replicas)
	}
	if image := spec.Template.Spec.Containers[0].Image; strings.HasSuffix(image, ":") {
		t.Errorf("db-collector image %q has no tag", image)
	}
}

// The one-replica StatefulSets must use the Parallel pod management policy.
// With OrderedReady the controller never replaces a pod that did not become
// Ready (kubernetes/kubernetes#67250): after a bad image or config, the
// corrected upgrade waits for the broken pod forever and needs a manual pod
// delete. Alertmanager's chart has no setting for it and keeps OrderedReady.
func checkStatefulSetPolicies(t *testing.T, objects map[string]object) {
	t.Helper()
	for _, name := range []string{"db-collector", "otel-collector", "loki", "postgresql", "valkey"} {
		sts, ok := objects["StatefulSet/"+name]
		if !ok {
			continue
		}
		var spec struct {
			PodManagementPolicy string `yaml:"podManagementPolicy"`
			UpdateStrategy      struct {
				Type string `yaml:"type"`
			} `yaml:"updateStrategy"`
		}
		if err := sts.Spec.Decode(&spec); err != nil {
			t.Fatal(err)
		}
		if spec.PodManagementPolicy != "Parallel" {
			t.Errorf("StatefulSet/%s podManagementPolicy = %q, want Parallel", name, spec.PodManagementPolicy)
		}
		if spec.UpdateStrategy.Type != "" && spec.UpdateStrategy.Type != "RollingUpdate" {
			t.Errorf("StatefulSet/%s updateStrategy = %q, want RollingUpdate", name, spec.UpdateStrategy.Type)
		}
	}
}

// The collector's GOMEMLIMIT keeps the Go heap below the container's memory
// limit, so a large outsystems_logs backlog is collected instead of
// OOM-killing the pod, which would read the same batch again on restart.
func checkCollectorGoMemLimit(t *testing.T, objects map[string]object, want string) {
	t.Helper()
	sts, ok := objects["StatefulSet/db-collector"]
	if !ok {
		return
	}
	var spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Env []struct {
						Name  string `yaml:"name"`
						Value string `yaml:"value"`
					} `yaml:"env"`
				} `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	}
	if err := sts.Spec.Decode(&spec); err != nil {
		t.Fatal(err)
	}
	if want == "" {
		want = "204MiB"
	}
	got := "-"
	for _, env := range spec.Template.Spec.Containers[0].Env {
		if env.Name == "GOMEMLIMIT" {
			got = env.Value
		}
	}
	if got != want {
		t.Errorf("db-collector GOMEMLIMIT = %q, want %q", got, want)
	}
}

// Every in-cluster address the engines are configured with (Prometheus scrape
// and Alertmanager targets, Alertmanager webhooks, Grafana datasources, the OTel
// Collector's exporters and scrape targets) must be
// a Service the same profile renders. Catches a profile that disables a
// component but still points at it.
func checkInClusterEndpoints(t *testing.T, objects map[string]object) {
	t.Helper()
	services := map[string]bool{}
	for _, obj := range objects {
		if obj.Kind == "Service" {
			services[obj.Metadata.Name] = true
		}
	}
	var hosts []string
	if cm, ok := objects["ConfigMap/prometheus"]; ok {
		var prom struct {
			ScrapeConfigs []struct {
				JobName       string `yaml:"job_name"`
				StaticConfigs []struct {
					Targets []string `yaml:"targets"`
				} `yaml:"static_configs"`
				DNSSDConfigs []struct {
					Names []string `yaml:"names"`
				} `yaml:"dns_sd_configs"`
			} `yaml:"scrape_configs"`
			Alerting struct {
				Alertmanagers []struct {
					StaticConfigs []struct {
						Targets []string `yaml:"targets"`
					} `yaml:"static_configs"`
				} `yaml:"alertmanagers"`
			} `yaml:"alerting"`
		}
		mustUnmarshal(t, cm.Data["prometheus.yml"], &prom)
		if len(prom.Alerting.Alertmanagers) == 0 {
			t.Error("Prometheus has no alerting.alertmanagers")
		}
		for _, job := range prom.ScrapeConfigs {
			for _, sc := range job.StaticConfigs {
				hosts = append(hosts, sc.Targets...)
			}
			for _, sd := range job.DNSSDConfigs {
				hosts = append(hosts, sd.Names...)
			}
		}
		for _, am := range prom.Alerting.Alertmanagers {
			for _, sc := range am.StaticConfigs {
				hosts = append(hosts, sc.Targets...)
			}
		}
	}
	if cm, ok := objects["ConfigMap/alertmanager"]; ok {
		var am struct {
			Receivers []struct {
				WebhookConfigs []struct {
					URL string `yaml:"url"`
				} `yaml:"webhook_configs"`
			} `yaml:"receivers"`
		}
		mustUnmarshal(t, cm.Data["alertmanager.yml"], &am)
		for _, r := range am.Receivers {
			for _, w := range r.WebhookConfigs {
				if w.URL != "" {
					hosts = append(hosts, w.URL)
				}
			}
		}
	}
	if cm, ok := objects["ConfigMap/grafana"]; ok {
		var ds struct {
			Datasources []struct {
				URL string `yaml:"url"`
			} `yaml:"datasources"`
		}
		mustUnmarshal(t, cm.Data["datasources.yaml"], &ds)
		for _, d := range ds.Datasources {
			hosts = append(hosts, d.URL)
		}
	}
	if config, ok := collectorRelay(t, objects); ok {
		var relay struct {
			Receivers struct {
				Prometheus struct {
					Config struct {
						ScrapeConfigs []struct {
							DNSSDConfigs []struct {
								Names []string `yaml:"names"`
							} `yaml:"dns_sd_configs"`
						} `yaml:"scrape_configs"`
					} `yaml:"config"`
				} `yaml:"prometheus"`
			} `yaml:"receivers"`
			Exporters map[string]struct {
				Endpoint string `yaml:"endpoint"`
			} `yaml:"exporters"`
		}
		mustUnmarshal(t, config, &relay)
		for _, job := range relay.Receivers.Prometheus.Config.ScrapeConfigs {
			for _, sd := range job.DNSSDConfigs {
				hosts = append(hosts, sd.Names...)
			}
		}
		for _, exporter := range relay.Exporters {
			// ${env:...} is set from a Secret; the pod's own address is local.
			if strings.HasPrefix(exporter.Endpoint, "http") {
				hosts = append(hosts, exporter.Endpoint)
			}
		}
	}
	for _, raw := range hosts {
		host := hostOf(raw)
		// Dotted names are external or fully qualified; localhost is the pod itself.
		if host == "localhost" || strings.Contains(host, ".") {
			continue
		}
		if !services[host] {
			t.Errorf("configured address %q points at Service %q, which this profile does not render", raw, host)
		}
	}
}

// The OTel Collector runs exactly Heartbeat's config in every profile: only
// the profile's receivers (the two OTLP receivers unless it says otherwise),
// and the logs, Heartbeat's own logs, metrics and traces pipelines. Values merged over the
// subchart's defaults once brought back its other receivers and a traces
// pipeline in profiles that did not set otelCollector keys.
func checkCollectorPipelines(t *testing.T, objects map[string]object, receivers []string) {
	t.Helper()
	if receivers == nil {
		receivers = []string{"otlp", "otlp/self"}
	}
	config, ok := collectorRelay(t, objects)
	if !ok {
		return
	}
	var relay struct {
		Receivers map[string]any `yaml:"receivers"`
		Service   struct {
			Pipelines map[string]any `yaml:"pipelines"`
		} `yaml:"service"`
	}
	mustUnmarshal(t, config, &relay)
	if got := slices.Sorted(maps.Keys(relay.Receivers)); !slices.Equal(got, receivers) {
		t.Errorf("OTel Collector receivers = %v, want %v", got, receivers)
	}
	if got := slices.Sorted(maps.Keys(relay.Service.Pipelines)); !slices.Equal(got, []string{"logs", "logs/self", "metrics", "traces"}) {
		t.Errorf("OTel Collector pipelines = %v, want [logs logs/self metrics traces]", got)
	}
}

// Heartbeat's own logs stay off the OutSystems delivery path: their own
// receiver, exporter and memory queue, so they neither wait for that path's
// one-request queue nor take a place in it (ADR 0018). The services send them
// to that receiver's port, never 4318, and the platform that gets the OutSystems
// logs gets these too: an example that repoints one exporter must repoint both.
func checkSelfLogs(t *testing.T, objects map[string]object, off bool) {
	t.Helper()
	endpoint, hasEndpoint := collectorEnv(t, objects, "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT")
	config, ok := collectorRelay(t, objects)
	if !ok {
		if hasEndpoint {
			t.Errorf("no OTel Collector, yet the collector sends its logs to %s", endpoint)
		}
		return
	}
	want := "http://otel-collector:4319/v1/logs"
	if off {
		want = ""
	}
	if endpoint != want {
		t.Errorf("collector OTEL_EXPORTER_OTLP_LOGS_ENDPOINT = %q, want %q", endpoint, want)
	}
	wantTraces := "http://otel-collector:4318/v1/traces"
	if off {
		wantTraces = ""
	}
	if got, _ := collectorEnv(t, objects, "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"); got != wantTraces {
		t.Errorf("collector OTEL_EXPORTER_OTLP_TRACES_ENDPOINT = %q, want %q", got, wantTraces)
	}
	if got, _ := collectorEnv(t, objects, "OTEL_TRACES_SAMPLER"); !off && got != "parentbased_traceidratio" {
		t.Errorf("collector OTEL_TRACES_SAMPLER = %q, want parentbased_traceidratio", got)
	}
	var relay struct {
		Exporters map[string]struct {
			Endpoint     string `yaml:"endpoint"`
			SendingQueue struct {
				Storage         string `yaml:"storage"`
				BlockOnOverflow bool   `yaml:"block_on_overflow"`
			} `yaml:"sending_queue"`
		} `yaml:"exporters"`
		Service struct {
			Pipelines map[string]struct {
				Receivers []string `yaml:"receivers"`
				Exporters []string `yaml:"exporters"`
			} `yaml:"pipelines"`
		} `yaml:"service"`
	}
	mustUnmarshal(t, config, &relay)
	if self, logs := relay.Exporters["otlp_http/self-logs"].Endpoint, relay.Exporters["otlp_http/logs"].Endpoint; self != logs {
		t.Errorf("otlp_http/self-logs goes to %q, otlp_http/logs to %q", self, logs)
	}
	self, logs := relay.Service.Pipelines["logs/self"], relay.Service.Pipelines["logs"]
	if !slices.Equal(self.Receivers, []string{"otlp/self"}) {
		t.Errorf("logs/self receivers = %v, want [otlp/self]", self.Receivers)
	}
	if slices.Contains(logs.Receivers, "otlp/self") {
		t.Errorf("logs receivers = %v, must not include otlp/self", logs.Receivers)
	}
	for _, name := range self.Exporters {
		if slices.Contains(logs.Exporters, name) {
			t.Errorf("exporter %s serves both logs pipelines", name)
		}
		if q := relay.Exporters[name].SendingQueue; q.Storage != "" || q.BlockOnOverflow {
			t.Errorf("%s sending_queue storage %q, block_on_overflow %t; want a queue in memory that never waits", name, q.Storage, q.BlockOnOverflow)
		}
	}
}

// collectorEnv returns a variable of the db-collector container, if it is set.
func collectorEnv(t *testing.T, objects map[string]object, name string) (string, bool) {
	t.Helper()
	sts, ok := objects["StatefulSet/db-collector"]
	if !ok {
		return "", false
	}
	var spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Env []struct {
						Name  string `yaml:"name"`
						Value string `yaml:"value"`
					} `yaml:"env"`
				} `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	}
	if err := sts.Spec.Decode(&spec); err != nil {
		t.Fatal(err)
	}
	for _, env := range spec.Template.Spec.Containers[0].Env {
		if env.Name == name {
			return env.Value, true
		}
	}
	return "", false
}

// lokiIngestionBurst is Loki's default ingestion_burst_size_mb, in bytes:
// a larger push is never accepted.
const lokiIngestionBurst = 6 << 20

// The logs pipeline must deliver a backlog to Loki in order and lose nothing
// the db-collector counted delivered: no batch processor, which accepts rows
// before they are queued, one export consumer, no retry deadline, and
// batches below Loki's ingestion burst.  The queue holds one request and the
// receiver waits for room in it: a larger queue sends the requests it was
// sending at a restart after the rest, out of order.
func checkLogDelivery(t *testing.T, objects map[string]object) {
	t.Helper()
	config, ok := collectorRelay(t, objects)
	if !ok {
		return
	}
	var relay struct {
		Exporters map[string]struct {
			SendingQueue struct {
				NumConsumers    int    `yaml:"num_consumers"`
				Sizer           string `yaml:"sizer"`
				QueueSize       int    `yaml:"queue_size"`
				BlockOnOverflow bool   `yaml:"block_on_overflow"`
				Batch           struct {
					Sizer   string `yaml:"sizer"`
					MinSize *int   `yaml:"min_size"`
					MaxSize int    `yaml:"max_size"`
				} `yaml:"batch"`
			} `yaml:"sending_queue"`
			RetryOnFailure struct {
				MaxElapsedTime string `yaml:"max_elapsed_time"`
			} `yaml:"retry_on_failure"`
		} `yaml:"exporters"`
		Service struct {
			Pipelines map[string]struct {
				Processors []string `yaml:"processors"`
				Exporters  []string `yaml:"exporters"`
			} `yaml:"pipelines"`
		} `yaml:"service"`
	}
	mustUnmarshal(t, config, &relay)
	logs := relay.Service.Pipelines["logs"]
	if slices.ContainsFunc(logs.Processors, func(p string) bool { return p == "batch" || strings.HasPrefix(p, "batch/") }) {
		t.Errorf("logs pipeline processors = %v, want no batch processor", logs.Processors)
	}
	for _, name := range logs.Exporters {
		if name == "debug" {
			continue
		}
		e := relay.Exporters[name]
		q := e.SendingQueue
		if q.NumConsumers != 1 {
			t.Errorf("%s sending_queue.num_consumers = %d, want 1", name, q.NumConsumers)
		}
		if q.Sizer != "requests" || q.QueueSize != 1 || !q.BlockOnOverflow {
			t.Errorf("%s sending_queue sizer %q, queue_size %d, block_on_overflow %t; want one request, the receiver waiting for room", name, q.Sizer, q.QueueSize, q.BlockOnOverflow)
		}
		if q.Batch.Sizer != "bytes" || q.Batch.MaxSize <= 0 || q.Batch.MaxSize >= lokiIngestionBurst {
			t.Errorf("%s sending_queue.batch = %+v, want sizer bytes and max_size below %d", name, q.Batch, lokiIngestionBurst)
		}
		if q.Batch.MinSize == nil || *q.Batch.MinSize != 0 {
			t.Errorf("%s sending_queue.batch.min_size is not 0: with one request queued, a small request would wait flush_timeout", name)
		}
		if d, err := time.ParseDuration(e.RetryOnFailure.MaxElapsedTime); err != nil || d != 0 {
			t.Errorf("%s retry_on_failure.max_elapsed_time = %q, want 0s (no deadline)", name, e.RetryOnFailure.MaxElapsedTime)
		}
	}
}

// collectorWorkload and collectorConfigMap are the OTel Collector's objects:
// a StatefulSet, whose ConfigMap the subchart names otel-collector-statefulset.
const (
	collectorWorkload  = "StatefulSet/otel-collector"
	collectorConfigMap = "ConfigMap/otel-collector-statefulset"
)

// collectorRelay returns the OTel Collector's config, and false when the
// profile renders no Collector. A Collector without that ConfigMap fails the
// test, so the Collector checks never pass by finding nothing to check.
func collectorRelay(t *testing.T, objects map[string]object) (string, bool) {
	t.Helper()
	cm, ok := objects[collectorConfigMap]
	if _, running := objects[collectorWorkload]; running && !ok {
		t.Errorf("%s renders without %s", collectorWorkload, collectorConfigMap)
	}
	return cm.Data["relay"], ok
}

// exportQueueClaim is the chart's claim for the Collector's export queue.
const exportQueueClaim = "otel-collector-queue"

// The logs exporters keep their sending queue on disk, so rows the
// db-collector counted delivered survive a Collector restart: the storage
// extension is a file_storage that syncs every write, the Collector runs it,
// and its directory is the export-queue volume, the otel-collector-queue
// claim (an emptyDir only when the profile turns persistence off). One
// Collector pod at a time, as a one-replica StatefulSet, opens the queue.
func checkExportQueue(t *testing.T, objects map[string]object, emptyDir bool) {
	t.Helper()
	if _, ok := objects["Deployment/otel-collector"]; ok {
		t.Error("the OTel Collector is a Deployment; its export queue needs the at-most-one pod of a StatefulSet")
	}
	config, ok := collectorRelay(t, objects)
	if !ok {
		return
	}
	var relay struct {
		Extensions map[string]struct {
			Directory string `yaml:"directory"`
			FSync     bool   `yaml:"fsync"`
		} `yaml:"extensions"`
		Exporters map[string]struct {
			SendingQueue struct {
				Storage string `yaml:"storage"`
			} `yaml:"sending_queue"`
		} `yaml:"exporters"`
		Service struct {
			Extensions []string `yaml:"extensions"`
			Pipelines  map[string]struct {
				Exporters []string `yaml:"exporters"`
			} `yaml:"pipelines"`
		} `yaml:"service"`
	}
	mustUnmarshal(t, config, &relay)
	directories := map[string]bool{}
	for _, name := range relay.Service.Pipelines["logs"].Exporters {
		if name == "debug" {
			continue
		}
		storage := relay.Exporters[name].SendingQueue.Storage
		ext, defined := relay.Extensions[storage]
		switch {
		case storage == "":
			t.Errorf("%s sending_queue has no storage; its queue is in memory and a restart loses it", name)
		case storage != "file_storage" && !strings.HasPrefix(storage, "file_storage/"):
			t.Errorf("%s sending_queue.storage = %q, want a file_storage extension", name, storage)
		case !defined:
			t.Errorf("%s sending_queue.storage %q is not among the extensions", name, storage)
		case !slices.Contains(relay.Service.Extensions, storage):
			t.Errorf("%s sending_queue.storage %q is not in service.extensions %v; the Collector would not start", name, storage, relay.Service.Extensions)
		case ext.Directory == "":
			t.Errorf("%s has no directory", storage)
		case !ext.FSync:
			t.Errorf("%s fsync is off; a node crash could lose or corrupt queued rows", storage)
		default:
			directories[ext.Directory] = true
		}
	}

	type volume struct {
		Name                  string    `yaml:"name"`
		EmptyDir              *struct{} `yaml:"emptyDir"`
		PersistentVolumeClaim *struct {
			ClaimName string `yaml:"claimName"`
		} `yaml:"persistentVolumeClaim"`
	}
	type volumeMount struct {
		Name      string `yaml:"name"`
		MountPath string `yaml:"mountPath"`
		ReadOnly  bool   `yaml:"readOnly"`
	}
	var spec struct {
		Replicas int `yaml:"replicas"`
		Template struct {
			Spec struct {
				SecurityContext struct {
					FSGroup int `yaml:"fsGroup"`
				} `yaml:"securityContext"`
				Containers []struct {
					VolumeMounts []volumeMount `yaml:"volumeMounts"`
				} `yaml:"containers"`
				Volumes []volume `yaml:"volumes"`
			} `yaml:"spec"`
		} `yaml:"template"`
		VolumeClaimTemplates []any `yaml:"volumeClaimTemplates"`
	}
	sts, ok := objects[collectorWorkload]
	if !ok {
		t.Fatalf("%s renders without %s", collectorConfigMap, collectorWorkload)
	}
	if err := sts.Spec.Decode(&spec); err != nil {
		t.Fatal(err)
	}
	if spec.Replicas != 1 {
		t.Errorf("%s replicas = %d, want 1: replicas would share neither the queue nor its order", collectorWorkload, spec.Replicas)
	}
	if len(spec.VolumeClaimTemplates) > 0 {
		t.Errorf("%s has volumeClaimTemplates; they can never change on an existing StatefulSet", collectorWorkload)
	}
	pod := spec.Template.Spec
	if pod.SecurityContext.FSGroup == 0 {
		t.Errorf("%s pod has no fsGroup; the Collector's non-root user may not write the queue volume", collectorWorkload)
	}
	for directory := range directories {
		mounted := slices.ContainsFunc(pod.Containers[0].VolumeMounts, func(m volumeMount) bool {
			return m.Name == "export-queue" && m.MountPath == directory && !m.ReadOnly
		})
		if !mounted {
			t.Errorf("the queue directory %s is not the writable export-queue volume", directory)
		}
	}
	hasEmptyDir := slices.ContainsFunc(pod.Volumes, func(v volume) bool { return v.Name == "export-queue" && v.EmptyDir != nil })
	usesClaim := slices.ContainsFunc(pod.Volumes, func(v volume) bool {
		return v.Name == "export-queue" && v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == exportQueueClaim
	})
	pvc, hasClaim := objects["PersistentVolumeClaim/"+exportQueueClaim]
	switch {
	case emptyDir && (!hasEmptyDir || usesClaim || hasClaim):
		t.Errorf("export queue: emptyDir %v, claim %v, mounted %v; want an emptyDir only", hasEmptyDir, hasClaim, usesClaim)
	case !emptyDir && (hasEmptyDir || !usesClaim || !hasClaim):
		t.Errorf("export queue: emptyDir %v, claim %v, mounted %v; want the %s claim only", hasEmptyDir, hasClaim, usesClaim, exportQueueClaim)
	case !emptyDir:
		var claim struct {
			AccessModes []string `yaml:"accessModes"`
			Resources   struct {
				Requests map[string]string `yaml:"requests"`
			} `yaml:"resources"`
		}
		if err := pvc.Spec.Decode(&claim); err != nil {
			t.Fatal(err)
		}
		if size := claim.Resources.Requests["storage"]; size != "1Gi" {
			t.Errorf("export queue claim requests %s, want 1Gi", size)
		}
		if !slices.Equal(claim.AccessModes, []string{"ReadWriteOnce"}) {
			t.Errorf("export queue claim access modes = %v, want [ReadWriteOnce]", claim.AccessModes)
		}
	}
}

// logIdentifiers are the log attributes session investigations correlate on.
var logIdentifiers = []string{"user_id", "session_id", "request_id", "client_ip"}

// The OTel Collector must pass the log identifiers through to Loki: no
// processor action may delete or hash them.
func checkLogIdentifiers(t *testing.T, objects map[string]object) {
	t.Helper()
	config, ok := collectorRelay(t, objects)
	if !ok {
		return
	}
	type action struct {
		Key    string `yaml:"key"`
		Action string `yaml:"action"`
	}
	var relay struct {
		Processors map[string]struct {
			Actions    []action `yaml:"actions"`
			Attributes []action `yaml:"attributes"`
		} `yaml:"processors"`
	}
	mustUnmarshal(t, config, &relay)
	for name, processor := range relay.Processors {
		for _, a := range append(processor.Actions, processor.Attributes...) {
			if slices.Contains(logIdentifiers, a.Key) && (a.Action == "delete" || a.Action == "hash") {
				t.Errorf("OTel Collector processor %s %ss %s; Loki keeps it as structured metadata, not a label", name, a.Action, a.Key)
			}
		}
	}
}

// Loki indexes only the agreed OTLP resource attributes; everything else,
// including the log identifiers, stays structured metadata.
func checkLokiLabelPolicy(t *testing.T, objects map[string]object) {
	t.Helper()
	cm, ok := objects["ConfigMap/loki"]
	if !ok {
		return
	}
	var config struct {
		LimitsConfig struct {
			AllowStructuredMetadata bool `yaml:"allow_structured_metadata"`
			MaxQuerySeries          int  `yaml:"max_query_series"`
			OTLPConfig              struct {
				ResourceAttributes struct {
					IgnoreDefaults   bool `yaml:"ignore_defaults"`
					AttributesConfig []struct {
						Action     string   `yaml:"action"`
						Attributes []string `yaml:"attributes"`
					} `yaml:"attributes_config"`
				} `yaml:"resource_attributes"`
			} `yaml:"otlp_config"`
		} `yaml:"limits_config"`
	}
	mustUnmarshal(t, cm.Data["config.yaml"], &config)
	limits := config.LimitsConfig
	if !limits.AllowStructuredMetadata {
		t.Error("Loki limits_config.allow_structured_metadata is not true")
	}
	// The active users and sessions panels hold one series per distinct ID.
	if limits.MaxQuerySeries < 10000 {
		t.Errorf("Loki limits_config.max_query_series is %d; the active sessions count needs at least 10000", limits.MaxQuerySeries)
	}
	resource := limits.OTLPConfig.ResourceAttributes
	if !resource.IgnoreDefaults {
		t.Error("Loki otlp_config.resource_attributes.ignore_defaults is not true; Loki's default label list would apply")
	}
	var labels []string
	for _, entry := range resource.AttributesConfig {
		if entry.Action == "index_label" {
			labels = append(labels, entry.Attributes...)
		}
	}
	sort.Strings(labels)
	if want := []string{"deployment.environment.name", "outsystems.log_type", "service.name", "service.namespace"}; !reflect.DeepEqual(labels, want) {
		t.Errorf("Loki OTLP index labels = %q, want %q", labels, want)
	}
}

// Loki holds personal data (ADR 0012), so its compactor deletes logs after
// 30 days, the period compliance signs off on [9.2]. Loki refuses to start
// with retention on and no delete request store; the store must be the one
// that holds the chunks, or production's delete requests would live on the
// pod's disk instead of S3.
func checkLokiRetention(t *testing.T, objects map[string]object) {
	t.Helper()
	cm, ok := objects["ConfigMap/loki"]
	if !ok {
		return
	}
	var config struct {
		Compactor struct {
			RetentionEnabled   bool   `yaml:"retention_enabled"`
			DeleteRequestStore string `yaml:"delete_request_store"`
		} `yaml:"compactor"`
		LimitsConfig struct {
			RetentionPeriod string `yaml:"retention_period"`
		} `yaml:"limits_config"`
		SchemaConfig struct {
			Configs []struct {
				ObjectStore string `yaml:"object_store"`
			} `yaml:"configs"`
		} `yaml:"schema_config"`
	}
	mustUnmarshal(t, cm.Data["config.yaml"], &config)
	if !config.Compactor.RetentionEnabled {
		t.Error("Loki compactor.retention_enabled is not true; logs are never deleted")
	}
	if got := config.LimitsConfig.RetentionPeriod; got != "30d" {
		t.Errorf("Loki limits_config.retention_period is %q, want 30d", got)
	}
	if n := len(config.SchemaConfig.Configs); n == 0 {
		t.Error("Loki schema_config has no configs")
	} else if store := config.SchemaConfig.Configs[n-1].ObjectStore; config.Compactor.DeleteRequestStore != store {
		t.Errorf("Loki compactor.delete_request_store is %q, want the chunks' object_store %q", config.Compactor.DeleteRequestStore, store)
	}
}

// Tempo's WAL must be on a writable volume wherever its traces are stored:
// the root filesystem is read-only, and a WAL there would not outlive the pod.
// Moving the traces to S3 by turning persistence off once left it nowhere.
func checkTempoWAL(t *testing.T, objects map[string]object) {
	t.Helper()
	sts, ok := objects["StatefulSet/tempo"]
	if !ok {
		return
	}
	var config struct {
		Storage struct {
			Trace struct {
				WAL struct {
					Path string `yaml:"path"`
				} `yaml:"wal"`
			} `yaml:"trace"`
		} `yaml:"storage"`
	}
	mustUnmarshal(t, objects["ConfigMap/tempo"].Data["tempo.yaml"], &config)
	wal := config.Storage.Trace.WAL.Path
	if wal == "" {
		t.Fatal("Tempo has no storage.trace.wal.path")
	}
	type volumeMount struct {
		MountPath string `yaml:"mountPath"`
		ReadOnly  bool   `yaml:"readOnly"`
	}
	var spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Name         string        `yaml:"name"`
					VolumeMounts []volumeMount `yaml:"volumeMounts"`
				} `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	}
	if err := sts.Spec.Decode(&spec); err != nil {
		t.Fatal(err)
	}
	for _, container := range spec.Template.Spec.Containers {
		if container.Name != "tempo" {
			continue
		}
		mounted := slices.ContainsFunc(container.VolumeMounts, func(m volumeMount) bool {
			return !m.ReadOnly && (wal == m.MountPath || strings.HasPrefix(wal, strings.TrimSuffix(m.MountPath, "/")+"/"))
		})
		if !mounted {
			t.Errorf("Tempo's WAL %s is on no writable volume", wal)
		}
		return
	}
	t.Error("StatefulSet/tempo has no tempo container")
}

func hostOf(raw string) string {
	if !strings.Contains(raw, "://") {
		raw = "tcp://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Hostname()
}

func mustUnmarshal(t *testing.T, content string, out any) {
	t.Helper()
	if err := yaml.Unmarshal([]byte(content), out); err != nil {
		t.Fatal(fmt.Errorf("parse embedded config: %w", err))
	}
}

// labelSelector is a Kubernetes label selector.
type labelSelector struct {
	MatchLabels      map[string]string `yaml:"matchLabels"`
	MatchExpressions []any             `yaml:"matchExpressions"`
}

// String renders the selector's labels as {k=v,...}, sorted by key.
func (s labelSelector) String() string {
	pairs := make([]string, 0, len(s.MatchLabels))
	for key, value := range s.MatchLabels {
		pairs = append(pairs, key+"="+value)
	}
	sort.Strings(pairs)
	if len(s.MatchExpressions) > 0 {
		pairs = append(pairs, fmt.Sprintf("expressions=%v", s.MatchExpressions))
	}
	return "{" + strings.Join(pairs, ",") + "}"
}

// networkPolicyPeer is one entry of a NetworkPolicy ingress rule's from.
type networkPolicyPeer struct {
	PodSelector       *labelSelector `yaml:"podSelector"`
	NamespaceSelector *labelSelector `yaml:"namespaceSelector"`
	IPBlock           *struct {
		CIDR   string   `yaml:"cidr"`
		Except []string `yaml:"except"`
	} `yaml:"ipBlock"`
}

// peerString renders a peer for comparison: "ns{...} pods{...}" for
// selectors, "ip <cidr> except <cidr>,..." for an ipBlock.
func peerString(peer networkPolicyPeer) string {
	var parts []string
	if peer.NamespaceSelector != nil {
		parts = append(parts, "ns"+peer.NamespaceSelector.String())
	}
	if peer.PodSelector != nil {
		parts = append(parts, "pods"+peer.PodSelector.String())
	}
	if peer.IPBlock != nil {
		block := "ip " + peer.IPBlock.CIDR
		if len(peer.IPBlock.Except) > 0 {
			block += " except " + strings.Join(peer.IPBlock.Except, ",")
		}
		parts = append(parts, block)
	}
	return strings.Join(parts, " ")
}

// Grafana's response gzip allocates buffers per CPU on every request, health
// probes included; with it on, a dashboard on a 5 s refresh OOM-killed the
// pod at its 512Mi limit.
func checkGrafanaGzip(t *testing.T, objects map[string]object) {
	t.Helper()
	cm, ok := objects["ConfigMap/grafana"]
	if !ok {
		return
	}
	section := ""
	for _, line := range strings.Split(cm.Data["grafana.ini"], "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			section = line
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if found && section == "[server]" && strings.TrimSpace(key) == "enable_gzip" {
			if strings.TrimSpace(value) != "false" {
				t.Errorf("Grafana server.enable_gzip = %s, want false", strings.TrimSpace(value))
			}
			return
		}
	}
	t.Error("Grafana grafana.ini does not set server.enable_gzip = false; Grafana 13 gzips by default")
}

// checkCollectorEgress checks what the db-collector NetworkPolicy lets the
// collector reach: nothing limited when want is nil, else exactly want, in
// "TCP/1433 -> peerString" form, and never an empty `to`, which would allow
// every destination. A port entry without a port is "TCP/any", and a rule
// without ports "any", since both allow every port.
func checkCollectorEgress(t *testing.T, objects map[string]object, want []string) {
	t.Helper()
	policy, ok := objects["NetworkPolicy/db-collector"]
	if !ok {
		return
	}
	var spec struct {
		PolicyTypes []string `yaml:"policyTypes"`
		Egress      *[]struct {
			Ports []struct {
				Protocol string `yaml:"protocol"`
				// A number or a named port.
				Port string `yaml:"port"`
			} `yaml:"ports"`
			To []networkPolicyPeer `yaml:"to"`
		} `yaml:"egress"`
	}
	if err := policy.Spec.Decode(&spec); err != nil {
		t.Fatal(err)
	}
	limits := slices.Contains(spec.PolicyTypes, "Egress")
	if want == nil {
		if limits || spec.Egress != nil {
			t.Errorf("db-collector NetworkPolicy limits egress (%v), want it off", spec.PolicyTypes)
		}
		return
	}
	if !limits || spec.Egress == nil {
		t.Fatalf("db-collector NetworkPolicy does not limit egress: policyTypes %v", spec.PolicyTypes)
	}
	var got []string
	for _, rule := range *spec.Egress {
		if len(rule.To) == 0 {
			t.Fatal("db-collector NetworkPolicy egress rule has an empty to, which allows every destination")
		}
		ports := []string{"any"}
		if len(rule.Ports) > 0 {
			ports = nil
			for _, port := range rule.Ports {
				ports = append(ports, cmp.Or(port.Protocol, "TCP")+"/"+cmp.Or(port.Port, "any"))
			}
		}
		for _, port := range ports {
			for _, peer := range rule.To {
				got = append(got, port+" -> "+peerString(peer))
			}
		}
	}
	slices.Sort(got)
	wanted := slices.Sorted(slices.Values(want))
	if !slices.Equal(got, wanted) {
		t.Errorf("db-collector NetworkPolicy egress = %q, want %q", got, wanted)
	}
}

// The db-collector NetworkPolicy must select exactly the collector pod, admit
// only the expected peers to TCP 8082 and never render an empty `from`
// (which admits every source). The bundled Prometheus peer must match the
// rendered Prometheus Deployment's selector, so a subchart label change cannot
// silently cut off scraping.
func checkCollectorNetworkPolicy(t *testing.T, objects map[string]object, wantPeers []string) {
	t.Helper()
	// The API reads GET /admin/config whenever it is deployed: its pods are
	// the last peer.
	if api, ok := objects["Deployment/api"]; ok {
		var apiSpec struct {
			Selector labelSelector `yaml:"selector"`
		}
		if err := api.Spec.Decode(&apiSpec); err != nil {
			t.Fatal(err)
		}
		wantPeers = append(slices.Clone(wantPeers), "pods"+apiSpec.Selector.String())
	}
	policy, ok := objects["NetworkPolicy/db-collector"]
	if !ok {
		return
	}
	var spec struct {
		PodSelector labelSelector `yaml:"podSelector"`
		PolicyTypes []string      `yaml:"policyTypes"`
		Ingress     *[]struct {
			Ports []struct {
				Protocol string `yaml:"protocol"`
				Port     int    `yaml:"port"`
			} `yaml:"ports"`
			From *[]networkPolicyPeer `yaml:"from"`
		} `yaml:"ingress"`
	}
	if err := policy.Spec.Decode(&spec); err != nil {
		t.Fatal(err)
	}
	if strings.Join(spec.PolicyTypes, ",") != "Ingress" && strings.Join(spec.PolicyTypes, ",") != "Ingress,Egress" {
		t.Errorf("db-collector NetworkPolicy policyTypes = %v, want [Ingress] or [Ingress Egress]", spec.PolicyTypes)
	}
	if sts, ok := objects["StatefulSet/db-collector"]; ok {
		var stsSpec struct {
			Selector labelSelector `yaml:"selector"`
		}
		if err := sts.Spec.Decode(&stsSpec); err != nil {
			t.Fatal(err)
		}
		if got, want := spec.PodSelector.String(), stsSpec.Selector.String(); got != want {
			t.Errorf("db-collector NetworkPolicy selects %s, StatefulSet selects %s", got, want)
		}
	}
	if spec.Ingress == nil {
		t.Fatal("db-collector NetworkPolicy has no ingress field; render ingress: [] to deny all")
	}
	var gotPeers []string
	for _, rule := range *spec.Ingress {
		if len(rule.Ports) != 1 || rule.Ports[0].Port != 8082 || rule.Ports[0].Protocol != "TCP" {
			t.Errorf("db-collector NetworkPolicy rule ports = %+v, want only TCP 8082", rule.Ports)
		}
		if rule.From == nil || len(*rule.From) == 0 {
			t.Fatal("db-collector NetworkPolicy rule has an empty from, which admits every source")
		}
		for _, peer := range *rule.From {
			gotPeers = append(gotPeers, peerString(peer))
		}
	}
	if !reflect.DeepEqual(gotPeers, wantPeers) {
		t.Errorf("db-collector NetworkPolicy peers = %q, want %q", gotPeers, wantPeers)
	}
	if prom, ok := objects["Deployment/prometheus"]; ok {
		var promSpec struct {
			Selector labelSelector `yaml:"selector"`
		}
		if err := prom.Spec.Decode(&promSpec); err != nil {
			t.Fatal(err)
		}
		if bundled := "pods" + promSpec.Selector.String(); !slices.Contains(gotPeers, bundled) {
			t.Errorf("db-collector NetworkPolicy does not admit the bundled Prometheus (%s); peers %q", bundled, gotPeers)
		}
	}
	checkCollectorSourceAddresses(t, objects, gotPeers)
}

// backendWorkloads are the workloads each engine NetworkPolicy protects.
var backendWorkloads = map[string]string{
	"otel-collector": "StatefulSet/otel-collector",
	"tempo":          "StatefulSet/tempo",
	"prometheus":     "Deployment/prometheus",
	"alertmanager":   "StatefulSet/alertmanager",
	"loki":           "StatefulSet/loki",
}

// podTemplateLabels returns the pod labels of every rendered Deployment and
// StatefulSet, by object ID.
func podTemplateLabels(t *testing.T, objects map[string]object) map[string]map[string]string {
	t.Helper()
	labels := map[string]map[string]string{}
	for id, obj := range objects {
		if obj.Kind != "Deployment" && obj.Kind != "StatefulSet" {
			continue
		}
		var spec struct {
			Template struct {
				Metadata struct {
					Labels map[string]string `yaml:"labels"`
				} `yaml:"metadata"`
			} `yaml:"template"`
		}
		if err := obj.Spec.Decode(&spec); err != nil {
			t.Fatal(err)
		}
		labels[id] = spec.Template.Metadata.Labels
	}
	return labels
}

// selects reports whether matchLabels selects a pod with the given labels.
func selects(matchLabels, labels map[string]string) bool {
	if len(matchLabels) == 0 {
		return false
	}
	for key, value := range matchLabels {
		if got, ok := labels[key]; !ok || got != value {
			return false
		}
	}
	return true
}

// The engine NetworkPolicies must be exactly the expected ones, each select
// only its engine's pods, admit exactly the expected port and peer pairs and
// never render an empty from (which admits every source) or an empty ports
// list (every port). A peer for a pod of this release must select a rendered
// workload, so a subchart label change cannot silently cut a flow off.
func checkBackendNetworkPolicies(t *testing.T, objects map[string]object, want map[string][]string) {
	t.Helper()
	if want == nil {
		return
	}
	for id, obj := range objects {
		if obj.Kind != "NetworkPolicy" || heartbeatPolicies[obj.Metadata.Name] {
			continue
		}
		if _, ok := want[obj.Metadata.Name]; !ok {
			t.Errorf("unexpected %s", id)
		}
	}
	workloads := podTemplateLabels(t, objects)
	for name, wantPairs := range want {
		policy, ok := objects["NetworkPolicy/"+name]
		if !ok {
			t.Errorf("missing NetworkPolicy/%s", name)
			continue
		}
		var spec struct {
			PodSelector labelSelector `yaml:"podSelector"`
			PolicyTypes []string      `yaml:"policyTypes"`
			Ingress     *[]struct {
				Ports []struct {
					Protocol string `yaml:"protocol"`
					Port     int    `yaml:"port"`
				} `yaml:"ports"`
				From *[]networkPolicyPeer `yaml:"from"`
			} `yaml:"ingress"`
		}
		if err := policy.Spec.Decode(&spec); err != nil {
			t.Fatal(err)
		}
		if strings.Join(spec.PolicyTypes, ",") != "Ingress" {
			t.Errorf("%s NetworkPolicy policyTypes = %v, want [Ingress]", name, spec.PolicyTypes)
		}
		target := backendWorkloads[name]
		if labels, ok := workloads[target]; !ok {
			t.Errorf("%s NetworkPolicy is rendered without %s", name, target)
		} else if !selects(spec.PodSelector.MatchLabels, labels) || len(spec.PodSelector.MatchExpressions) > 0 {
			t.Errorf("%s NetworkPolicy selects %s, which does not select %s's pods %v", name, spec.PodSelector, target, labels)
		}
		for id, labels := range workloads {
			if id != target && selects(spec.PodSelector.MatchLabels, labels) {
				t.Errorf("%s NetworkPolicy also selects %s's pods", name, id)
			}
		}
		if spec.Ingress == nil {
			t.Errorf("%s NetworkPolicy has no ingress field; render ingress: [] to deny all", name)
			continue
		}
		var gotPairs []string
		for _, rule := range *spec.Ingress {
			if rule.From == nil || len(*rule.From) == 0 {
				t.Errorf("%s NetworkPolicy has a rule with an empty from, which admits every source", name)
				continue
			}
			if len(rule.Ports) == 0 {
				t.Errorf("%s NetworkPolicy has a rule without ports, which admits every port", name)
			}
			for _, peer := range *rule.From {
				if peer.PodSelector != nil && peer.NamespaceSelector == nil && peer.PodSelector.MatchLabels["app.kubernetes.io/instance"] == "heartbeat" &&
					!slices.ContainsFunc(slices.Collect(maps.Values(workloads)), func(labels map[string]string) bool {
						return selects(peer.PodSelector.MatchLabels, labels)
					}) {
					t.Errorf("%s NetworkPolicy peer %s selects no rendered workload", name, peerString(peer))
				}
				for _, port := range rule.Ports {
					gotPairs = append(gotPairs, fmt.Sprintf("%s/%d <- %s", port.Protocol, port.Port, peerString(peer)))
				}
			}
		}
		sort.Strings(gotPairs)
		wantSorted := slices.Sorted(slices.Values(wantPairs))
		if !reflect.DeepEqual(gotPairs, wantSorted) && (len(gotPairs) > 0 || len(wantSorted) > 0) {
			t.Errorf("%s NetworkPolicy admits\n  %s\nwant\n  %s", name, strings.Join(gotPairs, "\n  "), strings.Join(wantSorted, "\n  "))
		}
	}
}

// An ipBlock only sees external clients' addresses when the collector
// Service keeps them: with externalTrafficPolicy Cluster, NodePort and
// LoadBalancer traffic is masqueraded to a node address first (on kind,
// kindnet then drops it).
func checkCollectorSourceAddresses(t *testing.T, objects map[string]object, peers []string) {
	t.Helper()
	svc, ok := objects["Service/db-collector"]
	if !ok || !slices.ContainsFunc(peers, func(peer string) bool { return strings.HasPrefix(peer, "ip ") }) {
		return
	}
	var spec struct {
		Type                  string `yaml:"type"`
		ExternalTrafficPolicy string `yaml:"externalTrafficPolicy"`
	}
	if err := svc.Spec.Decode(&spec); err != nil {
		t.Fatal(err)
	}
	if (spec.Type == "NodePort" || spec.Type == "LoadBalancer") && spec.ExternalTrafficPolicy != "Local" {
		t.Errorf("Service/db-collector is %s with externalTrafficPolicy %q; the NetworkPolicy ipBlock needs Local to see client addresses", spec.Type, spec.ExternalTrafficPolicy)
	}
}

// The collector keeps its outsystems_logs watermarks in a state volume at
// HEARTBEAT_STATE_DIR, the only writable path of its read-only root
// filesystem: the db-collector-state claim, not a volumeClaimTemplate, which
// could never change on an existing StatefulSet.
func checkCollectorState(t *testing.T, objects map[string]object, emptyDir bool) {
	t.Helper()
	sts, ok := objects["StatefulSet/db-collector"]
	if !ok {
		return
	}
	type volume struct {
		Name                  string    `yaml:"name"`
		EmptyDir              *struct{} `yaml:"emptyDir"`
		PersistentVolumeClaim *struct {
			ClaimName string `yaml:"claimName"`
		} `yaml:"persistentVolumeClaim"`
	}
	var spec struct {
		Template struct {
			Spec struct {
				SecurityContext struct {
					FSGroup int `yaml:"fsGroup"`
				} `yaml:"securityContext"`
				Containers []struct {
					Env []struct {
						Name  string `yaml:"name"`
						Value string `yaml:"value"`
					} `yaml:"env"`
					VolumeMounts []struct {
						Name      string `yaml:"name"`
						MountPath string `yaml:"mountPath"`
						ReadOnly  bool   `yaml:"readOnly"`
					} `yaml:"volumeMounts"`
					SecurityContext struct {
						ReadOnlyRootFilesystem bool `yaml:"readOnlyRootFilesystem"`
					} `yaml:"securityContext"`
				} `yaml:"containers"`
				Volumes []volume `yaml:"volumes"`
			} `yaml:"spec"`
		} `yaml:"template"`
		VolumeClaimTemplates []any `yaml:"volumeClaimTemplates"`
	}
	if err := sts.Spec.Decode(&spec); err != nil {
		t.Fatal(err)
	}
	pod := spec.Template.Spec
	container := pod.Containers[0]
	if !container.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("db-collector root filesystem is writable")
	}
	if pod.SecurityContext.FSGroup == 0 {
		t.Error("db-collector pod has no fsGroup; the nonroot user cannot write the state volume")
	}
	var stateDir string
	for _, env := range container.Env {
		if env.Name == "HEARTBEAT_STATE_DIR" {
			stateDir = env.Value
		}
	}
	mounted := false
	for _, mount := range container.VolumeMounts {
		if mount.Name == "state" && mount.MountPath == stateDir && stateDir != "" && !mount.ReadOnly {
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("db-collector does not mount a writable state volume at HEARTBEAT_STATE_DIR (%q)", stateDir)
	}
	if len(spec.VolumeClaimTemplates) > 0 {
		t.Error("db-collector has volumeClaimTemplates; they can never change on an existing StatefulSet")
	}
	hasEmptyDir := slices.ContainsFunc(pod.Volumes, func(v volume) bool { return v.Name == "state" && v.EmptyDir != nil })
	usesClaim := slices.ContainsFunc(pod.Volumes, func(v volume) bool {
		return v.Name == "state" && v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "db-collector-state"
	})
	pvc, hasClaim := objects["PersistentVolumeClaim/db-collector-state"]
	switch {
	case emptyDir && (!hasEmptyDir || usesClaim || hasClaim):
		t.Errorf("state: emptyDir %v, claim %v, mounted %v; want an emptyDir only", hasEmptyDir, hasClaim, usesClaim)
	case !emptyDir && (hasEmptyDir || !usesClaim || !hasClaim):
		t.Errorf("state: emptyDir %v, claim %v, mounted %v; want the db-collector-state claim only", hasEmptyDir, hasClaim, usesClaim)
	case !emptyDir:
		var claim struct {
			Resources struct {
				Requests map[string]string `yaml:"requests"`
			} `yaml:"resources"`
		}
		if err := pvc.Spec.Decode(&claim); err != nil {
			t.Fatal(err)
		}
		if size := claim.Resources.Requests["storage"]; size != "256Mi" {
			t.Errorf("state claim requests %s, want 256Mi", size)
		}
	}
}
