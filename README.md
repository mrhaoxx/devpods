# DevPod

Kubernetes-native multi-tenant remote development environments.

See `docs/superpowers/specs/2026-05-12-devpod-design.md` for the design.

## Web console

The optional web UI (`webui.enabled`) is a console: a permanent sidebar,
dense full-width tables and drawers instead of page hops, light and dark
themes, and ⌘K / Ctrl+K search over users, DevPods, sessions and
commands.

- **我的 DevPod** — every DevPod with per-second CPU, memory, GPU,
  sessions, network and RDMA (with `telemetry.enabled`); the detail page
  adds six live cards, history charts (1 s to 7 days), processes,
  sessions, container facts and events.
- **Admins** — cluster overview (KPIs, node heat strip, "needs
  attention"), all DevPods with bulk hibernate / start / delete, nodes
  with GPU occupants and the Kore CPU topology, users and quotas with
  batch actions and CSV import.
- **Audit** (`audit.enabled`) — sessions with replay beside a timeline,
  command and file search, alerts, access log, policy.

Live metrics need the telemetry agent, which ships in the supervisor: a
DevPod starts reporting when its Pod is next recreated.

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

    # Web UI typecheck + unit/component tests, and the Playwright e2e
    # against a mock API — both in a pod on a cluster, nothing local
    KUBE_CONTEXT=<ctx> bash hack/remote-web.sh sh -c 'npx tsc -b && npx vitest run'
    KUBE_CONTEXT=<ctx> bash hack/e2e-ui.sh

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
