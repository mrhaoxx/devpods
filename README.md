# DevPod

Kubernetes-native multi-tenant remote development environments.

See `docs/superpowers/specs/2026-05-12-devpod-design.md` for the design.

For Docker and RKE2/containerd snapshot configuration, see
[the snapshot tool image guide](images/snapshot/README.md).

## Commands

    # Build all binaries
    go build ./...

    # Generate deepcopy code + CRD manifests
    go generate ./...

    # Unit + envtest tests
    bash hack/test.sh

    # Lint
    go run github.com/golangci/golangci-lint/cmd/golangci-lint@v1.62.0 run

    # Build the web UI bundle (requires Node >= 20)
    bash hack/build-webui.sh

    # End-to-end (kind required)
    bash hack/e2e-up.sh
    go test -tags e2e ./test/e2e/... -count=1

    # Web UI end-to-end (kind required, after e2e-up.sh)
    bash hack/e2e-webui.sh

    # Audit: chart wiring, integration (needs a cluster for throwaway
    # ClickHouse + S3 pods), and an isolated end-to-end run
    bash hack/test-chart.sh
    KUBE_CONTEXT=<ctx> bash hack/test-integration.sh
    KUBE_CONTEXT=<ctx> bash hack/e2e-audit.sh
