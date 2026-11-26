import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { Route, Routes, useLocation } from "react-router-dom";
import { DetailPage } from "./DetailPage";
import { renderWithProviders, stubFetch, stubMe } from "../../../test/render";
import { LiveHub } from "../../../data/live/LiveHub";
import type { SseHandlers } from "../../../data/sse";
import type { DevPod } from "../../../data/api";

const dp: DevPod = {
  metadata: { name: "alice-x", uid: "u1" },
  spec: { owner: "alice", running: true, pod: { spec: { containers: [{ name: "dev", image: "ubuntu:24.04", resources: { limits: { cpu: "8", memory: "16Gi" } } }] } } },
  status: { phase: "Running" },
};

const rankDp: DevPod = { ...dp, metadata: { name: "alice-x", uid: "u2" }, spec: { ...dp.spec, job: { name: "abc12", rank: 1 } } };

function Where() {
  const l = useLocation();
  return <div data-testid="where">{l.search}</div>;
}

let calls: ReturnType<typeof stubFetch> = [];

function setup(opts: { route?: string; admin?: boolean; live?: boolean; user?: string; rank?: boolean } = {}) {
  const cs = stubFetch({
    "PATCH /api/devpods/alice-x": { devpod: opts.rank ? rankDp : dp, rebuild: false },
    "/api/devpods/alice-x": { devpod: opts.rank ? rankDp : dp, pod: { node: "g206", ip: "10.1.2.3", startedAt: "2026-11-10T10:00:00Z" } },
    "/api/audit/sessions": { items: [], next: "" },
    "/api/devpods/alice-x/history": {
      items: [{ ts: "2026-11-27T09:00:00Z", kind: "devpod", object: "alice-x", owner: "alice", actor: "alice", via: "web", action: "create", detail: "image ubuntu" }],
      next: "",
    },
    "/api/nodes": { items: [{ name: "g206", ready: true, cpu: { allocatable: 64, allocated: 0 }, mem: { allocatable: 512 * 1024 ** 3, allocated: 0 }, gpuTotal: 0, gpuUsed: 0, reservable: false, pending: 0, access: "ok", mine: null, selectable: true }] },
    "POST /api/devpods/alice-x/redeploy": {},
    "/api/devpods/alice-x/metrics": (u: URL) => ({ step: u.searchParams.get("range") === "15m" ? 1 : 5, ts: [Math.floor(Date.now() / 1000) - 60], series: { cpu: [2.5] } }),
  });
  calls = cs;
  const conns: { url: string; h: SseHandlers }[] = [];
  const hub = new LiveHub({
    open: (url, h) => {
      conns.push({ url, h });
      return () => {};
    },
  });
  const me = stubMe({ admin: !!opts.admin, user: opts.user ?? (opts.admin ? "root" : "alice") });
  me.features.telemetry = opts.live !== false;
  renderWithProviders(
    <Routes>
      <Route
        path="/devpods/:cluster/:name"
        element={
          <>
            <DetailPage />
            <Where />
          </>
        }
      />
    </Routes>,
    { route: opts.route ?? "/devpods/-/alice-x", me, hub, liveEnabled: opts.live !== false },
  );
  return conns;
}

const sample = {
  ts: "2026-11-10T12:00:00Z",
  container: { cpu: 3.5, cpu_limit: 8, mem_ws: 4 * 1024 ** 3, mem_limit: 16 * 1024 ** 3, net_rx: 1024, net_tx: 2048, io_r: 0, io_w: 0, net_kind: "pod" },
  gpus: [],
  rdma: [],
};

describe("DevPod detail", () => {
  it("a queue job's rank says whose it is, and stopping it asks first", async () => {
    setup({ rank: true });
    const note = await screen.findByText(/是任务/);
    expect(note.textContent).toMatch(/第 1 台/);
    expect(screen.getByRole("link", { name: "abc12" }).getAttribute("href")).toBe("/queue/-/abc12");
    fireEvent.click(screen.getByRole("button", { name: "休眠" }));
    const dialog = within(await screen.findByRole("dialog"));
    expect(dialog.getByText(/整个任务失败/)).toBeTruthy();
    expect(calls.some((c) => c.method === "PATCH")).toBe(false);
  });

  afterEach(() => vi.unstubAllGlobals());

  it("cards show values from the metrics event", async () => {
    const conns = setup();
    await screen.findByRole("heading", { name: "alice-x" });
    act(() => conns[0].h.onMessage({ kind: "metrics", metrics: sample }));
    expect(screen.getByTestId("card-cpu").textContent).toContain("3.5/8");
    expect(screen.getByTestId("card-mem").textContent).toContain("4.0G/16.0G");
  });

  it("no limit: the cards show the node's CPU and memory as the ceiling", async () => {
    const conns = setup();
    await screen.findByRole("heading", { name: "alice-x" });
    act(() => conns[0].h.onMessage({ kind: "metrics", metrics: { ...sample, container: { ...sample.container, cpu_limit: 0, mem_limit: 0 } } }));
    await waitFor(() => expect(screen.getByTestId("card-mem").textContent).toContain("4.0G/512.0G"));
    expect(screen.getByTestId("card-cpu").textContent).toContain("3.5/64");
  });

  it("cards show — without telemetry", async () => {
    setup({ live: false });
    await screen.findByRole("heading", { name: "alice-x" });
    for (const id of ["cpu", "mem", "gpu", "net", "rdma", "io"]) {
      expect(screen.getByTestId(`card-${id}`).textContent).toContain("—");
    }
  });

  it("tab follows ?tab= and switching tab updates the URL", async () => {
    setup({ route: "/devpods/-/alice-x?tab=events" });
    await screen.findByRole("heading", { name: "alice-x" });
    expect(screen.getByRole("tab", { name: "事件" }).getAttribute("aria-selected")).toBe("true");
    fireEvent.click(screen.getByRole("tab", { name: "会话" }));
    expect(screen.getByTestId("where").textContent).toBe("?tab=sessions");
  });

  it("记录 lists what was done to this DevPod, by whom", async () => {
    setup({ route: "/devpods/-/alice-x?tab=history" });
    expect(await screen.findByText("创建")).toBeTruthy();
    expect(screen.getByText("image ubuntu")).toBeTruthy();
  });

  it("协作者 adds collaborators to this DevPod", async () => {
    setup({ route: "/devpods/-/alice-x" });
    await screen.findByRole("heading", { name: "alice-x" });
    fireEvent.click(screen.getByRole("button", { name: "协作者" }));
    const dialog = within(await screen.findByRole("dialog"));
    fireEvent.change(dialog.getByLabelText("用户名"), { target: { value: "bob, carol" } });
    fireEvent.click(dialog.getByRole("button", { name: "应用" }));
    await waitFor(() => expect(calls.some((c) => c.method === "PATCH")).toBe(true));
    expect(JSON.parse(calls.find((c) => c.method === "PATCH")!.body!)).toEqual({ collaborators: ["bob", "carol"] });
  });

  it("设置镜像 sets this DevPod's image", async () => {
    setup({ route: "/devpods/-/alice-x" });
    await screen.findByRole("heading", { name: "alice-x" });
    fireEvent.click(screen.getByRole("button", { name: "设置镜像" }));
    const dialog = within(await screen.findByRole("dialog"));
    fireEvent.change(dialog.getByLabelText("镜像"), { target: { value: "ubuntu:26.04" } });
    fireEvent.click(dialog.getByRole("button", { name: "应用" }));
    await waitFor(() => expect(calls.some((c) => c.method === "PATCH")).toBe(true));
    expect(JSON.parse(calls.find((c) => c.method === "PATCH")!.body!)).toEqual({ image: "ubuntu:26.04" });
  });

  it("迁移 can hand this DevPod back to the scheduler", async () => {
    setup({ route: "/devpods/-/alice-x" });
    await screen.findByRole("heading", { name: "alice-x" });
    fireEvent.click(screen.getByRole("button", { name: "迁移" }));
    const dialog = within(await screen.findByRole("dialog"));
    fireEvent.click(dialog.getByRole("checkbox", { name: /取消固定节点/ }));
    fireEvent.click(dialog.getByRole("button", { name: "应用" }));
    await waitFor(() => expect(calls.some((c) => c.method === "PATCH")).toBe(true));
    expect(JSON.parse(calls.find((c) => c.method === "PATCH")!.body!)).toEqual({ node: "" });
  });

  it("重新部署 redeploys this DevPod once confirmed", async () => {
    setup({ route: "/devpods/-/alice-x" });
    await screen.findByRole("heading", { name: "alice-x" });
    fireEvent.click(screen.getByRole("button", { name: "重新部署" }));
    const dialog = within(await screen.findByRole("dialog"));
    fireEvent.click(dialog.getByRole("button", { name: "确认重新部署" }));
    await waitFor(() => expect(calls.some((c) => c.method === "POST" && c.url.pathname.endsWith("/api/devpods/alice-x/redeploy"))).toBe(true));
  });

  it("a DevPod shared with me has no 迁移, 设置镜像 or 重新部署", async () => {
    setup({ route: "/devpods/-/alice-x", user: "bob" });
    await screen.findByRole("heading", { name: "alice-x" });
    for (const name of ["迁移", "设置镜像", "重新部署"]) expect(screen.queryByRole("button", { name })).toBeNull();
  });

  it("编辑 opens the YAML tab", async () => {
    setup({ route: "/devpods/-/alice-x" });
    await screen.findByRole("heading", { name: "alice-x" });
    fireEvent.click(screen.getByRole("button", { name: "编辑" }));
    expect(screen.getByTestId("where").textContent).toBe("?tab=yaml");
  });

  it("processes tab reconnects with procs=all", async () => {
    const conns = setup();
    await screen.findByRole("heading", { name: "alice-x" });
    expect(conns.at(-1)!.url).toBe("/api/devpods/alice-x/stream");
    fireEvent.click(screen.getByRole("tab", { name: "进程" }));
    expect(conns.at(-1)!.url).toBe("/api/devpods/alice-x/stream?procs=all");
  });

  it("shows the deleted state when the stream reports DELETED", async () => {
    const conns = setup();
    await screen.findByRole("heading", { name: "alice-x" });
    act(() => conns[0].h.onMessage({ kind: "devpod", type: "DELETED", devpod: dp }));
    expect(screen.getByText("该 DevPod 已删除")).toBeTruthy();
    expect(screen.getByRole("link", { name: /返回列表/ }).getAttribute("href")).toBe("/");
  });

  it("admin sees Owner in the meta row, owner does not", async () => {
    setup({ admin: true });
    await screen.findByRole("heading", { name: "alice-x" });
    expect(screen.getByText("Owner")).toBeTruthy();
  });
  it("the owner sees no Owner field", async () => {
    setup();
    await screen.findByRole("heading", { name: "alice-x" });
    expect(screen.queryByText("Owner")).toBeNull();
  });

  it("opens on 监控 with the last hour from history", async () => {
    setup();
    await screen.findByRole("heading", { name: "alice-x" });
    expect(screen.getByRole("tab", { name: "监控" }).getAttribute("aria-selected")).toBe("true");
    expect(screen.getByRole("radio", { name: "1h" }).getAttribute("aria-checked")).toBe("true");
    await waitFor(() => expect(calls.some((c) => c.url.pathname === "/api/devpods/alice-x/metrics" && c.url.searchParams.get("range") === "1h")).toBe(true));
  });

  it("the range picked last is the one it opens on next time", async () => {
    localStorage.setItem("devpod.metricsRange", "6h");
    setup();
    await screen.findByRole("heading", { name: "alice-x" });
    expect(screen.getByRole("radio", { name: "6h" }).getAttribute("aria-checked")).toBe("true");
    await waitFor(() => expect(calls.some((c) => c.url.pathname === "/api/devpods/alice-x/metrics" && c.url.searchParams.get("range") === "6h")).toBe(true));
    fireEvent.click(screen.getByRole("radio", { name: "24h" }));
    expect(localStorage.getItem("devpod.metricsRange")).toBe("24h");
    fireEvent.click(screen.getByRole("radio", { name: "1h" }));
    expect(screen.getByRole("radio", { name: "1h" }).getAttribute("aria-checked")).toBe("true");
    localStorage.clear();
  });

  it("card sparklines come from history before any live sample", async () => {
    setup();
    await screen.findByRole("heading", { name: "alice-x" });
    await waitFor(() => expect(screen.getByTestId("card-cpu").querySelector("polyline")).toBeTruthy());
  });

  it("存为配置 leaves for the config editor", async () => {
    setup();
    fireEvent.click(await screen.findByRole("button", { name: "存为配置" }));
    await waitFor(() => expect(screen.queryByTestId("where")).toBeNull()); // the detail route is left
  });

  it("the address names the DevPod's cluster; its data comes from there", async () => {
    setup({ route: "/devpods/a/alice-x", live: false });
    await waitFor(() => expect(calls.some((c) => c.url.pathname === "/api/clusters/a/devpods/alice-x")).toBe(true));
    expect(calls.some((c) => c.url.pathname === "/api/devpods/alice-x")).toBe(false);
  });
});
