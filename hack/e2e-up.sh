#!/usr/bin/env bash
set -euo pipefail

CLUSTER="${CLUSTER:-devpod-e2e}"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${REPO_ROOT}"

# Detect target. If the current context is a kind cluster, ensure it
# exists and load images into the kind node. Otherwise (orbstack etc.),
# assume the cluster is up and shares the docker daemon's image store.
CTX="$(kubectl config current-context 2>/dev/null || echo "")"
case "$CTX" in
  kind-*)
    USE_KIND=1
    CLUSTER="${CTX#kind-}"
    ;;
  *)
    USE_KIND=0
    ;;
esac

if [ "$USE_KIND" = "1" ]; then
  if ! kind get clusters | grep -q "^${CLUSTER}$"; then
    kind create cluster --name "${CLUSTER}" --config test/e2e/kind-config.yaml
  fi
fi

# Detect kubelet/node arch (kind = node arch may differ from host;
# orbstack = node = host arch).
NODE_ARCH=$(kubectl get node -o jsonpath='{.items[0].status.nodeInfo.architecture}' 2>/dev/null || echo "$(uname -m)")
case "$NODE_ARCH" in
  amd64|x86_64) GOARCH=amd64; DOCKER_PLATFORM=linux/amd64 ;;
  arm64|aarch64) GOARCH=arm64; DOCKER_PLATFORM=linux/arm64 ;;
  *) echo "unsupported node arch: $NODE_ARCH" >&2; exit 1 ;;
esac

# Cross-compile binaries for the target node arch.
mkdir -p bin
CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go build -o bin/devpod-controller ./cmd/controller
CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go build -o bin/devpod-gateway   ./cmd/gateway
# devpod-supervisor binary is built inside the supervisor Dockerfile.

docker build --platform="$DOCKER_PLATFORM" -t devpod-controller:e2e -f - . <<EOF
FROM gcr.io/distroless/static
COPY bin/devpod-controller /usr/local/bin/devpod-controller
ENTRYPOINT ["/usr/local/bin/devpod-controller"]
EOF
docker build --platform="$DOCKER_PLATFORM" -t devpod-gateway:e2e -f - . <<EOF
FROM gcr.io/distroless/static
COPY bin/devpod-gateway /usr/local/bin/devpod-gateway
ENTRYPOINT ["/usr/local/bin/devpod-gateway"]
EOF
# v2: supervisor image carries static OpenSSH + supervisor binary; used
# as the per-DevPod initContainer that populates the shared emptyDir.
docker buildx build --platform="$DOCKER_PLATFORM" \
    -t devpod-supervisor:e2e -f images/supervisor/Dockerfile --load .

if [ "$USE_KIND" = "1" ]; then
  kind load docker-image --name "${CLUSTER}" \
    devpod-controller:e2e devpod-gateway:e2e devpod-supervisor:e2e
fi

# Apply CRDs server-side. CRDs live under deploy/chart/crds/ (top-level
# Helm 3 directory), which Helm installs only on first install and
# never updates on upgrade. We apply them explicitly so existing
# clusters pick up schema changes — server-side apply because the
# DevPod CRD exceeds the 256 KiB last-applied-config annotation limit.
kubectl apply --server-side --force-conflicts -f deploy/chart/crds/
for crd in users.devpod.io devpods.devpod.io gatewayconfigs.devpod.io; do
  kubectl wait --for=condition=established crd/"$crd" --timeout=30s 2>/dev/null || true
done

helm upgrade --install devpod deploy/chart \
  --create-namespace \
  --set image.controller.repository=devpod-controller --set image.controller.tag=e2e \
  --set image.controller.pullPolicy=IfNotPresent \
  --set image.gateway.repository=devpod-gateway     --set image.gateway.tag=e2e \
  --set image.gateway.pullPolicy=IfNotPresent \
  --set image.supervisor.repository=devpod-supervisor --set image.supervisor.tag=e2e

# Provision per-deployment SSH keys if absent.
if ! kubectl -n devpod-system get secret devpod-gateway-host-key >/dev/null 2>&1; then
  TMP=$(mktemp -d)
  ssh-keygen -t ed25519 -N '' -f "$TMP/host" -q
  kubectl -n devpod-system create secret generic devpod-gateway-host-key \
    --from-file=ssh_host_ed25519_key="$TMP/host" \
    --from-file=ssh_host_ed25519_key.pub="$TMP/host.pub"
  /bin/rm -rf "$TMP"
fi
if ! kubectl -n devpod-system get secret devpod-gateway-internal-key >/dev/null 2>&1; then
  TMP=$(mktemp -d)
  ssh-keygen -t ed25519 -N '' -f "$TMP/internal" -q
  kubectl -n devpod-system create secret generic devpod-gateway-internal-key \
    --from-file=ssh_host_ed25519_key="$TMP/internal" \
    --from-file=ssh_host_ed25519_key.pub="$TMP/internal.pub"
  /bin/rm -rf "$TMP"
fi

# Restart controller + gateway so they pick up the new images / mounts.
kubectl -n devpod-system rollout restart deploy/devpod-controller deploy/devpod-gateway
kubectl -n devpod-system rollout status  deploy/devpod-controller --timeout=180s
kubectl -n devpod-system rollout status  deploy/devpod-gateway    --timeout=180s
