#!/usr/bin/env bash
# Render the chart in the audit configurations and assert the wiring.
# Requires helm. Run from anywhere; CI runs it in the helm-render job.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CHART="$ROOT/deploy/chart"
OUT="$(mktemp -d)"
trap 'rm -rf "$OUT"' EXIT
fail=0

render() { # name, helm --set args...
  local name=$1; shift
  helm template devpod "$CHART" "$@" >"$OUT/$name.yaml"
  helm lint "$CHART" "$@" >/dev/null || { echo "FAIL [$name]: helm lint"; fail=1; }
}
has() { grep -qF -- "$2" "$OUT/$1.yaml" || { echo "FAIL [$1]: missing: $2"; fail=1; }; }
# doc RENDER KIND NAME: print that one object of a render.
doc() { KIND=$2 NAME=$3 perl -0777 -ne 'for (split /^---$/m) { print if /^kind: \Q$ENV{KIND}\E$/m && /^  name: \Q$ENV{NAME}\E$/m }' "$OUT/$1.yaml"; }
docHas() { # RENDER KIND NAME TEXT
  grep -qF -- "$4" <<<"$(doc "$1" "$2" "$3")" || { echo "FAIL [$1]: $2/$3 lacks: $4"; fail=1; }
}
lacks() { if grep -qF -- "$2" "$OUT/$1.yaml"; then echo "FAIL [$1]: unexpected: $2"; fail=1; fi; }

WEBUI=(--set webui.enabled=true --set webui.passwordAuth=true --set webui.sessionKeySecret.name=sk)

# (a) defaults: audit is off and nothing audit-related renders.
render defaults "${WEBUI[@]}"
lacks defaults "devpod-audit-"
lacks defaults "--audit-"
lacks defaults "terminationGracePeriodSeconds: 300"

# (b) bundled Silo + ClickHouse.
render bundled "${WEBUI[@]}" --set audit.enabled=true \
  --set audit.s3.bundled.enabled=true --set audit.clickhouse.bundled.enabled=true
has bundled "name: devpod-audit-s3"
has bundled "kind: StatefulSet"
has bundled "name: devpod-audit-clickhouse"
has bundled "name: devpod-audit-migrate"
has bundled '"helm.sh/hook": post-install,post-upgrade'
has bundled "- --create-bucket=true"
has bundled "- --manage-lifecycle=true"
has bundled "- --retention-days=180"
has bundled "- --audit-clickhouse-url=clickhouse://devpod-audit-clickhouse.devpod-system.svc:9000"
has bundled "- --audit-s3-endpoint=http://devpod-audit-s3.devpod-system.svc:9000"
# The spool budget leaves headroom below the emptyDir limit: kubelet counts
# disk blocks, the spool counts bytes, and exceeding sizeLimit evicts the pod.
has bundled "- --audit-spool-max-bytes=17408Mi"
# ~2 MiB per audited session (measured): 1Gi covers ~300 sessions on one
# replica, and GOMEMLIMIT (90%) makes the GC work before the OOM killer.
has bundled "memory: 1Gi"
has bundled "- {name: GOMEMLIMIT, value: 921MiB}"
has bundled "terminationGracePeriodSeconds: 300"
has bundled "sizeLimit: 20Gi"
has bundled "secretName: devpod-audit-ch-gateway"
has bundled "secretName: devpod-audit-ch-webui"
has bundled "secretName: devpod-audit-ch-migrate"
has bundled "GRANT INSERT ON devpod_audit.*"
has bundled "audit:"
has bundled "enabled: true"
has bundled "docker.io/pgsty/silo:RELEASE.2026-10-26T00-00-00Z"
has bundled "clickhouse/clickhouse-server:26.8.36.3"
has bundled "app.kubernetes.io/name: devpod-audit-migrate"
has bundled "resources: [\"gatewayconfigs\"]"

render small "${WEBUI[@]}" --set audit.enabled=true --set audit.spool.sizeLimit=2Gi \
  --set audit.s3.bundled.enabled=true --set audit.clickhouse.bundled.enabled=true
has small "- --audit-spool-max-bytes=1740Mi"
has small "sizeLimit: 2Gi"

# (c) external storage: no StatefulSets, the given Secrets are mounted.
render external "${WEBUI[@]}" --set audit.enabled=true \
  --set audit.s3.endpoint=https://s3.example.com --set audit.s3.credentialsSecret=my-s3 \
  --set audit.clickhouse.url=clickhouse://ch.example.com:9000 \
  --set audit.clickhouse.credentialsSecrets.gateway=ch-gw \
  --set audit.clickhouse.credentialsSecrets.webui=ch-ui \
  --set audit.clickhouse.credentialsSecrets.migrate=ch-mig
lacks external "kind: StatefulSet"
has external "secretName: my-s3"
has external "secretName: ch-gw"
has external "secretName: ch-ui"
has external "secretName: ch-mig"
has external "- --audit-clickhouse-url=clickhouse://ch.example.com:9000"
has external "- --create-bucket=false"
has external "- --manage-lifecycle=false"

# (e) site settings a production install needs (were hand-edited on zjusct).
render site "${WEBUI[@]}" --set audit.enabled=true \
  --set audit.s3.bundled.enabled=true --set audit.clickhouse.bundled.enabled=true \
  --set 'imagePullSecrets[0].name=harbor-pull' --set controller.homeHostPath=/tank/home \
  --set-json 'gateway.trustedProxyKeys=[{"alias":"openng","pubkey":"ssh-ed25519 AAAAtest openng"}]'
has site "- --home-host-path=/tank/home"
has site "trustedProxyKeys:"
has site "alias: openng"
has site "pubkey: ssh-ed25519 AAAAtest openng"
n=$(grep -c "name: harbor-pull" "$OUT/site.yaml" || true)
[ "$n" -ge 4 ] || { echo "FAIL [site]: imagePullSecrets on $n pod specs, want controller, gateway, webui, migrate"; fail=1; }
lacks defaults "imagePullSecrets"
lacks defaults "home-host-path"

# (f) webui behind a Gateway API HTTPRoute: hostnames[0] is served over the
# HTTPS listener; plain HTTP and the other hostnames 301 to it.
ROUTE=(--set webui.httpRoute.enabled=true --set webui.httpRoute.gateway.name=gw
  --set webui.httpRoute.gateway.namespace=gw-ns --set webui.httpRoute.httpsListener=https-wild
  --set webui.httpRoute.httpListener=http
  --set-json 'webui.httpRoute.hostnames=["devpod.example.org","devpod.alt.example.org"]')
render route "${WEBUI[@]}" --set webui.baseURL=https://devpod.example.org "${ROUTE[@]}"
n=$(grep -c "^kind: HTTPRoute" "$OUT/route.yaml" || true)
[ "$n" -eq 3 ] || { echo "FAIL [route]: $n HTTPRoutes, want 3 (backend, http redirect, alias redirect)"; fail=1; }
has route "sectionName: https-wild"
has route "sectionName: http"
has route "- devpod.alt.example.org"
has route "hostname: devpod.example.org"
has route "scheme: https"
has route "statusCode: 301"
has route "- --external-url=https://devpod.example.org"
lacks defaults "kind: HTTPRoute"
if helm template devpod "$CHART" "${WEBUI[@]}" "${ROUTE[@]}" >/dev/null 2>"$OUT/route-nourl.err"; then
  echo "FAIL [route]: rendered without webui.baseURL"; fail=1
fi

# (g) telemetry: the leader publishes itself behind a selectorless Service;
# the shared ingest key reaches the controller (tokens) and the webui.
AUDIT=(--set audit.enabled=true --set audit.s3.bundled.enabled=true --set audit.clickhouse.bundled.enabled=true)
render telemetry "${WEBUI[@]}" "${AUDIT[@]}" --set telemetry.enabled=true
has telemetry "name: devpod-webui-ingest"
svc=$(doc telemetry Service devpod-webui-ingest)
[ -n "$svc" ] || { echo "FAIL [telemetry]: no devpod-webui-ingest Service"; fail=1; }
if grep -q "selector:" <<<"$svc"; then echo "FAIL [telemetry]: the ingest Service must be selectorless"; fail=1; fi
grep -qF "name: ingest" <<<"$svc" || { echo "FAIL [telemetry]: ingest Service port must be named ingest (matches the EndpointSlice)"; fail=1; }
has telemetry "containerPort: 8081"
has telemetry "- --ingest-key-file=/etc/devpod/ingest/key"
has telemetry "- --leader-election-namespace=devpod-system"
has telemetry "- --ingest-service=devpod-webui-ingest"
has telemetry "- --idle-cpu-pct=5"
has telemetry "- --agent-silent-after=3m"
has telemetry "fieldPath: status.podIP"
# the leader decodes and writes every DevPod's telemetry: sized resources
docHas telemetry Deployment devpod-webui "cpu: \"2\""
docHas telemetry Deployment devpod-webui "memory: 1Gi"
docHas telemetry Deployment devpod-webui "- {name: GOGC, value: \"200\"}"
docHas telemetry Deployment devpod-webui "- {name: GOMEMLIMIT, value: 921MiB}"
docHas telemetry Role devpod-webui-leader "namespace: devpod-system"
docHas telemetry Role devpod-webui-leader 'resources: ["endpointslices"]'
docHas telemetry Role devpod-webui-leader 'resources: ["leases"]'
docHas telemetry RoleBinding devpod-webui-leader "name: devpod-webui"
docHas telemetry NetworkPolicy devpod-webui-ingest "port: 8081"
docHas telemetry NetworkPolicy devpod-webui-ingest "kubernetes.io/metadata.name: devpods"
docHas telemetry NetworkPolicy devpod-webui-ingest "app.kubernetes.io/name: devpod-webui"
docHas telemetry Role devpod-webui 'verbs: ["create", "patch"]'
has telemetry "- --ingest-url=http://devpod-webui-ingest.devpod-system.svc:8081/ingest/v1/batch"
has telemetry "- --telemetry-metrics-interval=1s"
has telemetry "- --metrics-retention-1s-days=7"
has telemetry "- --process-retention-hours=48"
has telemetry "name: devpod-ingest-key"
lacks defaults "devpod-webui-ingest"
lacks defaults "--ingest-"
lacks defaults "devpod-ingest-key"
lacks defaults "name: GOGC"
lacks bundled "devpod-webui-ingest"
lacks bundled "--ingest-"
if helm template devpod "$CHART" "${WEBUI[@]}" --set telemetry.enabled=true >/dev/null 2>"$OUT/tele-noaudit.err"; then
  echo "FAIL [telemetry]: rendered without audit"; fail=1
elif ! grep -qF "audit.enabled" "$OUT/tele-noaudit.err"; then
  echo "FAIL [telemetry]: unhelpful error: $(cat "$OUT/tele-noaudit.err")"; fail=1
fi

# (d) `helm upgrade --reuse-values` from v0.1.x: the old release's values
# replace the chart defaults, so audit.* and gatewayConfigName are absent.
OLD="$OUT/chart-v0.1"
cp -R "$CHART" "$OLD"
cp "$ROOT/hack/testdata/chart-values-v0.1.yaml" "$OLD/values.yaml"
if helm template devpod "$OLD" "${WEBUI[@]}" >"$OUT/reuse.yaml" 2>"$OUT/reuse.err"; then
  lacks reuse "--audit-"
  has reuse "- --gateway-config-name=default"
else
  echo "FAIL [reuse]: old values do not render: $(cat "$OUT/reuse.err")"; fail=1
fi
# Enabling audit on top of old values must say how to fix it.
if helm template devpod "$OLD" "${WEBUI[@]}" --set audit.enabled=true \
  --set audit.s3.bundled.enabled=true --set audit.clickhouse.bundled.enabled=true >/dev/null 2>"$OUT/reuse-audit.err"; then
  echo "FAIL [reuse-audit]: rendered with incomplete audit values"; fail=1
elif ! grep -qF -- "--reset-then-reuse-values" "$OUT/reuse-audit.err"; then
  echo "FAIL [reuse-audit]: unhelpful error: $(cat "$OUT/reuse-audit.err")"; fail=1
fi

[ "$fail" -eq 0 ] && echo "chart: OK"
exit "$fail"
