{{/*
Chart name.
*/}}
{{- define "backyard.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified app name (63 characters max, DNS label).
*/}}
{{- define "backyard.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "backyard.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "backyard.labels" -}}
helm.sh/chart: {{ include "backyard.chart" . }}
{{ include "backyard.selectorLabels" . }}
app.kubernetes.io/version: {{ include "backyard.imageTag" . | trunc 63 | trimSuffix "-" | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: backyard
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "backyard.selectorLabels" -}}
app.kubernetes.io/name: {{ include "backyard.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "backyard.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "backyard.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "backyard.imageTag" -}}
{{- default .Chart.AppVersion .Values.image.tag }}
{{- end }}

{{- define "backyard.image" -}}
{{- if .Values.image.digest }}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else }}
{{- printf "%s:%s" .Values.image.repository (include "backyard.imageTag" .) }}
{{- end }}
{{- end }}

{{/*
Name of the Secret holding the server's environment.
*/}}
{{- define "backyard.secretName" -}}
{{- default (include "backyard.fullname" .) .Values.existingSecret }}
{{- end }}

{{/*
True when the chart manages the Secret.
*/}}
{{- define "backyard.createSecret" -}}
{{- if and .Values.secret.create (not .Values.existingSecret) }}true{{ end }}
{{- end }}

{{/*
BACKYARD_SECRET: the configured value, else the one already in the cluster
(kept across upgrades), else a new random one.
*/}}
{{- define "backyard.backyardSecret" -}}
{{- if .Values.secret.backyardSecret }}
{{- if lt (len .Values.secret.backyardSecret) 32 }}
{{- fail "secret.backyardSecret must be at least 32 characters (openssl rand -hex 32)" }}
{{- end }}
{{- .Values.secret.backyardSecret }}
{{- else }}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (include "backyard.fullname" .) }}
{{- if and $existing $existing.data (hasKey $existing.data "BACKYARD_SECRET") }}
{{- index $existing.data "BACKYARD_SECRET" | b64dec }}
{{- else }}
{{- randAlphaNum 64 }}
{{- end }}
{{- end }}
{{- end }}

{{/*
The rendered backyard.yaml.
*/}}
{{- define "backyard.config" -}}
{{- tpl (toYaml .Values.config) . }}
{{- end }}

{{/*
Effective config values (with Backyard's defaults) used by the templates.
*/}}
{{- define "backyard.baseDomain" -}}
{{- dig "server" "base_domain" "" .Values.config | lower | trimSuffix "." }}
{{- end }}

{{- define "backyard.urlMode" -}}
{{- dig "server" "url_mode" "host" .Values.config | default "host" }}
{{- end }}

{{- define "backyard.publicURL" -}}
{{- $url := dig "server" "public_url" "" .Values.config }}
{{- if $url }}
{{- $url | trimSuffix "/" }}
{{- else }}
{{- printf "https://%s" (include "backyard.baseDomain" .) }}
{{- end }}
{{- end }}

{{/*
True when more than one pod may run at the same time.
*/}}
{{- define "backyard.multiReplica" -}}
{{- if or .Values.autoscaling.enabled (gt (int .Values.replicaCount) 1) }}true{{ end }}
{{- end }}

{{/*
True when the data volume is a ReadWriteOnce PVC (pods cannot share it).
*/}}
{{- define "backyard.rwoVolume" -}}
{{- if and .Values.persistence.enabled (not (has "ReadWriteMany" .Values.persistence.accessModes)) }}true{{ end }}
{{- end }}

{{/*
"true" when a server.trusted_proxies entry names at most one proxy's worth of
addresses: a bare IP, an IPv4 CIDR of /24 or longer, or an IPv6 CIDR of /120
or longer. ${VAR} references cannot be checked at render time and count as
wide.
*/}}
{{- define "backyard.narrowProxy" -}}
{{- $p := toString . | trim }}
{{- if and $p (not (contains "${" $p)) }}
{{- $parts := splitList "/" $p }}
{{- if eq (len $parts) 1 }}true
{{- else if and (eq (len $parts) 2) (regexMatch "^[0-9]{1,3}$" (index $parts 1)) }}
{{- $min := ternary 120 24 (contains ":" (index $parts 0)) }}
{{- if ge (atoi (index $parts 1)) $min }}true{{ end }}
{{- end }}
{{- end }}
{{- end }}

{{/*
auth.mode header without auth.header.jwt_header takes identity headers from
any peer in server.trusted_proxies. Fail unless those peers are one proxy's
addresses or the chart's NetworkPolicy keeps everyone else out: otherwise any
pod in the cluster can sign in as any user, admins included.
*/}}
{{- define "backyard.validateHeaderAuth" -}}
{{- $cfg := include "backyard.config" . | fromYaml }}
{{- $auth := get $cfg "auth" | default dict }}
{{- $mode := get $auth "mode" | default "none" | toString | lower }}
{{- $header := get $auth "header" | default dict }}
{{- if and (eq $mode "header") (not (get $header "jwt_header")) }}
{{- $proxies := dig "server" "trusted_proxies" list $cfg | default list }}
{{- if not $proxies }}
{{- fail "config.auth.mode header without auth.header.jwt_header needs config.server.trusted_proxies: the addresses of the proxy that sets the identity headers" }}
{{- end }}
{{- if not .Values.networkPolicy.enabled }}
{{- $wide := list }}
{{- range $proxies }}
{{- if not (include "backyard.narrowProxy" .) }}{{ $wide = append $wide (toString .) }}{{ end }}
{{- end }}
{{- if $wide }}
{{- fail (printf "config.auth.mode header without auth.header.jwt_header lets every peer in config.server.trusted_proxies assert any identity, admins included, and these entries are wider than one proxy (IPv4 /24, IPv6 /120) or unknown at render time: %s. Any pod in them could sign in as anyone. Set networkPolicy.enabled: true with networkPolicy.from admitting only the ingress controller or authenticating proxy, narrow trusted_proxies to that proxy's addresses, or verify a signed JWT with auth.header.jwt_header and jwks_url (see the chart README)" (join ", " $wide)) }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Validate settings that would only fail at runtime.
*/}}
{{- define "backyard.validate" -}}
{{- if not (include "backyard.baseDomain" .) }}
{{- fail "config.server.base_domain is required (e.g. backyard.example.com)" }}
{{- end }}
{{- $db := dig "database" "type" "sqlite" .Values.config | default "sqlite" }}
{{- $pubsub := dig "pubsub" "type" "memory" .Values.config | default "memory" }}
{{- $storage := dig "storage" "type" "fs" .Values.config | default "fs" }}
{{- if include "backyard.multiReplica" . }}
{{- if has $db (list "sqlite" "memory") }}
{{- fail (printf "config.database.type %q cannot be shared by several replicas: use postgres or dynamodb, or set replicaCount: 1 and autoscaling.enabled: false" $db) }}
{{- end }}
{{- if eq $pubsub "memory" }}
{{- fail "config.pubsub.type memory only reaches clients of one replica: use redis or postgres with several replicas" }}
{{- end }}
{{- if include "backyard.rwoVolume" . }}
{{- fail "several replicas cannot share a ReadWriteOnce volume: use persistence.accessModes [ReadWriteMany], or bucket storage (s3/gcs/azure) with persistence.enabled: false" }}
{{- end }}
{{- if and (eq $storage "fs") (not .Values.persistence.enabled) }}
{{- fail "fs storage with several replicas needs a shared volume: persistence.enabled with accessModes [ReadWriteMany] (or use s3/gcs/azure storage)" }}
{{- end }}
{{- end }}
{{- if eq $storage "fs" }}
{{- $root := dig "storage" "fs" "root" "/var/lib/backyard" .Values.config | default "/var/lib/backyard" }}
{{- if ne (trimSuffix "/" $root) (trimSuffix "/" .Values.persistence.mountPath) }}
{{- fail (printf "config.storage.fs.root (%s) must equal persistence.mountPath (%s)" $root .Values.persistence.mountPath) }}
{{- end }}
{{- end }}
{{- if and (not .Values.existingSecret) (not .Values.secret.create) }}
{{- $hasSecret := false }}
{{- range .Values.extraEnv }}{{ if eq .name "BACKYARD_SECRET" }}{{ $hasSecret = true }}{{ end }}{{ end }}
{{- if and (not $hasSecret) (not (dig "server" "secret" "" .Values.config)) (not .Values.extraEnvFrom) }}
{{- fail "BACKYARD_SECRET is not provided: set existingSecret, secret.create: true, or a BACKYARD_SECRET entry in extraEnv" }}
{{- end }}
{{- end }}
{{- if and .Values.serviceMonitor.enabled (not (dig "telemetry" "metrics_listen" "" .Values.config)) }}
{{- fail "serviceMonitor.enabled needs config.telemetry.metrics: true and metrics_listen (e.g. \":9090\")" }}
{{- end }}
{{- if .Values.networkPolicy.enabled }}
{{- if not .Values.networkPolicy.from }}
{{- fail "networkPolicy.enabled needs networkPolicy.from: the ingress controller or proxy pods allowed to reach Backyard (an empty list would admit every peer)" }}
{{- end }}
{{- if and .Values.serviceMonitor.enabled (not .Values.networkPolicy.metricsFrom) }}
{{- fail "serviceMonitor.enabled with networkPolicy.enabled needs networkPolicy.metricsFrom (e.g. Prometheus' namespace), or nothing can scrape the metrics port" }}
{{- end }}
{{- end }}
{{- include "backyard.validateHeaderAuth" . }}
{{- end }}
