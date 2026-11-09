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
has bundled "- --audit-spool-max-bytes=20Gi"
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

[ "$fail" -eq 0 ] && echo "chart: OK"
exit "$fail"
