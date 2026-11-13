{{- define "devpod.labels" -}}
app.kubernetes.io/name: devpod
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: devpod-{{ .Chart.Version }}
{{- end -}}

{{- /* devpod.http.on: non-empty when the gateway's HTTP proxy is enabled. */ -}}
{{- define "devpod.http.on" -}}
{{- if dig "http" "enabled" false .Values.gateway -}}
{{- $_ := required "gateway.http.baseDomain is required with gateway.http.enabled" .Values.gateway.http.baseDomain -}}
true
{{- end -}}
{{- end -}}

{{- /* devpod.cli.on: non-empty when the gateway serves the CLI over SSH
       (it calls the web UI's API with the shared key devpod-cli-key). */ -}}
{{- define "devpod.cli.on" -}}
{{- if dig "cli" "enabled" false .Values.gateway -}}
{{- if not .Values.webui.enabled -}}
{{- fail "gateway.cli.enabled needs webui.enabled: the CLI runs against the web UI's API" -}}
{{- end -}}
true
{{- end -}}
{{- end -}}

{{/* devpod.gateway.name: the gateway's name — of its Deployment, Services,
     ServiceAccount, roles, key Secrets and the label its Pods carry. Set
     another (gateway.name) to run beside an older install's devpod-gateway. */}}
{{- define "devpod.gateway.name" -}}
{{- dig "name" "" .Values.gateway | default "devpod-gateway" -}}
{{- end -}}

