#!/usr/bin/env bash
# End-to-end demo for v2: the bug v2 fixes is "scp / VS Code Remote
# fails because sftp-server ran in the wrong namespace". This script
# proves that's gone — the in-container sshd serves SFTP in the same
# filesystem the interactive SSH shell sees.
#
# Prereqs: hack/e2e-up.sh has been run; cert-manager is installed;
# /tmp/devpod-test-key-path holds the path to alice's ssh key.

set -euo pipefail

NS=devpods
NAME=v2demo
OWNER=alice
GW_PORT=2222
KEY="$(cat /tmp/devpod-test-key-path)"
LOCAL=/tmp/devpod-v2-marker

cleanup() {
    pkill -f "port-forward.*devpod-gateway" 2>/dev/null || true
    kubectl -n "$NS" delete devpod "$NAME" --ignore-not-found --wait=false 2>/dev/null || true
    /bin/rm -f "$LOCAL"
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

ssh_run() {
    ssh -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -p "$GW_PORT" -i "$KEY" "$OWNER+$NAME@127.0.0.1" -- "$@"
}

echo "[1/6] Apply DevPod (debian rootfs, 1Gi persistent /workspace)"
cat <<EOF | kubectl apply -f -
apiVersion: devpod.io/v1alpha1
kind: DevPod
metadata: {name: $NAME, namespace: $NS}
spec:
  owner: $OWNER
  running: true
  persistence: {size: 1Gi, mountPath: /workspace}
  pod:
    spec:
      containers:
      - {name: dev, image: debian:stable, command: ["sleep","infinity"]}
EOF
kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Running devpod/"$NAME" --timeout=180s

echo "[2/6] Verify Pod layout: 1 init container, no sidecar"
init_count=$(kubectl -n "$NS" get pod "$OWNER-$NAME" -o jsonpath='{range .spec.initContainers[*]}{.name} {end}' | tr -s ' ')
ctr_count=$(kubectl -n "$NS" get pod "$OWNER-$NAME" -o jsonpath='{range .spec.containers[*]}{.name} {end}' | tr -s ' ')
echo "  init: $init_count"
echo "  ctrs: $ctr_count"
if ! grep -q "devpod-bootstrap" <<<"$init_count"; then
    echo "FAIL: expected devpod-bootstrap init container"
    exit 1
fi
if grep -q "devpod-sshd" <<<"$ctr_count"; then
    echo "FAIL: legacy devpod-sshd sidecar still present"
    exit 1
fi
echo "OK: bootstrap init container + zero sidecar"

echo "[3/6] Verify shell lands in user container (debian)"
start_port_forward
out=$(ssh_run 'cat /etc/os-release | head -1')
if ! grep -q "Debian" <<<"$out"; then
    echo "FAIL: expected Debian rootfs, got: $out"
    exit 1
fi
echo "OK: shell sees debian rootfs"

echo "[4/6] scp a local file into the user container — the bug v2 fixes"
echo "v2 sftp works at $(date)" > "$LOCAL"
scp -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -P "$GW_PORT" -i "$KEY" "$LOCAL" "$OWNER+$NAME@127.0.0.1:/workspace/marker"

got=$(ssh_run 'cat /workspace/marker')
want=$(cat "$LOCAL")
if [[ "$got" != "$want" ]]; then
    echo "FAIL: scp-uploaded file did not round-trip"
    echo "want: $want"
    echo "got:  $got"
    exit 1
fi
echo "OK: scp wrote into /workspace; SSH shell sees the same bytes"

echo "[5/6] Hibernate + resume + marker survives"
kubectl -n "$NS" patch devpod "$NAME" --type=merge -p '{"spec":{"running":false}}'
kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Stopped devpod/"$NAME" --timeout=60s
kubectl -n "$NS" patch devpod "$NAME" --type=merge -p '{"spec":{"running":true}}'
kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Running devpod/"$NAME" --timeout=180s
start_port_forward
got=$(ssh_run 'cat /workspace/marker')
if [[ "$got" != "$want" ]]; then
    echo "FAIL: marker lost across hibernate"
    exit 1
fi
echo "OK: marker survived hibernate"

echo "[6/6] Verify status.endpoint encodes backendPort 2222"
endpoint=$(kubectl -n "$NS" get devpod "$NAME" -o jsonpath='{.status.endpoint}')
if [[ "$endpoint" != *":2222" ]]; then
    echo "FAIL: status.endpoint=%s does not end in :2222" "$endpoint"
    exit 1
fi
echo "OK: status.endpoint = $endpoint"

echo
echo "OK: v2 demo passed — sshd in user ns, scp/sftp work, hibernate roundtrip, backendPort 2222."
