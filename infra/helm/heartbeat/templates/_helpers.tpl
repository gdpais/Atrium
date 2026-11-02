{{/*
Labels shared by every Heartbeat-owned object. No release revision or
timestamps: Argo CD renders the chart itself and diffs the result (ADR 0005).
*/}}
{{- define "heartbeat.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/part-of: heartbeat
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels for one component. Call with (dict "root" $ "component" "db-collector").
*/}}
{{- define "heartbeat.selectorLabels" -}}
app.kubernetes.io/name: {{ .component }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
{{- end }}

{{- define "heartbeat.componentLabels" -}}
{{ include "heartbeat.labels" .root }}
app.kubernetes.io/name: {{ .component }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{/*
Image reference: a digest wins over a tag; the tag defaults to appVersion.
Call with (dict "root" $ "image" .Values.dbCollector.image).
*/}}
{{- define "heartbeat.image" -}}
{{- if .image.digest -}}
{{ .image.repository }}@{{ .image.digest }}
{{- else -}}
{{ .image.repository }}:{{ .image.tag | default .root.Chart.AppVersion }}
{{- end -}}
{{- end }}

{{/* Fixed name of the integrations ConfigMap. */}}
{{- define "heartbeat.integrationsConfigMap" -}}heartbeat-integrations{{- end }}

{{/*
The collectors of the rendered integrations.yaml, as a YAML list (read it
back with fromYamlArray): the hand-written integrations.collectors, then
managedCollectors, which the Heartbeat API renders from approved probe
assignments into a values file of its own (ADR 0014). Lists replace rather
than merge across values files, so the API's file never touches the
hand-written list. The schema reserves the api- id prefix for
managedCollectors. An id listed twice, in one list or across both, fails
here, compared trimmed as the collector compares them, instead of the
collector rejecting the file at its next reload.
*/}}
{{- define "heartbeat.collectors" -}}
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
{{- define "heartbeat.integrationsYaml" -}}
{{ toYaml (set (deepCopy .Values.integrations) "collectors" (include "heartbeat.collectors" . | fromYamlArray)) }}
{{- end }}

{{/*
GOMEMLIMIT for a Go container: 80% of its memory limit, in whole MiB, so the
garbage collector works harder before the kernel OOM-kills the container; the
rest is for goroutine stacks, the runtime and memory outside the Go heap.
Empty when there is no limit. Call with (dict "limit" $c.resources.limits.memory
"name" "dbCollector").
*/}}
{{- define "heartbeat.goMemLimit" -}}
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
"prometheus"); engine is prometheus, alertmanager, grafana, loki or
otelCollector, or db-collector for Heartbeat's own collector.
*/}}
{{- define "heartbeat.enginePodLabels" -}}
{{- $v := .root.Values }}
{{- if eq .engine "db-collector" }}
{{- include "heartbeat.selectorLabels" (dict "root" .root "component" "db-collector") }}
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
{{- fail (printf "heartbeat.enginePodLabels: unknown engine %q" .engine) }}
{{- end }}
{{- end }}

{{/*
A NetworkPolicyPeer (as YAML) for the pods of one engine in this release.
Call like heartbeat.enginePodLabels.
*/}}
{{- define "heartbeat.enginePeer" -}}
podSelector:
  matchLabels:
    {{- include "heartbeat.enginePodLabels" . | trim | nindent 4 }}
{{- end }}

{{/*
Whether the bundled Prometheus scrapes a job of prometheus.scrapeConfigs:
"true" when Prometheus is on and the job is listed and not disabled.
Call with (dict "root" $ "job" "loki").
*/}}
{{- define "heartbeat.prometheusScrapes" -}}
{{- $p := .root.Values.prometheus }}
{{- $job := get ($p.scrapeConfigs | default dict) .job }}
{{- if and $p.enabled (kindIs "map" $job) }}
{{- if or (not (hasKey $job "enabled")) $job.enabled }}true{{ end }}
{{- end }}
{{- end }}
