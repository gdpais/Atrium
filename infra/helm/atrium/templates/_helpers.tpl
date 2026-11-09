{{/*
Labels shared by every Atrium-owned object. No release revision or
timestamps: Argo CD renders the chart itself and diffs the result (ADR 0005).
*/}}
{{- define "atrium.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/part-of: atrium
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels for one component. Call with (dict "root" $ "component" "db-collector").
*/}}
{{- define "atrium.selectorLabels" -}}
app.kubernetes.io/name: {{ .component }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
{{- end }}

{{- define "atrium.componentLabels" -}}
{{ include "atrium.labels" .root }}
app.kubernetes.io/name: {{ .component }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{/*
Image reference: a digest wins over a tag; the tag defaults to appVersion.
Call with (dict "root" $ "image" .Values.dbCollector.image).
*/}}
{{- define "atrium.image" -}}
{{- if .image.digest -}}
{{ .image.repository }}@{{ .image.digest }}
{{- else -}}
{{ .image.repository }}:{{ .image.tag | default .root.Chart.AppVersion }}
{{- end -}}
{{- end }}

{{/* Fixed name of the integrations ConfigMap. */}}
{{- define "atrium.integrationsConfigMap" -}}atrium-integrations{{- end }}

{{- define "atrium.inventoryConfigMap" -}}atrium-inventory{{- end }}

{{/*
The collectors of the rendered integrations.yaml, as a YAML list (read it
back with fromYamlArray): the hand-written integrations.collectors, then
managedCollectors, which the Atrium API renders from approved probe
assignments into a values file of its own (ADR 0014). Lists replace rather
than merge across values files, so the API's file never touches the
hand-written list. The schema reserves the api- id prefix for
managedCollectors. An id listed twice, in one list or across both, fails
here, compared trimmed as the collector compares them, instead of the
collector rejecting the file at its next reload.
*/}}
{{- define "atrium.collectors" -}}
{{- $lists := list (list "integrations.collectors" ((.Values.integrations | default dict).collectors | default list)) (list "managedCollectors" (.Values.managedCollectors | default list)) }}
{{- $all := list }}
{{- $seen := dict }}
{{- range $lists }}
{{- $name := index . 0 }}
{{- range $i, $c := index . 1 }}
{{- $id := trim (toString $c.id) }}
{{- with get $seen $id }}
{{- fail (printf "collector id %q is listed twice, at %s and %s[%d]; collector ids must be unique across integrations.collectors and managedCollectors" $id . $name $i) }}
{{- end }}
{{- $_ := set $seen $id (printf "%s[%d]" $name $i) }}
{{- $all = append $all $c }}
{{- end }}
{{- end }}
{{- toYaml $all }}
{{- end }}

{{/* The integrations.yaml document as rendered into the ConfigMap. */}}
{{- define "atrium.integrationsYaml" -}}
{{ toYaml (set (deepCopy .Values.integrations) "collectors" (include "atrium.collectors" . | fromYamlArray)) }}
{{- end }}

{{/*
GOMEMLIMIT for a Go container: 80% of its memory limit, in whole MiB, so the
garbage collector works harder before the kernel OOM-kills the container; the
rest is for goroutine stacks, the runtime and memory outside the Go heap.
Empty when there is no limit. Call with (dict "limit" $c.resources.limits.memory
"name" "dbCollector").
*/}}
{{- define "atrium.goMemLimit" -}}
{{- with .limit -}}
{{- $bytes := 0.0 -}}
{{- if kindIs "string" . -}}
{{- $units := dict "" 1 "k" 1000 "M" 1000000 "G" 1000000000 "T" 1000000000000 "Ki" 1024 "Mi" 1048576 "Gi" 1073741824 "Ti" 1099511627776 -}}
{{- $number := regexFind "^[0-9]+(\\.[0-9]+)?" . -}}
{{- $suffix := trimPrefix $number . -}}
{{- if or (not $number) (not (hasKey $units $suffix)) -}}
{{- fail (printf "%s.resources.limits.memory %q: use a number with an optional k, M, G, T, Ki, Mi, Gi or Ti suffix, or set %s.goMemLimit" $.name . $.name) -}}
{{- end -}}
{{- $bytes = mulf (float64 $number) (get $units $suffix) -}}
{{- else -}}
{{- $bytes = float64 . -}}
{{- end -}}
{{- printf "%dMiB" (int64 (floor (divf (mulf $bytes 0.8) 1048576))) -}}
{{- end -}}
{{- end }}

{{/*
Pod labels of a bundled engine's pods, as the subchart's selector helpers
render them: name, instance and, for the Prometheus server, component. They
follow each subchart's nameOverride (the OTel Collector's default name is its
alias). Used as a NetworkPolicy's podSelector and as a peer, so a pod of
another release or chart never matches. Call with (dict "root" $ "engine"
"prometheus"); engine is prometheus, alertmanager, grafana, loki, tempo or
otelCollector, or db-collector or api for Atrium's own services.
*/}}
{{- define "atrium.enginePodLabels" -}}
{{- $v := .root.Values }}
{{- if has .engine (list "db-collector" "api") }}
{{- include "atrium.selectorLabels" (dict "root" .root "component" .engine) }}
{{- else if eq .engine "prometheus" }}
{{- /* The prometheus.server.matchLabels helper. */}}
app.kubernetes.io/name: {{ $v.prometheus.nameOverride | default "prometheus" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/component: {{ $v.prometheus.server.name | default "server" }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
{{- else if eq .engine "alertmanager" }}
app.kubernetes.io/name: {{ $v.prometheus.alertmanager.nameOverride | default "alertmanager" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
{{- else if eq .engine "grafana" }}
app.kubernetes.io/name: {{ $v.grafana.nameOverride | default "grafana" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
{{- else if eq .engine "loki" }}
app.kubernetes.io/name: {{ $v.loki.nameOverride | default "loki" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
{{- else if eq .engine "tempo" }}
app.kubernetes.io/name: {{ $v.tempo.nameOverride | default "tempo" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
{{- else if eq .engine "otelCollector" }}
app.kubernetes.io/name: {{ $v.otelCollector.nameOverride | default "otelCollector" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
{{- else }}
{{- fail (printf "atrium.enginePodLabels: unknown engine %q" .engine) }}
{{- end }}
{{- end }}

{{/*
A NetworkPolicyPeer (as YAML) for the pods of one engine in this release.
Call like atrium.enginePodLabels.
*/}}
{{- define "atrium.enginePeer" -}}
podSelector:
  matchLabels:
    {{- include "atrium.enginePodLabels" . | trim | nindent 4 }}
{{- end }}

{{/*
Whether the bundled Prometheus scrapes a job of prometheus.scrapeConfigs:
"true" when Prometheus is on and the job is listed and not disabled.
Call with (dict "root" $ "job" "loki").
*/}}
{{- define "atrium.prometheusScrapes" -}}
{{- $p := .root.Values.prometheus }}
{{- $job := get ($p.scrapeConfigs | default dict) .job }}
{{- if and $p.enabled (kindIs "map" $job) }}
{{- if or (not (hasKey $job "enabled")) $job.enabled }}true{{ end }}
{{- end }}
{{- end }}

{{/*
The OpenTelemetry variables of an Atrium service (ADR 0018), as container
env entries: its own logs to the Collector's otlp-self port and pipeline,
never 4318; its spans to OTLP/HTTP, sampled by ratio; and the environment it
runs in. Empty when there is nothing to set. Call with the root context.
*/}}
{{- define "atrium.telemetryEnv" -}}
{{- $v := .Values }}
{{- $ports := $v.otelCollector.ports | default dict }}
{{- if and $v.telemetry.enabled $v.otelCollector.enabled }}
# The port and pipeline of Atrium's own logs, never 4318 (ADR 0018).
- name: OTEL_EXPORTER_OTLP_LOGS_ENDPOINT
  value: http://{{ $v.otelCollector.fullnameOverride }}:{{ dig "otlp-self" "servicePort" 4319 $ports }}/v1/logs
{{- end }}
{{- if and $v.telemetry.enabled $v.telemetry.traces.enabled $v.otelCollector.enabled }}
- name: OTEL_EXPORTER_OTLP_TRACES_ENDPOINT
  value: http://{{ $v.otelCollector.fullnameOverride }}:{{ dig "otlp-http" "servicePort" 4318 $ports }}/v1/traces
- name: OTEL_TRACES_SAMPLER
  value: parentbased_traceidratio
- name: OTEL_TRACES_SAMPLER_ARG
  value: {{ $v.telemetry.traces.sampleRatio | quote }}
{{- end }}
{{- with $v.telemetry.environment }}
- name: OTEL_RESOURCE_ATTRIBUTES
  value: deployment.environment.name={{ . }}
{{- end }}
{{- end }}

{{/*
Where the API finds PostgreSQL and Redis (Valkey), as a dict: postgresHost,
postgresPort, postgresDatabase, sslMode, redisAddr and redisTLS. Empty
api.postgres.host and api.redis.addr mean the bundled postgresql and valkey,
without TLS; any other server gets TLS unless the values turn it off. Fails
when the API has neither a server nor the bundled one. Read it back with
fromYaml.
*/}}
{{- define "atrium.apiDependencies" -}}
{{- $v := .Values }}
{{- $pg := $v.api.postgres }}
{{- $redis := $v.api.redis }}
{{- $out := dict "postgresHost" $pg.host "postgresPort" (int $pg.port) "postgresDatabase" $pg.database "sslMode" ($pg.sslMode | default "verify-full") "redisAddr" $redis.addr "redisTLS" (ternary true $redis.tls (kindIs "invalid" $redis.tls)) }}
{{- if not $pg.host }}
{{- if not $v.postgresql.enabled }}
{{- fail "api.postgres.host is required when postgresql.enabled is false: the PostgreSQL server the API uses, such as an Amazon RDS endpoint" }}
{{- end }}
{{- $_ := set $out "postgresHost" "postgresql" }}
{{- $_ := set $out "postgresPort" 5432 }}
{{- $_ := set $out "postgresDatabase" $v.postgresql.auth.database }}
{{- $_ := set $out "sslMode" ($pg.sslMode | default "disable") }}
{{- end }}
{{- if not $redis.addr }}
{{- if not $v.valkey.enabled }}
{{- fail "api.redis.addr is required when valkey.enabled is false: the Redis (Valkey) server the API uses, as host:port, such as an Amazon ElastiCache endpoint" }}
{{- end }}
{{- $_ := set $out "redisAddr" "valkey:6379" }}
{{- $_ := set $out "redisTLS" (ternary false $redis.tls (kindIs "invalid" $redis.tls)) }}
{{- end }}
{{- toYaml $out }}
{{- end }}
