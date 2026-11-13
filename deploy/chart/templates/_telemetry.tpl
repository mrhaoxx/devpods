{{/* Live telemetry wiring shared by the controller and the webui. */}}

{{/* devpod.telemetry.on: "true" when telemetry is enabled; fails when
     its prerequisites are off. Old (--reuse-values) releases have no
     telemetry values and render as off. */}}
{{/* devpod.telemetry.forward: "true" in a cluster managed from
     elsewhere whose agent passes telemetry on to the console's receiver:
     no receiver, no store and no key of its own here. */}}
{{- define "devpod.telemetry.forward" -}}
{{- $a := .Values.agent | default dict -}}
{{- if and (dig "enabled" false $a) (not .Values.webui.enabled) -}}true{{- end -}}
{{- end -}}

{{/* devpod.telemetry.viaAgent: "true" when this cluster's DevPods post
     their telemetry to the agent, which takes it to the console over its
     link: in a cluster managed from elsewhere, and in the console's own
     cluster with agent.telemetryOverLink (DevPods on the host network, which
     the receiver's policy does not admit, reach the agent). */}}
{{- define "devpod.telemetry.viaAgent" -}}
{{- $a := .Values.agent | default dict -}}
{{- if or (include "devpod.telemetry.forward" .) (and (dig "enabled" false $a) (dig "telemetryOverLink" false $a)) -}}true{{- end -}}
{{- end -}}
{{- define "devpod.telemetry.on" -}}
{{- $t := .Values.telemetry | default dict -}}
{{- if and $t.enabled (include "devpod.telemetry.forward" .) -}}
true
{{- else if $t.enabled -}}
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
{{- if include "devpod.telemetry.viaAgent" . -}}
http://devpod-agent-ingest.{{ .Values.namespaces.system }}.svc:8081/ingest/v1/batch
{{- else -}}
http://devpod-webui-ingest.{{ .Values.namespaces.system }}.svc:8081/ingest/v1/batch
{{- end -}}
{{- end -}}

{{- define "devpod.telemetry.keyMount" -}}
- {name: ingest-key, mountPath: /etc/devpod/ingest, readOnly: true}
{{- end -}}

{{- define "devpod.telemetry.keyVolume" -}}
- name: ingest-key
  secret:
    secretName: devpod-ingest-key
{{- end -}}

{{/* devpod.ingest.public: the console of several clusters takes their
     telemetry at <baseURL>/ingest/v1/batch through its HTTPRoute. */}}
{{- define "devpod.ingest.public" -}}
{{- if and (include "devpod.telemetry.on" .) (not (include "devpod.telemetry.forward" .)) .Values.webui.enabled (dig "clusters" "enabled" false .Values.webui) (dig "httpRoute" "enabled" false .Values.webui) -}}
true
{{- end -}}
{{- end }}

{{/* devpod.ingest.proxyNamespace: where the route's proxy Pods run. */}}
{{- define "devpod.ingest.proxyNamespace" -}}
{{- $r := .Values.webui.httpRoute -}}
{{- $r.proxyNamespace | default $r.gateway.namespace | default .Values.namespaces.system -}}
{{- end }}
