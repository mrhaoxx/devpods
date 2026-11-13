# DevPod

Kubernetes-native multi-tenant remote development environments.

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

## Several clusters

One console can manage DevPods in several clusters, and one SSH gateway
can serve them all. Nothing dials into a cluster: each cluster runs an
**agent** that dials out.

    console  ◀── agents (Kubernetes requests, what the cluster is)
       ▲  ◀── gateway (signed questions: which DevPod is this login? whose key is this?)
       │
    gateway  ◀── agents (connections to DevPods: sshd, port links)

- **The console's release** — `webui.enabled`, `webui.clusters.enabled`.
  Under 集群, an admin registers each cluster and each gateway; either
  is shown a join token once, with the `helm` command to run.
- **A managed cluster's release** — `webui.enabled=false`,
  `agent.enabled`, `agent.cluster`, `agent.console`, `agent.joinToken`:
  the controller and the agent. The agent's service account is what the
  console can do in that cluster (DevPods, their Pods and events, nodes,
  reservations; no Secrets), and a gateway gets connections to running
  DevPods only. With `telemetry.enabled` the DevPods' telemetry goes to
  the console over the agent's link (copy the console's Secret
  `devpod-ingest-key` into this cluster).
- **A gateway of several clusters** — `gateway.hub.enabled`,
  `gateway.hub.name`, `gateway.hub.console`, `gateway.hub.joinToken`, and
  a route (TLS, long-lived connections) to the Service
  `devpod-gateway-agents` for the agents. Users log in with
  `<user>+<devpod>[+<cluster>]`.

Users, access tokens and saved configs are the console's; quotas are per
cluster. While a cluster's agent is away the console shows its last
known state and refuses changes to it; its DevPods keep running. A
release without any of these values is what it was: one cluster, with
its own console and gateway. See `docs/api.md` for the API.

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
