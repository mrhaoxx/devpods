#!/usr/bin/env bash
# Integration tests against real ClickHouse + S3 (Silo) running as
# throwaway pods on a Kubernetes cluster, reached via port-forward.
#
#   KUBE_CONTEXT=zjusct bash hack/test-integration.sh [go test args...]
#   KUBE_CONTEXT=zjusct bash hack/test-integration.sh down   # delete the namespace
#
# The pods live in namespace devpod-audit-it (see hack/integration/
# fixtures.yaml) and are left running between runs; `down` removes them.
set -euo pipefail

: "${KUBE_CONTEXT:?set KUBE_CONTEXT to the cluster that hosts the test pods}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
NS=devpod-audit-it
K=(kubectl --context "$KUBE_CONTEXT")

if [ "${1:-}" = "down" ]; then
  "${K[@]}" delete namespace "$NS" --ignore-not-found --wait=false
  exit 0
fi

"${K[@]}" apply -f "$ROOT/hack/integration/fixtures.yaml" >/dev/null
"${K[@]}" -n "$NS" rollout status deploy/clickhouse --timeout=10m >/dev/null
"${K[@]}" -n "$NS" rollout status deploy/silo --timeout=10m >/dev/null

TMP="$(mktemp -d)"
PIDS=()
cleanup() { for p in "${PIDS[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null || true; done; rm -rf "$TMP"; }
trap cleanup EXIT

# forward NAME TARGET_PORT → prints the local port once forwarding is up.
forward() {
  local log="$TMP/$1.log"
  "${K[@]}" -n "$NS" port-forward "deploy/$1" "0:$2" >"$log" 2>&1 &
  PIDS+=("$!")
  for _ in $(seq 1 100); do
    local port
    port=$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9]*\).*/\1/p' "$log" | head -1)
    if [ -n "$port" ]; then echo "$port"; return 0; fi
    sleep 0.2
  done
  echo "port-forward $1 did not come up:" >&2; cat "$log" >&2; return 1
}

CH_PORT=$(forward clickhouse 9000)
S3_PORT=$(forward silo 9000)

export AUDIT_IT_CLICKHOUSE_URL="clickhouse://127.0.0.1:$CH_PORT"
export AUDIT_IT_CLICKHOUSE_USER=it
export AUDIT_IT_CLICKHOUSE_PASSWORD=it-password
export AUDIT_IT_S3_ENDPOINT="http://127.0.0.1:$S3_PORT"
export AUDIT_IT_S3_ACCESS_KEY=devpod
export AUDIT_IT_S3_SECRET_KEY=devpod-secret

cd "$ROOT"
go test -tags integration ./internal/audit/... -count=1 "$@"
