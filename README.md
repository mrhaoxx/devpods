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

    # Unit + envtest tests (envtest's API server and etcd for the controllers)
    export KUBEBUILDER_ASSETS="$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.20 use 1.31.0 --bin-dir bin -p path)"
    go test ./... -count=1 -race

    # Lint
    go run github.com/golangci/golangci-lint/cmd/golangci-lint@v1.62.0 run

    # Web UI bundle, typecheck and unit/component tests (Node >= 20)
    (cd web && npm ci && npm run build && npx tsc -b && npx vitest run)

    # Chart: render it in its configurations and check the wiring (helm)
    bash deploy/test-chart.sh

    # Audit store integration: against throwaway ClickHouse and S3
    AUDIT_IT_CLICKHOUSE_URL=… AUDIT_IT_S3_ENDPOINT=… go test ./internal/audit/store/ -run Integration

    # Images: images/<name>/Dockerfile, built from the repository root, e.g.
    docker buildx build -f images/gateway/Dockerfile -t REGISTRY/devpod-gateway:TAG .

CI (.github/workflows) runs the generate check, vet, lint, the tests, the
web checks and the chart checks on every push; pushes to main and v* tags
publish multi-arch images to ghcr.io/mrhaoxx/devpod-* (`dev` for main, the
tag's name for a release).
