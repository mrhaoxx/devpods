{{/* Live telemetry wiring shared by the controller and the webui. */}}

{{/* devpod.telemetry.on: "true" when telemetry is enabled; fails when
     its prerequisites are off. Old (--reuse-values) releases have no
     telemetry values and render as off. */}}
{{- define "devpod.telemetry.on" -}}
{{- $t := .Values.telemetry | default dict -}}
{{- if $t.enabled -}}
{{- if not (include "devpod.audit.on" .) -}}
{{- fail "telemetry.enabled requires audit.enabled (metrics are stored in the audit ClickHouse)" -}}
{{- end -}}
{{- if not .Values.webui.enabled -}}
{{- fail "telemetry.enabled requires webui.enabled (the webui leader receives the telemetry)" -}}
{{- end -}}
true
{{- end -}}
{{- end -}}

{{- define "devpod.telemetry.ingestURL" -}}
http://devpod-webui-ingest.{{ .Values.namespaces.system }}.svc:8081/ingest/v1/batch
{{- end -}}

{{- define "devpod.telemetry.keyMount" -}}
- {name: ingest-key, mountPath: /etc/devpod/ingest, readOnly: true}
{{- end -}}

{{- define "devpod.telemetry.keyVolume" -}}
- name: ingest-key
  secret:
    secretName: devpod-ingest-key
{{- end -}}
