#!/usr/bin/env bash
# Web UI e2e: fake IdP + webui on the cluster stood up by e2e-up.sh,
# then curl-driven login → create → hibernate → quota-409 → kore-403 →
# delete through the public API.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${REPO_ROOT}"
NS=devpod-system

# --- cluster / arch detection (mirrors e2e-up.sh) --------------------------
CTX="$(kubectl config current-context 2>/dev/null || echo "")"
case "$CTX" in
  kind-*) USE_KIND=1; CLUSTER="${CTX#kind-}" ;;
  *)      USE_KIND=0 ;;
esac
NODE_ARCH=$(kubectl get node -o jsonpath='{.items[0].status.nodeInfo.architecture}' 2>/dev/null || echo "$(uname -m)")
case "$NODE_ARCH" in
  amd64|x86_64) GOARCH=amd64; DOCKER_PLATFORM=linux/amd64 ;;
  arm64|aarch64) GOARCH=arm64; DOCKER_PLATFORM=linux/arm64 ;;
  *) echo "unsupported node arch: $NODE_ARCH" >&2; exit 1 ;;
esac

# --- build images (SPA on host, Go cross-compiled, thin images) ------------
bash hack/build-webui.sh
mkdir -p bin
CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go build -o bin/devpod-webui ./cmd/webui
CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go build -o bin/fakeidp ./test/fakeidp

docker build --platform="$DOCKER_PLATFORM" -t devpod-webui:e2e -f - . <<EOF
FROM gcr.io/distroless/static
COPY bin/devpod-webui /usr/local/bin/devpod-webui
ENTRYPOINT ["/usr/local/bin/devpod-webui"]
EOF
docker build --platform="$DOCKER_PLATFORM" -t devpod-fakeidp:e2e -f - . <<EOF
FROM gcr.io/distroless/static
COPY bin/fakeidp /usr/local/bin/fakeidp
ENTRYPOINT ["/usr/local/bin/fakeidp"]
EOF

if [ "$USE_KIND" = "1" ]; then
  kind load docker-image --name "${CLUSTER}" devpod-webui:e2e devpod-fakeidp:e2e
fi

# CRDs may have changed since e2e-up.sh ran.
kubectl apply --server-side --force-conflicts -f deploy/chart/crds/ >/dev/null

# --- fake IdP ---------------------------------------------------------------
kubectl -n "$NS" apply -f - <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata: {name: fakeidp, labels: {app: fakeidp}}
spec:
  replicas: 1
  selector: {matchLabels: {app: fakeidp}}
  template:
    metadata: {labels: {app: fakeidp}}
    spec:
      containers:
      - name: fakeidp
        image: devpod-fakeidp:e2e
        imagePullPolicy: IfNotPresent
        args: ["--issuer=http://fakeidp.devpod-system.svc:9998"]
        ports: [{containerPort: 9998}]
---
apiVersion: v1
kind: Service
metadata: {name: fakeidp}
spec:
  selector: {app: fakeidp}
  ports: [{port: 9998}]
EOF
kubectl -n "$NS" rollout status deploy/fakeidp --timeout=120s

# --- secrets + chart --------------------------------------------------------
kubectl -n "$NS" delete secret webui-oauth webui-session --ignore-not-found >/dev/null
kubectl -n "$NS" create secret generic webui-oauth --from-literal=client-secret=e2e-secret
kubectl -n "$NS" create secret generic webui-session --from-literal=session-key="$(head -c 48 /dev/urandom | base64)"

helm upgrade devpod ./deploy/chart --reuse-values \
  --set image.webui.repository=devpod-webui --set image.webui.tag=e2e \
  --set webui.enabled=true \
  --set webui.replicas=1 \
  --set webui.baseURL=http://127.0.0.1:18080 \
  --set webui.userPrefix=gl- \
  --set webui.kore=off \
  --set webui.gitlab.issuerURL=http://fakeidp.devpod-system.svc:9998 \
  --set webui.gitlab.clientID=webui-client \
  --set webui.gitlab.clientSecretSecret.name=webui-oauth \
  --set webui.sessionKeySecret.name=webui-session \
  --set-json 'webui.defaultQuota={"maxDevPods":2,"compute":{"cpu":"4","memory":"8Gi"},"storage":"10Gi"}' >/dev/null
kubectl -n "$NS" rollout restart deploy/devpod-webui
kubectl -n "$NS" rollout status deploy/devpod-webui --timeout=180s

kubectl -n "$NS" port-forward svc/devpod-webui 18080:80 >/dev/null 2>&1 &
PF_PID=$!
trap 'kill $PF_PID 2>/dev/null || true' EXIT
sleep 2

BASE=http://127.0.0.1:18080
JAR="$(mktemp)"

# --- login ------------------------------------------------------------------
# /auth/login 302s to the (cluster-internal) fake IdP, which always
# issues code "e2e-code" — so extract state and hit the callback
# directly instead of following the redirect.
LOGIN_LOC=$(curl -sf -o /dev/null -w '%{redirect_url}' -c "$JAR" "$BASE/auth/login")
STATE=$(printf '%s' "$LOGIN_LOC" | sed -n 's/.*[?&]state=\([^&]*\).*/\1/p')
[ -n "$STATE" ] || { echo "FAIL: no state in login redirect: $LOGIN_LOC"; exit 1; }
curl -sf -o /dev/null -b "$JAR" -c "$JAR" "$BASE/auth/callback?code=e2e-code&state=$STATE"
grep -q devpod_session "$JAR" || { echo "FAIL: no session cookie"; exit 1; }
echo "login: OK (gl-alice)"

api() { curl -s -b "$JAR" -H 'Content-Type: application/json' "$@"; }

# --- exercise -----------------------------------------------------------------
OUT=$(api -X POST "$BASE/api/devpods" -d '{"name":"e2e","image":"ubuntu:24.04","cpu":"1","memory":"1Gi"}')
echo "$OUT" | grep -q '"gl-alice-e2e"' || { echo "FAIL: create: $OUT"; exit 1; }
echo "create: OK"

kubectl -n devpods wait --for=jsonpath='{.status.phase}'=Running devpod/gl-alice-e2e --timeout=180s
echo "running: OK"

api -X PATCH "$BASE/api/devpods/gl-alice-e2e" -d '{"running":false}' >/dev/null
kubectl -n devpods wait --for=jsonpath='{.status.phase}'=Stopped devpod/gl-alice-e2e --timeout=120s
echo "hibernate: OK"

# quota: cpu limit is 4 → a 6-cpu pod must be refused with 409
CODE=$(api -o /dev/null -w '%{http_code}' -X POST "$BASE/api/devpods" \
  -d '{"name":"big","image":"ubuntu:24.04","cpu":"6","memory":"1Gi"}')
[ "$CODE" = "409" ] || { echo "FAIL: quota expected 409, got $CODE"; exit 1; }
echo "quota 409: OK"

# kore stamping invariant: raw annotation must be rejected with 403
YAML_PAYLOAD='{"yaml":"apiVersion: devpod.io/v1alpha1\nkind: DevPod\nmetadata:\n  name: gl-alice-pin\nspec:\n  owner: gl-alice\n  running: true\n  pod:\n    metadata:\n      annotations:\n        kore.zjusct.io/pin: \"true\"\n    spec:\n      containers:\n      - name: dev\n        image: ubuntu:24.04\n        resources:\n          limits: {cpu: \"1\", memory: \"1Gi\"}\n"}'
CODE=$(api -o /dev/null -w '%{http_code}' -X POST "$BASE/api/devpods" -d "$YAML_PAYLOAD")
[ "$CODE" = "403" ] || { echo "FAIL: kore rejection expected 403, got $CODE"; exit 1; }
echo "kore 403: OK"

CODE=$(api -o /dev/null -w '%{http_code}' -X DELETE "$BASE/api/devpods/gl-alice-e2e")
[ "$CODE" = "204" ] || { echo "FAIL: delete expected 204, got $CODE"; exit 1; }
echo "delete: OK"

echo "e2e-webui: PASS"
