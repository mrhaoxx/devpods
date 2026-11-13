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
{{/* devpod.audit.route: the route block when routes to the bundled store
     are on, else nothing. */}}
{{- define "devpod.audit.route" -}}
{{- $r := dig "route" dict .Values.audit -}}
{{- if dig "enabled" false $r -}}{{ toJson $r }}{{- end -}}
{{- end -}}

{{/* devpod.audit.httpRoute: an HTTPRoute to a bundled store's Service.
     dict: root, name (route and Service), hostname (its key in
     audit.route), port. */}}
{{- define "devpod.audit.httpRoute" -}}
{{- $r := fromJson (include "devpod.audit.route" .root) -}}
---
# the bundled store for gateways in other clusters, at its own hostname
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: {{ .name }}
  namespace: {{ .root.Values.namespaces.system }}
  labels: {{ include "devpod.labels" .root | nindent 4 }}
spec:
  parentRefs:
  - name: {{ required "audit.route.gateway.name is required" $r.gateway.name }}
    namespace: {{ $r.gateway.namespace | default .root.Values.namespaces.system }}
    {{- with $r.httpsListener }}
    sectionName: {{ . }}
    {{- end }}
  hostnames:
  - {{ required (printf "audit.route.%s is required" .hostname) (index $r .hostname) }}
  rules:
  - backendRefs:
    - name: {{ .name }}
      port: {{ .port }}
{{- end -}}

{{/* devpod.audit.proxyFrom: the network policy rule admitting the route's
     proxy Pods to port (nothing when routes are off). dict: root, port. */}}
{{- define "devpod.audit.proxyFrom" -}}
{{- with include "devpod.audit.route" .root }}
{{- $r := fromJson . }}
# the route's proxy, for gateways in other clusters
- from:
  - namespaceSelector:
      matchLabels:
        kubernetes.io/metadata.name: {{ $r.proxyNamespace | default $r.gateway.namespace | default $.root.Values.namespaces.system }}
  ports: [{port: {{ $.port }}}]
{{- end }}
{{- end -}}

{{- define "devpod.audit.clients" -}}
- podSelector:
    matchExpressions:
    - key: app.kubernetes.io/name
      operator: In
      values: [devpod-gateway, devpod-webui, devpod-audit-migrate]
{{- end -}}

{{/* devpod.audit.on: "true" when audit is enabled, else "". Values from
     before session audit (`helm upgrade --reuse-values` from v0.1.x) have
     no audit block: that renders as disabled, and enabling audit on top of
     them fails with a hint instead of a nil-pointer error. */}}
{{- define "devpod.audit.on" -}}
{{- $a := .Values.audit | default dict -}}
{{- if $a.enabled -}}
{{- if not (and (dig "spool" "sizeLimit" "" $a) (dig "s3" "bucket" "" $a) (dig "clickhouse" "database" "" $a)) -}}
{{- fail "audit values are incomplete: this release's values predate session audit. Upgrade with --reset-then-reuse-values (Helm >= 3.14) instead of --reuse-values so the chart's new defaults apply." -}}
{{- end -}}
true
{{- end -}}
{{- end -}}

{{/* devpod.audit.spoolMaxBytes: 85% of audit.spool.sizeLimit. The spool
     counts bytes while kubelet counts disk blocks, and exceeding the
     emptyDir sizeLimit evicts the pod (killing every session). */}}
{{- define "devpod.audit.spoolMaxBytes" -}}
{{- div (mul (include "devpod.mebibytes" (toString .Values.audit.spool.sizeLimit)) 85) 100 }}Mi
{{- end -}}

{{/* devpod.mebibytes: a Mi or Gi quantity string as whole MiB. */}}
{{- define "devpod.mebibytes" -}}
{{- if hasSuffix "Gi" . -}}
{{- mul (trimSuffix "Gi" . | atoi) 1024 -}}
{{- else if hasSuffix "Mi" . -}}
{{- trimSuffix "Mi" . | atoi -}}
{{- else -}}
{{- fail (printf "quantity must be given in Mi or Gi, got %q" .) -}}
{{- end -}}
{{- end -}}
