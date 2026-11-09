{{/* Audit storage wiring shared by the gateway, webui and migrate Job. */}}

{{- define "devpod.audit.chURL" -}}
{{- if .Values.audit.clickhouse.bundled.enabled -}}
clickhouse://devpod-audit-clickhouse.{{ .Values.namespaces.system }}.svc:9000
{{- else -}}
{{ required "audit.clickhouse.url is required (or audit.clickhouse.bundled.enabled)" .Values.audit.clickhouse.url }}
{{- end -}}
{{- end -}}

{{- define "devpod.audit.s3Endpoint" -}}
{{- if .Values.audit.s3.bundled.enabled -}}
http://devpod-audit-s3.{{ .Values.namespaces.system }}.svc:9000
{{- else -}}
{{ .Values.audit.s3.endpoint }}
{{- end -}}
{{- end -}}

{{- define "devpod.audit.s3Secret" -}}
{{- if .Values.audit.s3.bundled.enabled -}}
devpod-audit-s3
{{- else -}}
{{ required "audit.s3.credentialsSecret is required (or audit.s3.bundled.enabled)" .Values.audit.s3.credentialsSecret }}
{{- end -}}
{{- end -}}

{{/* devpod.audit.chSecret: (dict "root" $ "component" "gateway") */}}
{{- define "devpod.audit.chSecret" -}}
{{- if .root.Values.audit.clickhouse.bundled.enabled -}}
devpod-audit-ch-{{ .component }}
{{- else -}}
{{ required (printf "audit.clickhouse.credentialsSecrets.%s is required" .component) (index .root.Values.audit.clickhouse.credentialsSecrets .component) }}
{{- end -}}
{{- end -}}

{{/* devpod.audit.args: storage flags (same names on all three binaries). */}}
{{- define "devpod.audit.args" -}}
- --audit-clickhouse-url={{ include "devpod.audit.chURL" . }}
- --audit-clickhouse-database={{ .Values.audit.clickhouse.database }}
- --audit-clickhouse-credentials-dir=/etc/devpod/audit/clickhouse
{{- with (include "devpod.audit.s3Endpoint" .) }}
- --audit-s3-endpoint={{ . }}
{{- end }}
- --audit-s3-bucket={{ .Values.audit.s3.bucket }}
- --audit-s3-region={{ .Values.audit.s3.region }}
- --audit-s3-path-style={{ .Values.audit.s3.pathStyle }}
- --audit-s3-credentials-dir=/etc/devpod/audit/s3
{{- end -}}

{{- define "devpod.audit.mounts" -}}
- {name: audit-clickhouse, mountPath: /etc/devpod/audit/clickhouse, readOnly: true}
- {name: audit-s3, mountPath: /etc/devpod/audit/s3, readOnly: true}
{{- end -}}

{{/* devpod.audit.volumes: (dict "root" $ "component" "gateway") */}}
{{- define "devpod.audit.volumes" -}}
- name: audit-clickhouse
  secret:
    secretName: {{ include "devpod.audit.chSecret" . }}
- name: audit-s3
  secret:
    secretName: {{ include "devpod.audit.s3Secret" .root }}
{{- end -}}

{{/* devpod.audit.password: keep a generated password across upgrades.
     (dict "root" $ "secret" "name" "key" "password") */}}
{{- define "devpod.audit.password" -}}
{{- $existing := lookup "v1" "Secret" .root.Values.namespaces.system .secret -}}
{{- if and $existing (index $existing.data .key) -}}
{{ index $existing.data .key | b64dec }}
{{- else -}}
{{ randAlphaNum 32 }}
{{- end -}}
{{- end -}}

{{/* Pods allowed to reach the bundled storage. */}}
{{- define "devpod.audit.clients" -}}
- podSelector:
    matchExpressions:
    - key: app.kubernetes.io/name
      operator: In
      values: [devpod-gateway, devpod-webui, devpod-audit-migrate]
{{- end -}}
