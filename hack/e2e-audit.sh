#!/usr/bin/env bash
# Session-audit end-to-end on a real cluster, isolated from any DevPod
# installation already running there:
#
#   * no second controller: the test DevPod lives in $NS and its status is
#     written by this script, pointing at a throwaway sshd pod;
#   * the gateway and webui binaries run inside a runner pod (kubectl cp,
#     no image push) and read their own GatewayConfig "audit-e2e";
#   * storage = the ClickHouse + Silo pods of hack/test-integration.sh.
#
# Cluster-scoped objects created (and removed at the end): User audit-e2e,
# User audit-e2e2, GatewayConfig audit-e2e, ClusterRole/Binding
# devpod-audit-e2e. The GatewayConfig and User CRDs must already carry the
# audit fields; E2E_APPLY_CRDS=1 server-side-applies just those two first
# (additive schema change).
#
#   KUBE_CONTEXT=zjusct bash hack/e2e-audit.sh          # run + clean up
#   KUBE_CONTEXT=zjusct E2E_KEEP=1 bash hack/e2e-audit.sh  # keep objects for poking
#
# Host tools: kubectl, go, node/npm, ssh, sftp, scp, socat, lrzsz, htpasswd, curl, openssl.
set -euo pipefail

: "${KUBE_CONTEXT:?set KUBE_CONTEXT}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
NS=devpod-audit-it
DB=devpod_audit_e2e
BUCKET=devpod-audit-e2e
K=(kubectl --context "$KUBE_CONTEXT")
WORK="$(mktemp -d)"
PIDS=()
log() { printf '\n== %s\n' "$*"; }
die() { echo "e2e-audit: FAIL: $*" >&2; exit 1; }

cleanup() {
  for p in "${PIDS[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null || true; done
  if [ -z "${E2E_KEEP:-}" ]; then
    "${K[@]}" -n "$NS" delete pod audit-e2e-runner audit-e2e-box --ignore-not-found --wait=false >/dev/null 2>&1 || true
    "${K[@]}" -n "$NS" delete devpod audit-e2e-box --ignore-not-found >/dev/null 2>&1 || true
    "${K[@]}" delete user audit-e2e audit-e2e2 --ignore-not-found --wait=false >/dev/null 2>&1 || true
    "${K[@]}" delete gatewayconfig audit-e2e clusterrole/devpod-audit-e2e clusterrolebinding/devpod-audit-e2e --ignore-not-found >/dev/null 2>&1 || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

# --- CRDs ----------------------------------------------------------------
if [ -n "${E2E_APPLY_CRDS:-}" ]; then
  log "applying the GatewayConfig + User CRDs (additive: spec.audit, spec.auditPolicy)"
  "${K[@]}" apply --server-side --force-conflicts \
    -f "$ROOT/config/crd/bases/devpod.io_gatewayconfigs.yaml" -f "$ROOT/config/crd/bases/devpod.io_users.yaml" >/dev/null
fi
"${K[@]}" get crd gatewayconfigs.devpod.io -o yaml | grep -q 'audit:' ||
  die "CRDs lack the audit fields; rerun with E2E_APPLY_CRDS=1"

# --- storage -------------------------------------------------------------
log "storage pods"
"${K[@]}" apply -f "$ROOT/hack/integration/fixtures.yaml" >/dev/null
"${K[@]}" -n "$NS" rollout status deploy/clickhouse --timeout=10m >/dev/null
"${K[@]}" -n "$NS" rollout status deploy/silo --timeout=10m >/dev/null

# --- binaries ------------------------------------------------------------
log "building linux/amd64 binaries"
bash "$ROOT/hack/build-webui.sh" >/dev/null
mkdir -p "$WORK/bin"
(cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$WORK/bin/devpod-gateway" ./cmd/gateway)
(cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$WORK/bin/devpod-webui" ./cmd/webui)
git -C "$ROOT" checkout -q web/dist/index.html 2>/dev/null || true

# --- keys and secrets ----------------------------------------------------
log "keys"
cd "$WORK"
ssh-keygen -q -t ed25519 -N '' -f gw_host
ssh-keygen -q -t ed25519 -N '' -f gw_internal
ssh-keygen -q -t ed25519 -N '' -f box_host
ssh-keygen -q -t ed25519 -N '' -f user1
ssh-keygen -q -t ed25519 -N '' -f user2
openssl rand -base64 48 >session-key
PW=e2e-$(openssl rand -hex 6)
HASH=$(htpasswd -bnBC 10 "" "$PW" | tr -d ':\n' | sed 's/^\$2y/$2a/')

# --- RBAC, GatewayConfig, users, sshd box, runner ------------------------
log "cluster objects"
"${K[@]}" -n "$NS" create secret generic audit-e2e-keys --dry-run=client -o yaml \
  --from-file=gw_host --from-file=gw_internal --from-file=box_host --from-file=box_host.pub \
  --from-file=gw_internal.pub --from-file=session-key | "${K[@]}" apply -f - >/dev/null

"${K[@]}" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: ServiceAccount
metadata: {name: devpod-audit-e2e, namespace: $NS}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: devpod-audit-e2e}
rules:
- apiGroups: ["devpod.io"]
  resources: ["users"]
  verbs: ["get", "list", "watch", "update", "patch"]
- apiGroups: ["devpod.io"]
  resources: ["devpods", "gatewayconfigs", "devpodtemplates"]
  verbs: ["get", "list", "watch"]
- apiGroups: [""]
  resources: ["pods", "events"]
  verbs: ["get", "list", "watch", "create", "patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: devpod-audit-e2e}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: devpod-audit-e2e}
subjects: [{kind: ServiceAccount, name: devpod-audit-e2e, namespace: $NS}]
---
apiVersion: devpod.io/v1alpha1
kind: GatewayConfig
metadata: {name: audit-e2e}
spec:
  devPodNamespace: $NS
  supervisorImage: unused
  hostKeyRef: {name: unused, namespace: $NS}
  internalKeyRef: {name: unused, namespace: $NS}
  audit: {enabled: true}
---
apiVersion: devpod.io/v1alpha1
kind: User
metadata: {name: audit-e2e}
spec:
  pubkeys: ["$(cat user1.pub)"]
  passwordHash: "$HASH"
---
apiVersion: devpod.io/v1alpha1
kind: User
metadata: {name: audit-e2e2}
spec:
  pubkeys: ["$(cat user2.pub)"]
  passwordHash: "$HASH"
---
apiVersion: v1
kind: Pod
metadata: {name: audit-e2e-box, namespace: $NS, labels: {app: audit-e2e-box}}
spec:
  nodeSelector: {kubernetes.io/arch: amd64}
  containers:
  - name: box
    image: debian:trixie-slim
    command: [sh, -c]
    args:
    - |
      set -e
      apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends openssh-server lrzsz >/dev/null
      mkdir -p /run/sshd /root/.ssh
      cp /keys/box_host /etc/ssh/ssh_host_ed25519_key && chmod 600 /etc/ssh/ssh_host_ed25519_key
      cp /keys/gw_internal.pub /root/.ssh/authorized_keys && chmod 600 /root/.ssh/authorized_keys
      head -c 1048576 /dev/urandom >/root/z.bin
      head -c 1048576 /dev/urandom >/root/get.bin
      exec /usr/sbin/sshd -D -e -p 2222 -h /etc/ssh/ssh_host_ed25519_key \
        -o PermitRootLogin=prohibit-password -o "Subsystem sftp /usr/lib/openssh/sftp-server"
    readinessProbe: {tcpSocket: {port: 2222}, periodSeconds: 3, failureThreshold: 100}
    volumeMounts: [{name: keys, mountPath: /keys}]
  volumes: [{name: keys, secret: {secretName: audit-e2e-keys}}]
---
apiVersion: v1
kind: Pod
metadata: {name: audit-e2e-runner, namespace: $NS, labels: {app.kubernetes.io/name: devpod-gateway}}
spec:
  serviceAccountName: devpod-audit-e2e
  nodeSelector: {kubernetes.io/arch: amd64}
  containers:
  - name: runner
    image: alpine:3.20
    command: [sleep, infinity]
    volumeMounts: [{name: keys, mountPath: /keys}, {name: spool, mountPath: /spool}]
  volumes:
  - {name: keys, secret: {secretName: audit-e2e-keys}}
  - {name: spool, emptyDir: {sizeLimit: 2Gi}}
EOF
"${K[@]}" -n "$NS" wait --for=condition=Ready pod/audit-e2e-box pod/audit-e2e-runner --timeout=5m >/dev/null
BOX_IP=$("${K[@]}" -n "$NS" get pod audit-e2e-box -o jsonpath='{.status.podIP}')

"${K[@]}" apply -f - >/dev/null <<EOF
apiVersion: devpod.io/v1alpha1
kind: DevPod
metadata: {name: audit-e2e-box, namespace: $NS}
spec:
  owner: audit-e2e
  running: true
  pod: {spec: {containers: [{name: box, image: alpine:3.20}]}}
EOF
"${K[@]}" -n "$NS" patch devpod audit-e2e-box --subresource=status --type=merge \
  -p "{\"status\":{\"phase\":\"Running\",\"endpoint\":\"$BOX_IP:2222\"}}" >/dev/null

# --- start gateway + webui in the runner ---------------------------------
log "starting gateway and webui in the runner pod"
R=("${K[@]}" -n "$NS" exec audit-e2e-runner --)
"${K[@]}" -n "$NS" cp "$WORK/bin/devpod-gateway" audit-e2e-runner:/devpod-gateway
"${K[@]}" -n "$NS" cp "$WORK/bin/devpod-webui" audit-e2e-runner:/devpod-webui
"${R[@]}" sh -c 'chmod +x /devpod-*; mkdir -p /creds/ch /creds/s3 /gwkeys/internal
  printf it >/creds/ch/username; printf it-password >/creds/ch/password
  printf devpod >/creds/s3/access-key; printf devpod-secret >/creds/s3/secret-key
  cp /keys/gw_host /gwkeys/ssh_host_ed25519_key; cp /keys/gw_internal /gwkeys/internal/ssh_host_ed25519_key'
STORE="--audit-clickhouse-url=clickhouse://clickhouse.$NS.svc:9000 --audit-clickhouse-database=$DB --audit-clickhouse-credentials-dir=/creds/ch --audit-s3-endpoint=http://silo.$NS.svc:9000 --audit-s3-bucket=$BUCKET --audit-s3-credentials-dir=/creds/s3"
"${R[@]}" sh -c "/devpod-webui migrate $STORE --create-bucket --manage-lifecycle --retention-days=7"
"${R[@]}" sh -c "POD_NAME=audit-e2e-runner nohup /devpod-gateway --listen=:2222 --host-key-dir=/gwkeys --devpod-namespace=$NS --gateway-config-name=audit-e2e --metrics-bind-address=:9100 --audit-spool-dir=/spool $STORE >/gateway.log 2>&1 &"
"${R[@]}" sh -c "nohup /devpod-webui --listen=:8080 --session-key-file=/keys/session-key --password-auth=true --admins=audit-e2e --devpod-namespace=$NS --gateway-config-name=audit-e2e --kore=off --external-url=http://127.0.0.1:18080 $STORE >/webui.log 2>&1 &"
sleep 5

"${K[@]}" -n "$NS" port-forward pod/audit-e2e-runner 12222:2222 18080:8080 >"$WORK/pf.log" 2>&1 &
PIDS+=("$!")
for _ in $(seq 1 50); do grep -q 'Forwarding from 127.0.0.1:18080' "$WORK/pf.log" && break; sleep 0.2; done

SSHO=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o IdentitiesOnly=yes)
LOGIN=audit-e2e+box

# --- 1. exec with exit code ----------------------------------------------
log "exec"
set +e
ssh "${SSHO[@]}" -i user1 -p 12222 "$LOGIN@127.0.0.1" 'echo hi; exit 3'
rc=$?
set -e
[ "$rc" -eq 3 ] || die "exec exit status $rc, want 3"

# --- 2. transfers: sftp, legacy scp, ZMODEM ------------------------------
log "sftp / scp -O / zmodem"
head -c 1048576 /dev/urandom >up.bin
printf 'put up.bin /root/up.bin\nget /root/get.bin got.bin\n' >batch
sftp "${SSHO[@]}" -i user1 -P 12222 -b batch "$LOGIN@127.0.0.1" >/dev/null
scp -O "${SSHO[@]}" -i user1 -P 12222 up.bin "$LOGIN@127.0.0.1:/root/scp.bin"
mkdir -p zrecv
(cd zrecv && socat EXEC:"ssh ${SSHO[*]} -i $WORK/user1 -tt -p 12222 $LOGIN@127.0.0.1 sz -q /root/z.bin" EXEC:"rz -q -y") || true
[ -s zrecv/z.bin ] || die "zmodem download produced no file"

# --- 3. policy: download denied ------------------------------------------
log "policy: download=Deny"
"${K[@]}" patch user audit-e2e --type=merge -p '{"spec":{"auditPolicy":{"download":"Deny"}}}' >/dev/null
sleep 3
set +e
out=$(printf 'get /root/get.bin denied.bin\n' | sftp "${SSHO[@]}" -i user1 -P 12222 -b - "$LOGIN@127.0.0.1" 2>&1)
set -e
echo "$out" | grep -q 'blocked by DevPod audit policy: downloads disabled' || die "sftp get was not blocked: $out"
"${K[@]}" patch user audit-e2e --type=json -p '[{"op":"remove","path":"/spec/auditPolicy"}]' >/dev/null

# --- 4. ClickHouse rows ---------------------------------------------------
log "waiting for upload, checking ClickHouse"
sleep 12
q() { "${K[@]}" -n "$NS" exec deploy/clickhouse -- clickhouse-client --user it --password it-password --query "$1"; }
[ "$(q "SELECT count() FROM $DB.commands FINAL WHERE cmdline = 'echo hi; exit 3' AND exit_code = 3")" -ge 1 ] || die "exec command row"
for proto in sftp scp zmodem; do
  n=$(q "SELECT count() FROM $DB.file_events WHERE protocol = '$proto' AND sha256 != '' AND complete")
  [ "$n" -ge 1 ] || die "no complete $proto file event"
done
[ "$(q "SELECT count() FROM $DB.policy_events WHERE rule = 'download'")" -ge 1 ] || die "policy event"

# --- 5. webui API ---------------------------------------------------------
log "webui API"
B=http://127.0.0.1:18080
curl -fsS -c j1 -H 'Content-Type: application/json' -d "{\"username\":\"audit-e2e\",\"password\":\"$PW\"}" $B/api/auth/password >/dev/null
curl -fsS -c j2 -H 'Content-Type: application/json' -d "{\"username\":\"audit-e2e2\",\"password\":\"$PW\"}" $B/api/auth/password >/dev/null
SID=$(curl -fsS -b j1 "$B/api/audit/sessions?user=audit-e2e" | python3 -c 'import sys,json; print(json.load(sys.stdin)["items"][0]["session_id"])')
[ -n "$SID" ] || die "no sessions listed"
CH=$(curl -fsS -b j1 "$B/api/audit/sessions/$SID" | python3 -c 'import sys,json; print(json.load(sys.stdin)["channels"][0]["channel_id"])')
curl -fsS -b j1 "$B/api/audit/sessions/$SID/channels/$CH/cast" | head -1 | grep -q '"version":2' || die "cast"
code=$(curl -s -o /dev/null -w '%{http_code}' -b j2 "$B/api/audit/sessions/$SID")
[ "$code" = 404 ] || die "other user got $code for audit-e2e's session"

echo
echo "e2e-audit: OK"
