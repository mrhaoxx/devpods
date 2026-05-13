#!/usr/bin/env bash
# End-to-end demo for M3:
#  1. Multi-replica round-robin (at least 2 gateway pods serve traffic)
#  2. PROXY v2 round-trip (gateway logs spoofed source IP)
#  3. Trusted proxy alternate auth (non-User key authenticates)
#
# Prereqs: hack/e2e-up.sh has been run; cert-manager is installed;
# /tmp/devpod-test-key-path holds the path to alice's ssh key.

set -euo pipefail

NS=devpods
NAME=m3demo
OWNER=alice
GW_PORT=2222
KEY="$(cat /tmp/devpod-test-key-path)"

PROXY_KEY=/tmp/devpod-m3-proxy-key
PROXY_PUB="$PROXY_KEY.pub"

cleanup() {
    pkill -f "port-forward.*devpod-gateway" 2>/dev/null || true
    kubectl -n "$NS" delete devpod "$NAME" --ignore-not-found --wait=false 2>/dev/null || true
    # Restore GatewayConfig to its baseline.
    kubectl patch gatewayconfig default --type=merge -p \
        '{"spec":{"listen":{"proxyProtocol":{"enabled":false,"trustedCIDRs":[]}},"trustedProxyKeys":[]}}' 2>/dev/null || true
    rm -f "$PROXY_KEY" "$PROXY_PUB"
}
trap cleanup EXIT

start_port_forward() {
    pkill -f "port-forward.*devpod-gateway" 2>/dev/null || true
    sleep 1
    kubectl -n devpod-system port-forward svc/devpod-gateway "$GW_PORT:22" >/dev/null 2>&1 &
    local deadline=$((SECONDS + 30))
    until nc -z 127.0.0.1 "$GW_PORT" 2>/dev/null; do
        [[ $SECONDS -lt $deadline ]] || { echo "FAIL: port-forward never came up"; exit 1; }
        sleep 1
    done
}

SSH_OPTS=(-o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null
          -o ConnectTimeout=10 -o ServerAliveInterval=5 -o ServerAliveCountMax=3)

echo "[1/3] Multi-replica round-robin"
cat <<EOF | kubectl apply -f -
apiVersion: devpod.io/v1alpha1
kind: DevPod
metadata:
  name: $NAME
  namespace: $NS
spec:
  owner: $OWNER
  running: true
  pod:
    spec:
      containers:
      - name: dev
        image: debian:stable
        command: ["sleep", "infinity"]
EOF
kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Running devpod/"$NAME" --timeout=120s
start_port_forward
# Fan out 10 concurrent SSH attempts. Backgrounded ssh stdio is redirected
# to /dev/null so banner/MOTD output can't fill the pipe and block `wait`.
pids=()
for i in 1 2 3 4 5 6 7 8 9 10; do
    ssh "${SSH_OPTS[@]}" -p "$GW_PORT" -i "$KEY" "$OWNER+$NAME@127.0.0.1" -- true </dev/null >/dev/null 2>&1 &
    pids+=($!)
done
for pid in "${pids[@]}"; do
    wait "$pid" || true
done
sleep 2
# accept lines look like:
#   [pod/devpod-gateway-XXXX/gateway] {"msg":"accept",...}
# Pulling the pod-path between [] gives us a per-replica fingerprint.
distinct=$(kubectl -n devpod-system logs -l app.kubernetes.io/name=devpod-gateway --tail=200 --prefix=true 2>&1 \
    | grep '"msg":"accept"' | awk -F'[][]' '{print $2}' | sort -u | wc -l | tr -d ' ')
if [[ "$distinct" -lt 2 ]]; then
    echo "FAIL: only $distinct gateway pod(s) served traffic; expected >= 2"
    kubectl -n devpod-system logs -l app.kubernetes.io/name=devpod-gateway --tail=20 --prefix=true
    exit 1
fi
echo "OK: $distinct distinct gateway pods served 10 concurrent SSH connections"

echo "[2/3] PROXY v2 round-trip"
kubectl patch gatewayconfig default --type=merge -p \
    '{"spec":{"listen":{"proxyProtocol":{"enabled":true,"trustedCIDRs":["0.0.0.0/0"]}}}}'
kubectl -n devpod-system rollout restart deploy/devpod-gateway
kubectl -n devpod-system rollout status deploy/devpod-gateway --timeout=120s
start_port_forward
# Write PROXY v2 header claiming 1.2.3.4:5678 as source.
go run ./hack/proxyv2-writer -addr 127.0.0.1:"$GW_PORT" -src 1.2.3.4:5678
# Poll because the kubectl logs aggregator can lag a few seconds behind
# container stdout.
found=0
deadline=$((SECONDS + 20))
while [[ $SECONDS -lt $deadline ]]; do
    if kubectl -n devpod-system logs -l app.kubernetes.io/name=devpod-gateway --tail=200 2>&1 \
        | grep -q '"msg":"accept".*"from":"1.2.3.4:5678"'; then
        found=1
        break
    fi
    sleep 1
done
if [[ $found -eq 1 ]]; then
    echo "OK: gateway parsed PROXY v2 header (saw from=1.2.3.4:5678 in accept log)"
else
    echo "FAIL: expected an accept log line with from=1.2.3.4:5678"
    kubectl -n devpod-system logs -l app.kubernetes.io/name=devpod-gateway --tail=30
    exit 1
fi

echo "[3/3] Trusted proxy alternate auth"
# Generate a key NOT in alice's User pubkeys, register it as a trustedProxyKey.
ssh-keygen -q -t ed25519 -N "" -f "$PROXY_KEY"
PUB=$(cat "$PROXY_PUB")
kubectl patch gatewayconfig default --type=merge -p \
    "{\"spec\":{\"listen\":{\"proxyProtocol\":{\"enabled\":false,\"trustedCIDRs\":[]}},\"trustedProxyKeys\":[{\"alias\":\"e2e\",\"pubkey\":\"${PUB}\"}]}}"
kubectl -n devpod-system rollout restart deploy/devpod-gateway
kubectl -n devpod-system rollout status deploy/devpod-gateway --timeout=120s
start_port_forward
out=$(ssh "${SSH_OPTS[@]}" -p "$GW_PORT" -i "$PROXY_KEY" "$OWNER+$NAME@127.0.0.1" -- echo trustedproxy 2>&1 | tail -1 | tr -d '\r\n')
if [[ "$out" != "trustedproxy" ]]; then
    echo "FAIL: trusted-proxy auth did not succeed: $out"
    kubectl -n devpod-system logs -l app.kubernetes.io/name=devpod-gateway --tail=20
    exit 1
fi
found=0
deadline=$((SECONDS + 20))
while [[ $SECONDS -lt $deadline ]]; do
    if kubectl -n devpod-system logs -l app.kubernetes.io/name=devpod-gateway --tail=200 2>&1 \
        | grep -q '"auth_path":"trusted_proxy"'; then
        found=1
        break
    fi
    sleep 1
done
if [[ $found -eq 1 ]]; then
    echo "OK: audit log records trusted_proxy auth path"
else
    echo "FAIL: expected an audit log with auth_path=trusted_proxy"
    kubectl -n devpod-system logs -l app.kubernetes.io/name=devpod-gateway --tail=30
    exit 1
fi

echo
echo "OK: M3 demo passed — multi-replica, PROXY v2, trusted-proxy all green."
