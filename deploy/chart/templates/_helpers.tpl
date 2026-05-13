{{- define "devpod.labels" -}}
app.kubernetes.io/name: devpod
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: devpod-{{ .Chart.Version }}
{{- end -}}
