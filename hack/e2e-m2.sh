#!/usr/bin/env bash
# End-to-end demo for M2: persistence + manual hibernate.
#
# Writes a marker file over SSH, hibernates (spec.running=false),
# verifies the Pod is gone but the PVC stays, resumes, and reads the
# marker back. Also exercises the webhook (reserved volume name)
# and the finalizer-detach behavior on DevPod delete.
#
# Prereqs: bash hack/e2e-up.sh has been run and the gateway/controller
# Deployments are healthy in OrbStack k8s; cert-manager is installed
# (helm install cert-manager jetstack/cert-manager --set installCRDs=true).
#
# Test key path is taken from /tmp/devpod-test-key-path (created by
# hack/e2e-up.sh on first run).

set -euo pipefail

NS=devpods
NAME=m2demo
OWNER=alice
KEY="$(cat /tmp/devpod-test-key-path)"
GW_PORT=2222

PF_PID=""
cleanup() {
    if [[ -n "$PF_PID" ]] && kill -0 "$PF_PID" 2>/dev/null; then
        kill "$PF_PID" 2>/dev/null || true
    fi
}
trap cleanup EXIT

ssh_run() {
    ssh -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -p "$GW_PORT" -i "$KEY" "$OWNER+$NAME@127.0.0.1" -- "$@"
}

start_port_forward() {
    pkill -f "port-forward svc/devpod-gateway" 2>/dev/null || true
    sleep 1
    kubectl -n devpod-system port-forward svc/devpod-gateway "$GW_PORT:22" >/dev/null 2>&1 &
    PF_PID=$!
    local deadline=$((SECONDS + 30))
    until nc -z 127.0.0.1 "$GW_PORT" 2>/dev/null; do
        [[ $SECONDS -lt $deadline ]] || { echo "FAIL: port-forward never came up"; exit 1; }
        sleep 1
    done
}

echo "[1/9] Webhook smoke: reject reserved volume name"
if cat <<EOF | kubectl apply -f - 2>/dev/null
apiVersion: devpod.io/v1alpha1
kind: DevPod
metadata:
  name: webhookreject
  namespace: $NS
spec:
  owner: $OWNER
  running: true
  pod:
    spec:
      containers:
      - name: dev
        image: debian:stable
        volumeMounts:
        - name: devpod-blocked
          mountPath: /x
EOF
then
    echo "FAIL: webhook accepted a reserved-name volumeMount"
    kubectl -n "$NS" delete devpod webhookreject --ignore-not-found
    exit 1
fi

echo "[2/9] Apply DevPod with persistence"
cat <<EOF | kubectl apply -f -
apiVersion: devpod.io/v1alpha1
kind: DevPod
metadata:
  name: $NAME
  namespace: $NS
spec:
  owner: $OWNER
  running: true
  persistence:
    size: 1Gi
    mountPath: /workspace
  pod:
    spec:
      containers:
      - name: dev
        image: debian:stable
        command: ["sleep", "infinity"]
EOF

echo "[3/9] Wait for Running phase"
kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Running devpod/"$NAME" --timeout=120s

echo "[4/9] SSH write marker"
start_port_forward
ssh_run 'echo hello-from-m2 > /workspace/marker && cat /workspace/marker'

echo "[5/9] Hibernate (spec.running=false)"
kubectl -n "$NS" patch devpod "$NAME" --type=merge -p '{"spec":{"running":false}}'
kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Stopped devpod/"$NAME" --timeout=60s

echo "[6/9] Verify Pod is gone (or terminating); PVC remains"
podstate=$(kubectl -n "$NS" get pod "$OWNER-$NAME" -o jsonpath='{.metadata.deletionTimestamp}' 2>/dev/null || true)
if kubectl -n "$NS" get pod "$OWNER-$NAME" >/dev/null 2>&1 && [[ -z "$podstate" ]]; then
    echo "FAIL: Pod is still active (no deletionTimestamp) after hibernate"
    exit 1
fi
kubectl -n "$NS" get pvc "$OWNER-$NAME-home" >/dev/null

echo "[7/9] Resume (spec.running=true)"
kubectl -n "$NS" patch devpod "$NAME" --type=merge -p '{"spec":{"running":true}}'
kubectl -n "$NS" wait --for=jsonpath='{.status.phase}'=Running devpod/"$NAME" --timeout=120s

echo "[8/9] SSH read marker back"
start_port_forward
got=$(ssh_run 'cat /workspace/marker' | tail -1 | tr -d '\r\n')
if [[ "$got" != "hello-from-m2" ]]; then
    echo "FAIL: marker round-trip mismatch; got '$got'"
    exit 1
fi
echo "marker round-trip: '$got'"

echo "[9/9] Delete DevPod; verify PVC is detached and retained"
kubectl -n "$NS" delete devpod "$NAME" --wait=true
ownerrefs=$(kubectl -n "$NS" get pvc "$OWNER-$NAME-home" -o jsonpath='{.metadata.ownerReferences}')
if [[ "$ownerrefs" == *"DevPod"* ]]; then
    echo "FAIL: PVC still has DevPod ownerRef after delete: $ownerrefs"
    exit 1
fi
kubectl -n "$NS" delete pvc "$OWNER-$NAME-home"

echo
echo "OK: M2 demo passed — write/hibernate/resume/read + finalizer detach."
