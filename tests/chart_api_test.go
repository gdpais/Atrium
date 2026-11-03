//go:build chart

package tests

import (
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

// The API (ADR 0013) and its bundled PostgreSQL and Valkey (ADR 0015), as
// TestChartProfiles renders them.

// apiObjects are what the chart renders for the API with the bundled
// engines on.
var apiObjects = []string{
	"Deployment/api", "Service/api", "Service/api-headless", "NetworkPolicy/api",
	"StatefulSet/postgresql", "Service/postgresql", "PersistentVolumeClaim/postgresql-data", "NetworkPolicy/postgresql",
	"StatefulSet/valkey", "Service/valkey", "NetworkPolicy/valkey",
}

// bundledEngineObjects are the bundled PostgreSQL's and Valkey's objects.
var bundledEngineObjects = []string{
	"StatefulSet/postgresql", "Service/postgresql", "PersistentVolumeClaim/postgresql-data", "NetworkPolicy/postgresql",
	"StatefulSet/valkey", "Service/valkey", "NetworkPolicy/valkey",
}

// heartbeatPolicies are the NetworkPolicies of Heartbeat's own workloads,
// which checkBackendNetworkPolicies leaves to their own checks.
var heartbeatPolicies = map[string]bool{"db-collector": true, "api": true, "postgresql": true, "valkey": true}

// apiDependencies is what the API is told about PostgreSQL and Redis, as
// the values of its environment variables; empty sslRootCert means unset.
type apiDependencies struct {
	postgresHost, postgresPort, postgresDatabase, sslMode, sslRootCert string
	redisAddr, redisTLS                                                string
}

// bundledDependencies are the bundled postgresql and valkey, without TLS.
var bundledDependencies = apiDependencies{
	postgresHost: "postgresql", postgresPort: "5432", postgresDatabase: "heartbeat",
	sslMode: "disable", redisAddr: "valkey:6379", redisTLS: "false",
}

type container struct {
	Name            string   `yaml:"name"`
	Image           string   `yaml:"image"`
	ImagePullPolicy string   `yaml:"imagePullPolicy"`
	Command         []string `yaml:"command"`
	Args            []string `yaml:"args"`
	Env             []struct {
		Name      string `yaml:"name"`
		Value     string `yaml:"value"`
		ValueFrom *struct {
			SecretKeyRef *struct {
				Name string `yaml:"name"`
				Key  string `yaml:"key"`
			} `yaml:"secretKeyRef"`
		} `yaml:"valueFrom"`
	} `yaml:"env"`
	EnvFrom      []any `yaml:"envFrom"`
	VolumeMounts []struct {
		Name      string `yaml:"name"`
		MountPath string `yaml:"mountPath"`
		SubPath   string `yaml:"subPath"`
		ReadOnly  bool   `yaml:"readOnly"`
	} `yaml:"volumeMounts"`
	LivenessProbe  *probe `yaml:"livenessProbe"`
	ReadinessProbe *probe `yaml:"readinessProbe"`
	Resources      struct {
		Limits map[string]string `yaml:"limits"`
	} `yaml:"resources"`
	SecurityContext struct {
		AllowPrivilegeEscalation *bool `yaml:"allowPrivilegeEscalation"`
		ReadOnlyRootFilesystem   bool  `yaml:"readOnlyRootFilesystem"`
		Capabilities             struct {
			Drop []string `yaml:"drop"`
		} `yaml:"capabilities"`
	} `yaml:"securityContext"`
}

type probe struct {
	HTTPGet *struct {
		Path string `yaml:"path"`
	} `yaml:"httpGet"`
	Exec *struct {
		Command []string `yaml:"command"`
	} `yaml:"exec"`
	// Zero when unset: the kubelet's default, 1 second.
	TimeoutSeconds int `yaml:"timeoutSeconds"`
}

type podSpec struct {
	Replicas *int `yaml:"replicas"`
	Template struct {
		Spec struct {
			AutomountServiceAccountToken *bool `yaml:"automountServiceAccountToken"`
			SecurityContext              struct {
				RunAsNonRoot bool `yaml:"runAsNonRoot"`
				RunAsUser    int  `yaml:"runAsUser"`
			} `yaml:"securityContext"`
			InitContainers []container `yaml:"initContainers"`
			Containers     []container `yaml:"containers"`
			Volumes        []struct {
				Name   string `yaml:"name"`
				Secret *struct {
					SecretName string `yaml:"secretName"`
				} `yaml:"secret"`
				EmptyDir              any `yaml:"emptyDir"`
				PersistentVolumeClaim *struct {
					ClaimName string `yaml:"claimName"`
				} `yaml:"persistentVolumeClaim"`
			} `yaml:"volumes"`
		} `yaml:"spec"`
	} `yaml:"template"`
}

func decodePodSpec(t *testing.T, obj object) podSpec {
	t.Helper()
	var spec podSpec
	if err := obj.Spec.Decode(&spec); err != nil {
		t.Fatal(err)
	}
	return spec
}

// env returns a container's variables with a literal value.
func (c container) env() map[string]string {
	out := map[string]string{}
	for _, e := range c.Env {
		if e.ValueFrom == nil {
			out[e.Name] = e.Value
		}
	}
	return out
}

// mountedAt returns the volume mounted at path, and whether it is a whole,
// read-only directory.
func (c container) mountedAt(path string) (name string, wholeDir bool) {
	for _, m := range c.VolumeMounts {
		if m.MountPath == path {
			return m.Name, m.SubPath == "" && m.ReadOnly
		}
	}
	return "", false
}

// postgresVariables are what both `api migrate up` and the API read about
// PostgreSQL and the credentials.
var postgresVariables = []string{
	"HEARTBEAT_API_POSTGRES_HOST", "HEARTBEAT_API_POSTGRES_PORT", "HEARTBEAT_API_POSTGRES_DATABASE",
	"HEARTBEAT_API_POSTGRES_SSLMODE", "HEARTBEAT_API_POSTGRES_SSLROOTCERT", "HEARTBEAT_CREDENTIALS_DIR",
}

// The API's Deployment: an init container runs `api migrate up` with the
// API's own image and PostgreSQL settings before the API starts, never a
// hook; both read their logins from the mounted credentials Secret as files;
// liveness never depends on PostgreSQL or Redis, readiness does; and its
// telemetry variables are the db-collector's (ADR 0018).
func checkAPI(t *testing.T, objects map[string]object, want *apiDependencies) {
	t.Helper()
	deployment, ok := objects["Deployment/api"]
	if !ok {
		if _, ok := objects["NetworkPolicy/api"]; ok {
			t.Error("NetworkPolicy/api without Deployment/api")
		}
		return
	}
	if want == nil {
		want = &bundledDependencies
	}
	spec := decodePodSpec(t, deployment)
	pod := spec.Template.Spec
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("api pods mount a ServiceAccount token; the API never talks to Kubernetes")
	}
	if !pod.SecurityContext.RunAsNonRoot {
		t.Error("api pods may run as root")
	}
	if len(pod.InitContainers) != 1 || len(pod.Containers) != 1 {
		t.Fatalf("api pod has %d init containers and %d containers, want 1 and 1", len(pod.InitContainers), len(pod.Containers))
	}
	migrate, api := pod.InitContainers[0], pod.Containers[0]
	if migrate.Name != "migrate" || !slices.Equal(migrate.Args, []string{"migrate", "up"}) || len(migrate.Command) > 0 {
		t.Errorf("api init container %s runs %v %v, want migrate running the image's entrypoint with [migrate up]", migrate.Name, migrate.Command, migrate.Args)
	}
	if migrate.Image != api.Image || migrate.ImagePullPolicy != api.ImagePullPolicy {
		t.Errorf("migrate runs %s (%s), the API %s (%s); the migrations must be the ones the API embeds", migrate.Image, migrate.ImagePullPolicy, api.Image, api.ImagePullPolicy)
	}
	if strings.HasSuffix(api.Image, ":") {
		t.Errorf("api image %q has no tag", api.Image)
	}

	apiEnv, migrateEnv := api.env(), migrate.env()
	for _, name := range postgresVariables {
		if apiEnv[name] != migrateEnv[name] {
			t.Errorf("%s is %q for the API and %q for migrate; both must reach the same database", name, apiEnv[name], migrateEnv[name])
		}
	}
	for name := range migrateEnv {
		if strings.HasPrefix(name, "OTEL_") || strings.HasPrefix(name, "HEARTBEAT_API_REDIS_") || name == "HEARTBEAT_API_AUDIT_ENDPOINT" {
			t.Errorf("migrate sets %s; `api migrate up` reads only PostgreSQL's settings and sends no telemetry", name)
		}
	}
	// Audit records go to the OTel Collector's OTLP/HTTP receiver, the logs
	// pipeline with the queue on disk (ADR 0016), whenever the Collector is
	// on, telemetry or not; never to the 4319 receiver of the API's own logs.
	wantAudit := ""
	if config, ok := collectorRelay(t, objects); ok {
		var relay struct {
			Receivers struct {
				OTLP struct {
					Protocols struct {
						HTTP struct {
							Endpoint string `yaml:"endpoint"`
						} `yaml:"http"`
					} `yaml:"protocols"`
				} `yaml:"otlp"`
			} `yaml:"receivers"`
		}
		mustUnmarshal(t, config, &relay)
		_, port, _ := strings.Cut(relay.Receivers.OTLP.Protocols.HTTP.Endpoint, "}:")
		wantAudit = "http://otel-collector:" + port
	}
	if got := apiEnv["HEARTBEAT_API_AUDIT_ENDPOINT"]; got != wantAudit {
		t.Errorf("HEARTBEAT_API_AUDIT_ENDPOINT = %q, want %q: the Collector's OTLP/HTTP logs receiver while it is on", got, wantAudit)
	}
	got := apiDependencies{
		postgresHost: apiEnv["HEARTBEAT_API_POSTGRES_HOST"], postgresPort: apiEnv["HEARTBEAT_API_POSTGRES_PORT"],
		postgresDatabase: apiEnv["HEARTBEAT_API_POSTGRES_DATABASE"], sslMode: apiEnv["HEARTBEAT_API_POSTGRES_SSLMODE"],
		sslRootCert: apiEnv["HEARTBEAT_API_POSTGRES_SSLROOTCERT"],
		redisAddr:   apiEnv["HEARTBEAT_API_REDIS_ADDR"], redisTLS: apiEnv["HEARTBEAT_API_REDIS_TLS"],
	}
	if got != *want {
		t.Errorf("API dependencies = %+v, want %+v", got, *want)
	}
	if got.sslRootCert != "" {
		if _, ok := api.mountedAt(path(got.sslRootCert)); !ok {
			t.Errorf("HEARTBEAT_API_POSTGRES_SSLROOTCERT %s is in no read-only directory mount", got.sslRootCert)
		}
	}
	if apiEnv["HEARTBEAT_API_LISTEN_ADDR"] != ":8081" {
		t.Errorf("HEARTBEAT_API_LISTEN_ADDR = %q, want :8081", apiEnv["HEARTBEAT_API_LISTEN_ADDR"])
	}
	if api.Resources.Limits["memory"] != "" && apiEnv["GOMEMLIMIT"] == "" {
		t.Error("the API has a memory limit and no GOMEMLIMIT")
	}

	// Credentials: a directory of files read on every connection, in both
	// containers, never variables.
	dir := apiEnv["HEARTBEAT_CREDENTIALS_DIR"]
	var secret string
	for _, v := range pod.Volumes {
		if v.Name == "credentials" && v.Secret != nil {
			secret = v.Secret.SecretName
		}
	}
	if (dir == "") != (secret == "") {
		t.Errorf("HEARTBEAT_CREDENTIALS_DIR %q with credentials Secret %q", dir, secret)
	}
	for _, c := range []container{migrate, api} {
		if len(c.EnvFrom) > 0 {
			t.Errorf("%s uses envFrom; credentials must stay files", c.Name)
		}
		for _, e := range c.Env {
			if strings.HasPrefix(e.Name, "HEARTBEAT_CREDENTIAL_") {
				t.Errorf("%s sets %s; credentials must stay files", c.Name, e.Name)
			}
		}
		if dir != "" {
			if name, whole := c.mountedAt(dir); name != "credentials" || !whole {
				t.Errorf("%s mounts %q at %s (a whole read-only directory: %t), want the credentials Secret", c.Name, name, dir, whole)
			}
		}
		if !c.SecurityContext.ReadOnlyRootFilesystem || c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
			t.Errorf("%s may write its root filesystem or escalate privileges", c.Name)
		}
	}

	if api.LivenessProbe == nil || api.LivenessProbe.HTTPGet == nil || api.LivenessProbe.HTTPGet.Path != "/healthz" {
		t.Error("API liveness is not GET /healthz, which depends on neither PostgreSQL nor Redis")
	}
	if api.ReadinessProbe == nil || api.ReadinessProbe.HTTPGet == nil || api.ReadinessProbe.HTTPGet.Path != "/readyz" {
		t.Error("API readiness is not GET /readyz, which waits for both and the newest migration")
	}
	// GET /readyz answers within 2 seconds, every check included
	// (defaultTimeouts in apps/api/internal/app/app.go).
	if api.ReadinessProbe != nil && api.ReadinessProbe.TimeoutSeconds <= 2 {
		t.Errorf("API readiness times out after %ds, within the API's own 2s for its checks", max(api.ReadinessProbe.TimeoutSeconds, 1))
	}

	// The same telemetry wiring as the db-collector.
	if _, ok := objects["StatefulSet/db-collector"]; ok {
		for _, name := range []string{"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_TRACES_SAMPLER", "OTEL_TRACES_SAMPLER_ARG", "OTEL_RESOURCE_ATTRIBUTES"} {
			collector, _ := collectorEnv(t, objects, name)
			if apiEnv[name] != collector {
				t.Errorf("API %s = %q, db-collector %q; ADR 0018 wires both the same way", name, apiEnv[name], collector)
			}
		}
	}
	if logs := apiEnv["OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"]; logs != "" && !strings.HasSuffix(logs, ":4319/v1/logs") {
		t.Errorf("the API sends its own logs to %s; they belong on the otlp/self port, never 4318 (ADR 0018)", logs)
	}

	// Prometheus scrapes every replica through the headless Service, ready
	// or not, like the collector.
	if headless, ok := objects["Service/api-headless"]; !ok {
		t.Error("no Service/api-headless to scrape")
	} else {
		var svc struct {
			ClusterIP                string `yaml:"clusterIP"`
			PublishNotReadyAddresses bool   `yaml:"publishNotReadyAddresses"`
		}
		if err := headless.Spec.Decode(&svc); err != nil {
			t.Fatal(err)
		}
		if svc.ClusterIP != "None" || !svc.PublishNotReadyAddresses {
			t.Errorf("Service/api-headless clusterIP %q, publishNotReadyAddresses %t; want None and true", svc.ClusterIP, svc.PublishNotReadyAddresses)
		}
	}
	if svc, ok := objects["Service/api"]; ok {
		var spec struct {
			Type string `yaml:"type"`
		}
		if err := svc.Spec.Decode(&spec); err != nil {
			t.Fatal(err)
		}
		if spec.Type != "ClusterIP" {
			t.Errorf("Service/api is %s; the API has no authentication yet [4.2]", spec.Type)
		}
	}
}

// path returns the directory of a file path.
func path(file string) string {
	return file[:strings.LastIndex(file, "/")]
}

// policyPairs returns the port and peer pairs a NetworkPolicy admits, in
// "TCP/8081 <- peerString" form, sorted; it fails on an empty from or ports
// list, which admit every source or port.
func policyPairs(t *testing.T, policy object) []string {
	t.Helper()
	var spec struct {
		PolicyTypes []string `yaml:"policyTypes"`
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
		t.Errorf("%s policyTypes = %v, want [Ingress]", policy.id(), spec.PolicyTypes)
	}
	if spec.Ingress == nil {
		t.Errorf("%s has no ingress field; render ingress: [] to deny all", policy.id())
		return nil
	}
	pairs := []string{}
	for _, rule := range *spec.Ingress {
		if rule.From == nil || len(*rule.From) == 0 || len(rule.Ports) == 0 {
			t.Errorf("%s has a rule with an empty from or ports, which admits every source or port", policy.id())
			continue
		}
		for _, peer := range *rule.From {
			for _, port := range rule.Ports {
				pairs = append(pairs, fmt.Sprintf("%s/%d <- %s", port.Protocol, port.Port, peerString(peer)))
			}
		}
	}
	sort.Strings(pairs)
	return pairs
}

// checkPolicy compares a NetworkPolicy's pairs with want, the peers admitted
// to port, and checks it selects exactly the workload's pods.
func checkPolicy(t *testing.T, objects map[string]object, name, workload string, port int, wantPeers []string) {
	t.Helper()
	policy, ok := objects["NetworkPolicy/"+name]
	if !ok {
		return
	}
	var spec struct {
		PodSelector labelSelector `yaml:"podSelector"`
	}
	if err := policy.Spec.Decode(&spec); err != nil {
		t.Fatal(err)
	}
	workloads := podTemplateLabels(t, objects)
	for id, labels := range workloads {
		if selects(spec.PodSelector.MatchLabels, labels) != (id == workload) {
			t.Errorf("NetworkPolicy/%s selects %s's pods: %t", name, id, id != workload)
		}
	}
	want := []string{}
	for _, peer := range wantPeers {
		want = append(want, fmt.Sprintf("TCP/%d <- %s", port, peer))
	}
	sort.Strings(want)
	if got := policyPairs(t, policy); !reflect.DeepEqual(got, want) {
		t.Errorf("NetworkPolicy/%s admits\n  %s\nwant\n  %s", name, strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// The API's port 8081 admits exactly the expected peers.
func checkAPINetworkPolicy(t *testing.T, objects map[string]object, want []string) {
	t.Helper()
	if want == nil {
		return
	}
	if _, ok := objects["NetworkPolicy/api"]; !ok {
		t.Error("missing NetworkPolicy/api")
		return
	}
	checkPolicy(t, objects, "api", "Deployment/api", 8081, want)
}

// The bundled PostgreSQL and Valkey (ADR 0015): one replica each, a
// readiness probe, non-root with a read-only root filesystem, passwords from
// their Secrets and never on a command line, and a NetworkPolicy that admits
// only the API, and the engine's extraPeers (by engine name, in peerString
// form).
func checkBundledEngines(t *testing.T, objects map[string]object, extraPeers map[string][]string) {
	t.Helper()
	var admitted []string
	if _, ok := objects["Deployment/api"]; ok {
		admitted = []string{apiPeer}
	}
	engines := []struct {
		name string
		port int
	}{{"postgresql", 5432}, {"valkey", 6379}}
	for _, engine := range engines {
		sts, ok := objects["StatefulSet/"+engine.name]
		if !ok {
			if _, ok := objects["NetworkPolicy/"+engine.name]; ok {
				t.Errorf("NetworkPolicy/%s without its StatefulSet", engine.name)
			}
			continue
		}
		spec := decodePodSpec(t, sts)
		pod := spec.Template.Spec
		if spec.Replicas == nil || *spec.Replicas != 1 {
			t.Errorf("StatefulSet/%s replicas = %v, want 1", engine.name, spec.Replicas)
		}
		if !pod.SecurityContext.RunAsNonRoot || pod.SecurityContext.RunAsUser == 0 {
			t.Errorf("StatefulSet/%s may run as root", engine.name)
		}
		if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
			t.Errorf("StatefulSet/%s mounts a ServiceAccount token", engine.name)
		}
		if len(pod.Containers) != 1 {
			t.Fatalf("StatefulSet/%s has %d containers", engine.name, len(pod.Containers))
		}
		c := pod.Containers[0]
		if c.ReadinessProbe == nil || c.ReadinessProbe.Exec == nil {
			t.Errorf("StatefulSet/%s has no readiness probe", engine.name)
		}
		if !c.SecurityContext.ReadOnlyRootFilesystem {
			t.Errorf("StatefulSet/%s may write its root filesystem", engine.name)
		}
		if c.Resources.Limits["memory"] == "" {
			t.Errorf("StatefulSet/%s has no memory limit", engine.name)
		}
		for name, value := range c.env() {
			if strings.Contains(strings.ToLower(name), "password") && !strings.HasSuffix(name, "_FILE") {
				t.Errorf("StatefulSet/%s sets %s=%q in plain text", engine.name, name, value)
			}
		}
		command := strings.Join(slices.Concat(c.Command, c.Args), " ")
		switch engine.name {
		case "postgresql":
			if file := c.env()["POSTGRES_PASSWORD_FILE"]; file == "" {
				t.Error("PostgreSQL reads no POSTGRES_PASSWORD_FILE")
			} else if name, whole := c.mountedAt(path(file)); name == "" || !whole {
				t.Errorf("POSTGRES_PASSWORD_FILE %s is in no read-only directory mount", file)
			}
			if name, _ := c.mountedAt("/var/lib/postgresql"); name != "data" {
				t.Error("PostgreSQL's data volume is not at /var/lib/postgresql, the parent of postgres 18's PGDATA")
			}
			var claim string
			for _, v := range pod.Volumes {
				if v.Name == "data" && v.PersistentVolumeClaim != nil {
					claim = v.PersistentVolumeClaim.ClaimName
				}
			}
			if _, ok := objects["PersistentVolumeClaim/"+claim]; claim == "" || !ok {
				t.Errorf("PostgreSQL's data is on claim %q, which this profile does not render; a pod restart must keep it", claim)
			}
		case "valkey":
			auth := false
			for _, e := range c.Env {
				if e.Name == "VALKEYCLI_AUTH" && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
					auth = true
				}
			}
			if !auth {
				t.Error("Valkey's password does not come from its Secret as VALKEYCLI_AUTH")
			}
			if !strings.Contains(command, "exec valkey-server - <<EOF") || !strings.Contains(command, `requirepass "$VALKEYCLI_AUTH"`) {
				t.Errorf("Valkey's configuration does not reach it on standard input with the password from the environment: %s", command)
			}
			if !strings.Contains(command, "maxmemory-policy noeviction") {
				t.Error("Valkey may evict keys: sessions and jobs would vanish without an error")
			}
			_, claim := objects["PersistentVolumeClaim/valkey-data"]
			if strings.Contains(command, "appendonly yes") != claim {
				t.Errorf("Valkey's append-only file is on (%t) without its claim, or off with it (%t)", strings.Contains(command, "appendonly yes"), claim)
			}
		}
		want := admitted
		if peers, ok := extraPeers[engine.name]; ok {
			want = slices.Concat(admitted, peers)
		}
		checkPolicy(t, objects, engine.name, "StatefulSet/"+engine.name, engine.port, want)
	}
}

// The missing-API alert ships only where its absent(up{job="api"}) can
// match: the API rendered and scraped by the bundled Prometheus as job api.
// Anywhere else it would fire for good.
func checkAPIMissingAlert(t *testing.T, objects map[string]object) {
	t.Helper()
	const file = "heartbeat-api-missing.rules.yml"
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
			scraped = scraped || job.JobName == "api"
		}
	}
	_, api := objects["Deployment/api"]
	want := api && scraped
	content, got := rules.Data[file]
	if got != want {
		t.Fatalf("rules ConfigMap holds %s: %v, want %v (API rendered: %v, scraped by the bundled Prometheus: %v)", file, got, want, api, scraped)
	}
	if got && !strings.Contains(content, `absent(up{job="api"})`) {
		t.Errorf("%s does not alert on absent(up{job=\"api\"}):\n%s", file, content)
	}
}
