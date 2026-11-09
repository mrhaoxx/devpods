#!/usr/bin/env bash
# Build and test in a builder pod on the cluster instead of locally.
#
#   KUBE_CONTEXT=zjusct bash hack/remote.sh go test ./internal/telemetry/...
#   KUBE_CONTEXT=zjusct bash hack/remote.sh bash hack/test.sh        # unit suite (envtest)
#   KUBE_CONTEXT=zjusct bash hack/remote.sh it -run Telemetry        # integration suite
#   KUBE_CONTEXT=zjusct bash hack/remote.sh fetch /out/devpod-webui DEST   # copy a file out
#   KUBE_CONTEXT=zjusct bash hack/remote.sh down                     # delete the builder pod
#
# The pod (devpod-builder in devpod-audit-it, golang on amd64) keeps its
# module and build caches between runs; every run first syncs the working
# tree (tracked + untracked, not ignored) into /src.
set -euo pipefail

: "${KUBE_CONTEXT:?set KUBE_CONTEXT}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
NS=devpod-audit-it
POD=devpod-builder
IMAGE=${REMOTE_GO_IMAGE:-golang:1.26}
K=(kubectl --context "$KUBE_CONTEXT" -n "$NS")

case "${1:-}" in
down) "${K[@]}" delete pod "$POD" --ignore-not-found --wait=false; exit 0 ;;
fetch) "${K[@]}" cp "$POD:$2" "$3" >/dev/null; exit 0 ;;
esac

if ! "${K[@]}" get pod "$POD" >/dev/null 2>&1; then
  "${K[@]}" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: {name: $POD, namespace: $NS, labels: {app: devpod-builder}}
spec:
  nodeSelector: {kubernetes.io/arch: amd64}
  containers:
  - name: go
    image: $IMAGE
    command: [sleep, infinity]
    env:
    - {name: GOPROXY, value: "https://goproxy.cn,https://proxy.golang.org,direct"}
    - {name: GOFLAGS, value: "-buildvcs=false"}
    resources:
      requests: {cpu: "2", memory: 4Gi}
      limits: {cpu: "8", memory: 8Gi}
    volumeMounts:
    - {name: gocache, mountPath: /root/.cache}
    - {name: gomod, mountPath: /go/pkg}
  volumes:
  - {name: gocache, emptyDir: {sizeLimit: 10Gi}}
  - {name: gomod, emptyDir: {sizeLimit: 10Gi}}
EOF
  "${K[@]}" wait --for=condition=Ready pod/"$POD" --timeout=10m >/dev/null
fi

# sync the working tree
(cd "$ROOT" && git ls-files -co --exclude-standard -z | COPYFILE_DISABLE=1 tar --null -T - -czf -) |
  "${K[@]}" exec -i "$POD" -- sh -c 'rm -rf /src && mkdir -p /src /out && tar -xzf - -C /src 2>/dev/null'

# envtest binaries (controller and webui tests) live in the cache volume
ENVTEST='export BIN_DIR=/root/.cache/bin KUBEBUILDER_ASSETS="$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.20 use 1.31.0 --bin-dir /root/.cache/bin -p path)"'

if [ "${1:-}" = "it" ]; then
  shift
  exec "${K[@]}" exec "$POD" -- sh -c "cd /src && $ENVTEST &&
    AUDIT_IT_CLICKHOUSE_URL=clickhouse://clickhouse.$NS.svc:9000 AUDIT_IT_CLICKHOUSE_USER=it AUDIT_IT_CLICKHOUSE_PASSWORD=it-password \
    AUDIT_IT_S3_ENDPOINT=http://silo.$NS.svc:9000 AUDIT_IT_S3_ACCESS_KEY=devpod AUDIT_IT_S3_SECRET_KEY=devpod-secret \
    go test -tags integration ./internal/audit/... -count=1 $(printf '%q ' "$@")"
fi
exec "${K[@]}" exec "$POD" -- sh -c "cd /src && $ENVTEST && $(printf '%q ' "$@")"
