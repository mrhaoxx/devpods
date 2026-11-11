import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { PodTab } from "./PodTab";
import { renderWithProviders, stubFetch } from "../../../test/render";
import type { PodDetail } from "../../../data/api";

const pod: PodDetail = {
  name: "alice-x",
  phase: "Running",
  ready: "1/2",
  restarts: 2,
  qos: "BestEffort",
  node: "mulan-rack01-node01",
  hostIP: "10.0.0.1",
  podIP: "10.42.0.108",
  createdAt: "2026-11-11T18:52:27Z",
  startedAt: "2026-11-11T18:52:27Z",
  restartPolicy: "Always",
  serviceAccount: "default",
  gracePeriodSeconds: 30,
  hostNetwork: true,
  conditions: [
    { type: "PodScheduled", status: "True", since: "2026-11-11T18:52:27Z" },
    { type: "Ready", status: "False", reason: "ContainersNotReady", message: "containers with unready status: [side]" },
  ],
  initContainers: [{ name: "devpod-bootstrap", image: "reg/supervisor:v1", state: "terminated: exit 0 (Completed)", tone: "ok", ready: false, restarts: 0 }],
  containers: [
    {
      name: "dev",
      image: "ubuntu:24.04",
      state: "running since 2026-11-11T18:52:29Z",
      tone: "ok",
      lastState: "terminated: exit 137 (OOMKilled)",
      ready: true,
      restarts: 2,
      limits: { "nvidia.com/gpu": "8" },
      requests: { "nvidia.com/gpu": "8" },
      mounts: ["/models ← model (ro)"],
    },
    { name: "side", image: "busybox", state: 'waiting: ImagePullBackOff — Back-off pulling image "busybox"', tone: "fail", ready: false, restarts: 0 },
  ],
  volumes: [
    { name: "model", kind: "hostPath", detail: "/data/models" },
    { name: "dshm", kind: "emptyDir", detail: "medium Memory, limit 1300Gi" },
  ],
  labels: { "devpod.io/owner": "alice" },
  nodeSelector: { "kubernetes.io/hostname": "mulan-rack01-node01" },
  tolerations: ["nvidia.com/gpu:NoSchedule"],
};

describe("PodTab", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("tells the Pod as kubectl describe would: summary, conditions, containers with their state, volumes", async () => {
    stubFetch({ "/api/devpods/alice-x/pod": { pod } });
    renderWithProviders(<PodTab name="alice-x" />);
    const summary = await screen.findByRole("group", { name: "概要" });
    for (const [k, v] of [
      ["阶段", "Running"],
      ["就绪", "1/2"],
      ["重启", "2"],
      ["QoS", "BestEffort"],
      ["节点", "mulan-rack01-node01"],
      ["主机 IP", "10.0.0.1"],
      ["Pod IP", "10.42.0.108"],
      ["重启策略", "Always"],
      ["服务账号", "default"],
      ["终止宽限", "30 秒"],
      ["网络", "主机网络"],
    ]) {
      const dt = within(summary).getByText(k);
      expect(dt.parentElement?.textContent).toContain(v);
    }
    const conds = screen.getByRole("list", { name: "条件" });
    expect(within(conds).getByText("Ready").closest("li")?.textContent).toMatch(/False.*ContainersNotReady.*containers with unready status/);
    expect(within(conds).getByText("False").className).toMatch(/text-fail/);

    const containers = screen.getByRole("list", { name: "容器" });
    const dev = within(containers).getByText("dev").closest("li")!;
    expect(dev.textContent).toContain("ubuntu:24.04");
    expect(dev.textContent).toContain("running since 2026-11-11T18:52:29Z");
    expect(dev.textContent).toContain("上次：terminated: exit 137 (OOMKilled)");
    expect(dev.textContent).toContain("limits nvidia.com/gpu=8");
    expect(dev.textContent).toContain("/models ← model (ro)");
    const side = within(containers).getByText("side").closest("li")!;
    expect(within(side).getByText(/ImagePullBackOff/).className).toMatch(/text-fail/);
    expect(within(screen.getByRole("list", { name: "初始化容器" })).getByText(/exit 0/).className).toMatch(/text-run/);

    const vols = screen.getByRole("list", { name: "卷" });
    expect(within(vols).getByText("dshm").closest("li")?.textContent).toContain("emptyDir: medium Memory, limit 1300Gi");
    expect(screen.getByText("devpod.io/owner=alice")).toBeTruthy();
    expect(screen.getByText("kubernetes.io/hostname=mulan-rack01-node01")).toBeTruthy();
    expect(screen.getByText("nvidia.com/gpu:NoSchedule")).toBeTruthy();
  });

  it("a stopped DevPod has no Pod, and says so; 刷新 asks again", async () => {
    let n = 0;
    stubFetch({ "/api/devpods/alice-x/pod": () => new Response(JSON.stringify({ pod: n++ === 0 ? null : pod })) });
    renderWithProviders(<PodTab name="alice-x" />);
    expect(await screen.findByText(/没有 Pod/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "刷新" }));
    await waitFor(() => expect(screen.getByRole("group", { name: "概要" })).toBeTruthy());
  });

  it("a failure to load is shown with its reason", async () => {
    stubFetch({ "/api/devpods/alice-x/pod": new Response(JSON.stringify({ code: "INTERNAL", message: "the API server is away" }), { status: 500 }) });
    renderWithProviders(<PodTab name="alice-x" />);
    expect((await screen.findByRole("alert")).textContent).toContain("the API server is away");
  });
});
