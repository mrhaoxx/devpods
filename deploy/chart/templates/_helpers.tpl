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
