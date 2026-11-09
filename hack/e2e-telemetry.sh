#!/usr/bin/env bash
# Live-telemetry end-to-end on a real cluster, isolated from any DevPod
# installation already running there (same approach as hack/e2e-audit.sh):
#
#   * no controller: the test DevPod lives in $NS and its status is written
#     by this script, pointing at a box pod that runs the real supervisor
#     (the published image's /opt/devpod plus the freshly built binary);
#   * the gateway runs in a runner pod and the webui in two replica pods
#     (kubectl cp, no image push), electing their leader in $NS and
#     publishing it behind the selectorless Service $INGEST_SVC;
#   * storage = the ClickHouse + Silo pods of hack/test-integration.sh.
#
# Cluster-scoped objects created (and removed at the end): User
# telemetry-e2e, GatewayConfig telemetry-e2e, ClusterRole/Binding
# devpod-telemetry-e2e.
#
#   KUBE_CONTEXT=zjusct bash hack/e2e-telemetry.sh            # run + clean up
#   KUBE_CONTEXT=zjusct E2E_KEEP=1 bash hack/e2e-telemetry.sh # keep objects
#   E2E_APT_MIRROR=mirrors.zju.edu.cn                         # faster apt in the box
#   E2E_SUPERVISOR_IMAGE=mrhaoxx/devpod-supervisor:v0.2.0     # source of /opt/devpod
#   E2E_SKIP_FAILOVER=1                                       # keep both webui replicas (load tests)
#
# Host tools: kubectl, go, node/npm, ssh, htpasswd, curl, openssl, python3.
set -euo pipefail

: "${KUBE_CONTEXT:?set KUBE_CONTEXT}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
NS=devpod-audit-it
DB=devpod_telemetry_e2e
BUCKET=devpod-telemetry-e2e
INGEST_SVC=devpod-telemetry-e2e-ingest
SUP_IMAGE=${E2E_SUPERVISOR_IMAGE:-mrhaoxx/devpod-supervisor:v0.2.0}
PODS=(telemetry-e2e-runner telemetry-e2e-webui-0 telemetry-e2e-webui-1 telemetry-e2e-box)
K=(kubectl --context "$KUBE_CONTEXT")
WORK="$(mktemp -d)"
PIDS=()
log() { printf '\n== %s\n' "$*"; }

diag() {
  for p in telemetry-e2e-webui-0 telemetry-e2e-webui-1; do
    echo "--- $p"; "${K[@]}" -n "$NS" logs "$p" --tail=20 2>/dev/null || true
  done
  echo "--- gateway.log"; "${K[@]}" -n "$NS" exec telemetry-e2e-runner -- tail -10 /gateway.log 2>/dev/null || true
  echo "--- box"; "${K[@]}" -n "$NS" logs telemetry-e2e-box -c box --tail=20 2>/dev/null || true
}
die() { echo "e2e-telemetry: FAIL: $*" >&2; diag >&2; exit 1; }

cleanup() {
  for p in "${PIDS[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null || true; done
  if [ -z "${E2E_KEEP:-}" ]; then
    "${K[@]}" -n "$NS" delete pod "${PODS[@]}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    "${K[@]}" -n "$NS" delete devpod telemetry-e2e-box --ignore-not-found >/dev/null 2>&1 || true
    "${K[@]}" -n "$NS" delete secret/telemetry-e2e-keys secret/telemetry-e2e-box service/"$INGEST_SVC" \
      endpointslice/"$INGEST_SVC"-leader lease/devpod-webui-leader role/devpod-telemetry-e2e \
      rolebinding/devpod-telemetry-e2e serviceaccount/devpod-telemetry-e2e --ignore-not-found >/dev/null 2>&1 || true
    "${K[@]}" delete user telemetry-e2e --ignore-not-found --wait=false >/dev/null 2>&1 || true
    "${K[@]}" delete gatewayconfig/telemetry-e2e clusterrole/devpod-telemetry-e2e clusterrolebinding/devpod-telemetry-e2e \
      --ignore-not-found >/dev/null 2>&1 || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

"${K[@]}" get crd gatewayconfigs.devpod.io -o yaml | grep -q 'audit:' ||
  die "CRDs lack the audit fields; run hack/e2e-audit.sh with E2E_APPLY_CRDS=1 first"

# --- storage -------------------------------------------------------------
log "storage pods"
"${K[@]}" apply -f "$ROOT/hack/integration/fixtures.yaml" >/dev/null
"${K[@]}" -n "$NS" rollout status deploy/clickhouse --timeout=10m >/dev/null
"${K[@]}" -n "$NS" rollout status deploy/silo --timeout=10m >/dev/null

# --- binaries ------------------------------------------------------------
log "building linux/amd64 binaries"
bash "$ROOT/hack/build-webui.sh" >/dev/null
mkdir -p "$WORK/bin"
for b in gateway webui supervisor; do
  (cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$WORK/bin/devpod-$b" "./cmd/$b")
done
git -C "$ROOT" checkout -q web/dist/index.html 2>/dev/null || true

# --- keys ----------------------------------------------------------------
log "keys"
cd "$WORK"
for k in gw_host gw_internal box_host user1; do ssh-keygen -q -t ed25519 -N '' -f "$k"; done
openssl rand -base64 48 >session-key
openssl rand -hex 32 | tr -d '\n' >ingest-key
PW=e2e-$(openssl rand -hex 6)
HASH=$(htpasswd -bnBC 10 "" "$PW" | tr -d ':\n' | sed 's/^\$2y/$2a/')

# --- cluster objects -----------------------------------------------------
log "cluster objects"
"${K[@]}" -n "$NS" delete pod "${PODS[@]}" --ignore-not-found --wait=true --timeout=2m >/dev/null
"${K[@]}" -n "$NS" delete lease/devpod-webui-leader endpointslice/"$INGEST_SVC"-leader --ignore-not-found >/dev/null
"${K[@]}" -n "$NS" create secret generic telemetry-e2e-keys --dry-run=client -o yaml \
  --from-file=gw_host --from-file=gw_internal --from-file=session-key --from-file=ingest-key |
  "${K[@]}" apply -f - >/dev/null
"${K[@]}" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: ServiceAccount
metadata: {name: devpod-telemetry-e2e, namespace: $NS}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: devpod-telemetry-e2e}
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
- apiGroups: [""]
  resources: ["nodes"]
  verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: devpod-telemetry-e2e}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: devpod-telemetry-e2e}
subjects: [{kind: ServiceAccount, name: devpod-telemetry-e2e, namespace: $NS}]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: devpod-telemetry-e2e, namespace: $NS}
rules:
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["get", "list", "watch", "create", "update", "patch"]
- apiGroups: ["discovery.k8s.io"]
  resources: ["endpointslices"]
  verbs: ["get", "list", "watch", "create", "update"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: devpod-telemetry-e2e, namespace: $NS}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: devpod-telemetry-e2e}
subjects: [{kind: ServiceAccount, name: devpod-telemetry-e2e, namespace: $NS}]
---
apiVersion: v1
kind: Service
metadata: {name: $INGEST_SVC, namespace: $NS}
spec:
  ports: [{name: ingest, port: 8081, targetPort: 8081, protocol: TCP}]
---
apiVersion: devpod.io/v1alpha1
kind: GatewayConfig
metadata: {name: telemetry-e2e}
spec:
  devPodNamespace: $NS
  supervisorImage: unused
  hostKeyRef: {name: unused, namespace: $NS}
  internalKeyRef: {name: unused, namespace: $NS}
  audit: {enabled: true}
---
apiVersion: devpod.io/v1alpha1
kind: User
metadata: {name: telemetry-e2e}
spec:
  pubkeys: ["$(cat user1.pub)"]
  passwordHash: "$HASH"
---
apiVersion: devpod.io/v1alpha1
kind: DevPod
metadata: {name: telemetry-e2e-box, namespace: $NS}
spec:
  owner: telemetry-e2e
  running: true
  pod: {spec: {containers: [{name: box, image: debian:trixie-slim}]}}
EOF
UID_=$("${K[@]}" -n "$NS" get devpod telemetry-e2e-box -o jsonpath='{.metadata.uid}')
[ -n "$UID_" ] || die "DevPod has no UID"
# The controller would write these two into the DevPod's host-key Secret.
TOKEN=$(printf %s "$UID_" | openssl dgst -sha256 -hmac "$(cat ingest-key)" | awk '{print $NF}')
printf '%s' "$TOKEN" >ingest-token
printf '{"devpodUID":"%s","ingestURL":"http://%s.%s.svc:8081/ingest/v1/batch"}' "$UID_" "$INGEST_SVC" "$NS" >telemetry.json
cp box_host ssh_host_ed25519_key && cp box_host.pub ssh_host_ed25519_key.pub && cp gw_internal.pub authorized_keys
"${K[@]}" -n "$NS" create secret generic telemetry-e2e-box --dry-run=client -o yaml \
  --from-file=ssh_host_ed25519_key --from-file=ssh_host_ed25519_key.pub --from-file=authorized_keys \
  --from-file=ingest-token --from-file=telemetry.json | "${K[@]}" apply -f - >/dev/null

MIRROR_SED=true
[ -n "${E2E_APT_MIRROR:-}" ] && MIRROR_SED="sed -i 's#deb.debian.org#${E2E_APT_MIRROR}#g' /etc/apt/sources.list.d/debian.sources"
# The webui runs as PID 1 (exec once the binary is copied in) so deleting
# the pod delivers SIGTERM to it and it releases the lease, as in the chart.
webui_pod() { # name
  cat <<EOF
---
apiVersion: v1
kind: Pod
metadata: {name: $1, namespace: $NS, labels: {app.kubernetes.io/name: devpod-webui}}
spec:
  serviceAccountName: devpod-telemetry-e2e
  nodeSelector: {kubernetes.io/arch: amd64}
  terminationGracePeriodSeconds: 10
  containers:
  - name: webui
    image: alpine:3.20
    command: [sh, -c, 'while [ ! -e /tmp/go ]; do sleep 1; done; exec /devpod-webui \$(cat /tmp/args)']
    env:
    - {name: POD_NAME, valueFrom: {fieldRef: {fieldPath: metadata.name}}}
    - {name: POD_IP, valueFrom: {fieldRef: {fieldPath: status.podIP}}}
    volumeMounts: [{name: keys, mountPath: /keys}]
  volumes: [{name: keys, secret: {secretName: telemetry-e2e-keys}}]
EOF
}
{
  webui_pod telemetry-e2e-webui-0
  webui_pod telemetry-e2e-webui-1
  cat <<EOF
---
apiVersion: v1
kind: Pod
metadata: {name: telemetry-e2e-runner, namespace: $NS, labels: {app.kubernetes.io/name: devpod-gateway}}
spec:
  serviceAccountName: devpod-telemetry-e2e
  nodeSelector: {kubernetes.io/arch: amd64}
  containers:
  - name: runner
    image: alpine:3.20
    command: [sleep, infinity]
    volumeMounts: [{name: keys, mountPath: /keys}, {name: spool, mountPath: /spool}]
  volumes:
  - {name: keys, secret: {secretName: telemetry-e2e-keys}}
  - {name: spool, emptyDir: {sizeLimit: 2Gi}}
---
apiVersion: v1
kind: Pod
metadata: {name: telemetry-e2e-box, namespace: $NS, labels: {app: telemetry-e2e-box}}
spec:
  nodeSelector: {kubernetes.io/arch: amd64}
  initContainers:
  - name: bootstrap
    image: $SUP_IMAGE
    command: [/opt/devpod/devpod-supervisor, bootstrap]
    env:
    - {name: SUPERVISOR_BOOTSTRAP_SRC, value: /opt/devpod}
    - {name: SUPERVISOR_BOOTSTRAP_DST, value: /devpod-bin}
    volumeMounts: [{name: bin, mountPath: /devpod-bin}]
  containers:
  - name: box
    image: debian:trixie-slim
    command: [sh, -c]
    args:
    - |
      set -e
      $MIRROR_SED
      apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends procps tmux >/dev/null
      while [ ! -e /opt/devpod/.ready ]; do sleep 1; done
      exec /opt/devpod/devpod-supervisor
    env: [{name: SUPERVISOR_SSHD_PORT, value: "2222"}]
    readinessProbe: {tcpSocket: {port: 2222}, periodSeconds: 3, failureThreshold: 200}
    volumeMounts:
    - {name: bin, mountPath: /opt/devpod}
    - {name: host, mountPath: /etc/devpod, readOnly: true}
  volumes:
  - {name: bin, emptyDir: {sizeLimit: 100Mi}}
  - {name: host, secret: {secretName: telemetry-e2e-box, defaultMode: 256}}
EOF
} | "${K[@]}" apply -f - >/dev/null

"${K[@]}" -n "$NS" wait --for=condition=Ready pod/telemetry-e2e-runner pod/telemetry-e2e-webui-0 pod/telemetry-e2e-webui-1 --timeout=10m >/dev/null
"${K[@]}" -n "$NS" wait --for=jsonpath='{.status.phase}'=Running pod/telemetry-e2e-box --timeout=10m >/dev/null
log "installing the fresh supervisor in the box"
"${K[@]}" -n "$NS" cp "$WORK/bin/devpod-supervisor" telemetry-e2e-box:/opt/devpod/devpod-supervisor.new -c box
"${K[@]}" -n "$NS" exec telemetry-e2e-box -c box -- sh -c \
  'chmod +x /opt/devpod/devpod-supervisor.new && mv /opt/devpod/devpod-supervisor.new /opt/devpod/devpod-supervisor && touch /opt/devpod/.ready'
"${K[@]}" -n "$NS" wait --for=condition=Ready pod/telemetry-e2e-box --timeout=15m >/dev/null
BOX_IP=$("${K[@]}" -n "$NS" get pod telemetry-e2e-box -o jsonpath='{.status.podIP}')
"${K[@]}" -n "$NS" patch devpod telemetry-e2e-box --subresource=status --type=merge \
  -p "{\"status\":{\"phase\":\"Running\",\"endpoint\":\"$BOX_IP:2222\"}}" >/dev/null

# --- start gateway + webuis ----------------------------------------------
log "starting the gateway and two webui replicas"
STORE="--audit-clickhouse-url=clickhouse://clickhouse.$NS.svc:9000 --audit-clickhouse-database=$DB --audit-clickhouse-credentials-dir=/creds/ch --audit-s3-endpoint=http://silo.$NS.svc:9000 --audit-s3-bucket=$BUCKET --audit-s3-credentials-dir=/creds/s3"
CREDS='mkdir -p /creds/ch /creds/s3; printf it >/creds/ch/username; printf it-password >/creds/ch/password
  printf devpod >/creds/s3/access-key; printf devpod-secret >/creds/s3/secret-key'
for p in telemetry-e2e-runner telemetry-e2e-webui-0 telemetry-e2e-webui-1; do
  b=webui; [ "$p" = telemetry-e2e-runner ] && b=gateway
  "${K[@]}" -n "$NS" cp "$WORK/bin/devpod-$b" "$p:/devpod-$b"
  "${K[@]}" -n "$NS" exec "$p" -- sh -c "chmod +x /devpod-$b; $CREDS"
done
"${K[@]}" -n "$NS" exec telemetry-e2e-runner -- sh -c 'mkdir -p /gwkeys/internal
  cp /keys/gw_host /gwkeys/ssh_host_ed25519_key; cp /keys/gw_internal /gwkeys/internal/ssh_host_ed25519_key'
# A fresh database each run: kept runs reuse the DevPod (same UID), and
# old rows would satisfy the checks below before this run's agent reports.
"${K[@]}" -n "$NS" exec deploy/clickhouse -- clickhouse-client --user it --password it-password --query "DROP DATABASE IF EXISTS $DB SYNC"
"${K[@]}" -n "$NS" exec telemetry-e2e-webui-0 -- sh -c "/devpod-webui migrate $STORE --create-bucket --manage-lifecycle --retention-days=7"
"${K[@]}" -n "$NS" exec telemetry-e2e-runner -- sh -c "POD_NAME=telemetry-e2e-runner nohup /devpod-gateway --listen=:2222 --host-key-dir=/gwkeys --devpod-namespace=$NS --gateway-config-name=telemetry-e2e --metrics-bind-address=:9100 --audit-spool-dir=/spool --audit-spool-max-bytes=1536Mi $STORE >/gateway.log 2>&1 &"
WEBUI_ARGS="--listen=:8080 --session-key-file=/keys/session-key --password-auth=true --admins=telemetry-e2e --devpod-namespace=$NS --gateway-config-name=telemetry-e2e --kore=off --external-url=http://127.0.0.1:18080 --ingest-key-file=/keys/ingest-key --leader-election-namespace=$NS --ingest-service=$INGEST_SVC $STORE"
for p in telemetry-e2e-webui-0 telemetry-e2e-webui-1; do
  "${K[@]}" -n "$NS" exec "$p" -- sh -c "printf '%s' '$WEBUI_ARGS' >/tmp/args && touch /tmp/go"
done

forward() { # pod local remote
  "${K[@]}" -n "$NS" port-forward "pod/$1" "$2:$3" >"$WORK/pf-$2.log" 2>&1 &
  PIDS+=("$!")
  disown "$!"
  for _ in $(seq 1 50); do grep -q "Forwarding from 127.0.0.1:$2" "$WORK/pf-$2.log" && return; sleep 0.2; done
  die "port-forward $1 $2"
}
forward telemetry-e2e-runner 12222 2222
forward telemetry-e2e-webui-0 18080 8080
forward telemetry-e2e-webui-1 18081 8080

q() { "${K[@]}" -n "$NS" exec deploy/clickhouse -- clickhouse-client --user it --password it-password --query "$1"; }
holder() { "${K[@]}" -n "$NS" get lease devpod-webui-leader -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true; }
waitfor() { # seconds description command...
  local t=$1 what=$2; shift 2
  for _ in $(seq 1 "$t"); do "$@" >/dev/null 2>&1 && return 0; sleep 1; done
  die "timed out after ${t}s: $what"
}
has_leader() { [ -n "$(holder)" ]; }
rows() { q "SELECT count() FROM $DB.metrics_container_1s WHERE devpod_uid = '$UID_'"; }
have_rows() { [ "$(rows)" -ge 5 ]; }

# --- 1. samples reach ClickHouse -----------------------------------------
log "leader election and first samples"
waitfor 60 "a webui leader" has_leader
waitfor 90 "metrics_container_1s rows for the box" have_rows
echo "leader $(holder), $(rows) container samples"

SSHO=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o IdentitiesOnly=yes)
LOGIN=telemetry-e2e+box

# --- 2. interactive session: hooks and a daemonised process ---------------
log "interactive session"
printf 'cd /tmp\nfalse\ntmux new -d "sleep 600"\nsleep 2\nexit\n' |
  ssh "${SSHO[@]}" -tt -i user1 -p 12222 "$LOGIN@127.0.0.1" >"$WORK/interactive.log" 2>&1 || true
hook() { [ "$(q "SELECT count() FROM $DB.commands FINAL WHERE devpod_uid = '$UID_' AND source = 'shell_hook' AND $1")" -ge 1 ]; }
CD="trimBoth(cmdline) = 'cd /tmp'"
FALSE="trimBoth(cmdline) = 'false' AND exit_code = 1"
for _ in $(seq 1 30); do hook "$CD" && hook "$FALSE" && break; sleep 1; done
hook "$CD" || die "no shell_hook row for 'cd /tmp': $(q "SELECT cmdline, exit_code FROM $DB.commands FINAL WHERE devpod_uid = '$UID_' FORMAT TSV")"
hook "$FALSE" || die "no shell_hook row for 'false' with exit 1"

# --- 3. live API sees a CPU burner within seconds --------------------------
log "live CPU"
B0=http://127.0.0.1:18080
curl -fsS -c "$WORK/jar" -H 'Content-Type: application/json' -d "{\"username\":\"telemetry-e2e\",\"password\":\"$PW\"}" $B0/api/auth/password >/dev/null
cat >"$WORK/watch.py" <<'PY'
import json, sys, time, urllib.request, http.cookiejar
url, jar, name, limit = sys.argv[1], sys.argv[2], sys.argv[3], float(sys.argv[4])
cj = http.cookiejar.MozillaCookieJar(jar); cj.load(ignore_discard=True, ignore_expires=True)
op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cj))
start = None
with op.open(url, timeout=30) as r:
    print("ready", flush=True)
    for raw in r:
        line = raw.decode().strip()
        if not line.startswith("data: "):
            continue
        ev = json.loads(line[6:])
        for it in ev.get("items", []):
            if it.get("name") != name:
                continue
            print("event", repr(time.time()), ev.get("ts"), it.get("cpu"), flush=True)
            if (it.get("cpu") or 0) > limit:
                print("cpu", it["cpu"], "at", repr(time.time()), flush=True)
                sys.exit(0)
PY
python3 "$WORK/watch.py" "$B0/api/live/devpods?scope=mine" "$WORK/jar" telemetry-e2e-box 0.5 >"$WORK/watch.log" 2>&1 &
WATCH=$!
PIDS+=("$WATCH")
waitfor 15 "live stream connected" grep -q ready "$WORK/watch.log"
# The clock starts when the burner runs (it says so), not before the ssh
# connection through the port-forward and the gateway.
ssh "${SSHO[@]}" -i user1 -p 12222 "$LOGIN@127.0.0.1" 'echo started; timeout 12 sh -c "while :; do :; done"' >"$WORK/burner.log" 2>&1 &
PIDS+=("$!")
for _ in $(seq 1 300); do grep -q started "$WORK/burner.log" && break; sleep 0.05; done
grep -q started "$WORK/burner.log" || die "the burner never started: $(cat "$WORK/burner.log")"
T0=$(python3 -c 'import time; print(repr(time.time()))')
for _ in $(seq 1 110); do kill -0 "$WATCH" 2>/dev/null || break; sleep 0.1; done
if kill -0 "$WATCH" 2>/dev/null; then kill "$WATCH"; die "the live stream never showed the burner above 0.5 cores in 11 s"; fi
wait "$WATCH" || die "the live stream never showed the burner above 0.5 cores: $(cat "$WORK/watch.log")"
T1=$(awk '/^cpu/ {print $4}' "$WORK/watch.log")
LAT=$(python3 -c "print(round(max($T1 - $T0, 0), 1))")
python3 -c "import sys; sys.exit(0 if $T1 - $T0 <= 3 else 1)" ||
  die "live CPU took ${LAT}s after the burner started (T0=$T0), want ≤ 3; stream: $(tail -15 "$WORK/watch.log")"
echo "burner visible ${LAT}s after it started ($(grep cpu "$WORK/watch.log"))"

# --- 4. kubectl exec is an untracked entry --------------------------------
log "untracked entry"
"${K[@]}" -n "$NS" exec telemetry-e2e-box -c box -- sleep 3
alert() { [ "$(q "SELECT count() FROM $DB.alerts WHERE devpod_uid = '$UID_' AND kind = 'untracked_entry'")" -ge 1 ]; }
for _ in $(seq 1 30); do alert && break; sleep 1; done
alert || die "kubectl exec raised no untracked_entry alert"

# --- 5. the daemonised sleep belongs to the SSH session --------------------
log "attribution of 'sleep 600' (may need one backfill minute)"
attributed() {
  [ "$(q "SELECT count() FROM $DB.processes FINAL WHERE devpod_uid = '$UID_' AND startsWith(cmdline, 'sleep 600')
     AND session_id IN (SELECT session_id FROM $DB.sessions FINAL WHERE devpod_uid = '$UID_' AND user = 'telemetry-e2e')")" -ge 1 ]
}
for _ in $(seq 1 150); do attributed && break; sleep 1; done
attributed || die "sleep 600 has no session: $(q "SELECT session_id, attribution, peer FROM $DB.processes FINAL WHERE devpod_uid = '$UID_' AND startsWith(cmdline, 'sleep 600') FORMAT TSV")"

# --- 6. leader failover ----------------------------------------------------
if [ -n "${E2E_SKIP_FAILOVER:-}" ]; then echo; echo "e2e-telemetry: OK (failover skipped)"; exit 0; fi
log "leader failover"
OLD=$(holder)
case "$OLD" in telemetry-e2e-webui-0) NEW=telemetry-e2e-webui-1 ;; telemetry-e2e-webui-1) NEW=telemetry-e2e-webui-0 ;; *) die "unexpected leader '$OLD'" ;; esac
TD=$(date -u +%s)
"${K[@]}" -n "$NS" delete pod "$OLD" --wait=false >/dev/null
# wall-clock bounded: each query is a kubectl exec (~1 s)
TL="" TR=""
while [ $(( $(date -u +%s) - TD )) -le 20 ]; do
  [ -z "$TL" ] && [ "$(holder)" = "$NEW" ] && TL=$(( $(date -u +%s) - TD ))
  # a sample collected after the deletion has been written
  if [ -n "$TL" ] && [ "$(q "SELECT count() FROM $DB.metrics_container_1s WHERE devpod_uid = '$UID_' AND ts > toDateTime($TD + 1)")" -ge 1 ]; then
    TR=$(( $(date -u +%s) - TD )); break
  fi
  sleep 0.5
done
[ -n "$TL" ] || die "$NEW did not take the lease within 20 s"
[ -n "$TR" ] || die "no new samples within 20 s of deleting the leader (lease taken after ${TL}s)"
sleep $(( 45 - TR ))
# every second around the failover is present (buffered and resent)
GOT=$(q "SELECT uniqExact(toStartOfSecond(ts)) FROM $DB.metrics_container_1s WHERE devpod_uid = '$UID_' AND ts >= toDateTime($TD - 10) AND ts < toDateTime($TD + 30)")
[ "$GOT" -ge 38 ] || die "only $GOT of 40 seconds around the failover have a sample"
echo "failover $OLD → $NEW: lease after ${TL}s, samples written again after ${TR}s, $GOT/40 seconds present"

echo
echo "e2e-telemetry: OK"
