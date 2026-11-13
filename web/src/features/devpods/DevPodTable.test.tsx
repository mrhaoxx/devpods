import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { DevPodTable } from "./DevPodTable";
import { collabPatch, migrateTargets } from "./BulkDialog";
import { renderWithProviders, stubFetch, stubMe } from "../../test/render";
import type { DevPod } from "../../data/api";
import { act } from "@testing-library/react";
import { LiveHub } from "../../data/live/LiveHub";
import type { SseHandlers } from "../../data/sse";

const mk = (name: string, owner: string, phase: string, running = true): DevPod => ({
  metadata: { name, uid: name },
  spec: { owner, running, pod: { spec: { containers: [{ name: "dev", image: "ubuntu:24.04", resources: { limits: { cpu: "4", memory: "8Gi" } } }] } } },
  status: { phase },
});

const list = {
  items: [mk("alice-a", "alice", "Running"), mk("alice-b", "alice", "Stopped", false), mk("bob-c", "bob", "Running"), mk("bob-d", "bob", "Failed")],
  pods: { "alice-a": { node: "g206", ip: "10.0.0.1", startedAt: "2026-11-10T10:00:00Z" } },
};

const rowNames = () =>
  screen
    .getAllByRole("row")
    .slice(1)
    .map((r) => within(r).queryByRole("link")?.textContent)
    .filter(Boolean);

describe("DevPodTable", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("shows — in every metric cell without live data", async () => {
    stubFetch({ "/api/devpods": { items: [list.items[0]], pods: list.pods } });
    renderWithProviders(<DevPodTable scope="mine" />);
    await screen.findByRole("link", { name: "alice-a" });
    // CPU, 内存, GPU, 网络, RDMA all fall back to — with the hint
    expect(screen.getAllByLabelText("监控在 Pod 下次重建后生效").length).toBeGreaterThanOrEqual(4);
  });

  it("filters by the q param user:alice phase:running", async () => {
    stubFetch({ "/api/devpods": list });
    renderWithProviders(<DevPodTable scope="all" />, { me: stubMe({ admin: true }), route: "/admin/devpods?q=user:alice%20phase:running" });
    await screen.findByRole("link", { name: "alice-a" });
    expect(rowNames()).toEqual(["alice-a"]);
  });

  it("the filter box takes each key at once; the rows follow when typing pauses", async () => {
    stubFetch({ "/api/devpods": list });
    renderWithProviders(<DevPodTable scope="all" />, { me: stubMe({ admin: true }), route: "/admin/devpods" });
    await screen.findByRole("link", { name: "bob-c" });
    const box = screen.getByRole<HTMLInputElement>("textbox", { name: "过滤" });
    fireEvent.change(box, { target: { value: "user:alice" } });
    expect(box.value).toBe("user:alice");
    expect(rowNames()).toContain("bob-c"); // a thousand rows are not redrawn on every key
    await act(() => new Promise((r) => setTimeout(r, 300))); // the pause, inside act: the URL change is a transition
    expect(rowNames()).toEqual(["alice-a", "alice-b"]);
  });

  it("status chip counts follow the rows", async () => {
    stubFetch({ "/api/devpods": list });
    renderWithProviders(<DevPodTable scope="all" />, { me: stubMe({ admin: true }), route: "/admin/devpods?q=user:bob" });
    await screen.findByRole("link", { name: "bob-c" });
    expect(screen.getByRole("button", { name: /Running\s*1/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: /Failed\s*1/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: /Stopped\s*0/ })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /Failed\s*1/ }));
    await waitFor(() => expect(rowNames()).toEqual(["bob-d"]));
  });

  it("bulk delete stays disabled until 删除 is typed", async () => {
    const calls = stubFetch({ "/api/devpods": list, "DELETE /api/devpods/alice-a": new Response(null, { status: 204 }) });
    renderWithProviders(<DevPodTable scope="all" />, { me: stubMe({ admin: true }), route: "/admin/devpods" });
    await screen.findByRole("link", { name: "alice-a" });
    fireEvent.click(screen.getByLabelText("选择 alice-a"));
    fireEvent.click(screen.getByRole("button", { name: "删除…" }));
    const confirm = screen.getByRole("button", { name: "确认删除" }) as HTMLButtonElement;
    expect(confirm.disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("输入 删除 以确认"), { target: { value: "删除" } });
    expect(confirm.disabled).toBe(false);
    fireEvent.click(confirm);
    await waitFor(() => expect(calls.some((c) => c.method === "DELETE" && c.url.pathname === "/api/devpods/alice-a")).toBe(true));
  });

  it("admin chips hidden for non-admins", async () => {
    stubFetch({ "/api/devpods": { items: [list.items[0]], pods: {} } });
    renderWithProviders(<DevPodTable scope="mine" />);
    await screen.findByRole("link", { name: "alice-a" });
    expect(screen.queryByRole("button", { name: /空转/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /GPU only/ })).toBeNull();
  });

  it("my DevPods can be selected for bulk actions", async () => {
    stubFetch({ "/api/devpods": { items: [list.items[0]], pods: {} } });
    renderWithProviders(<DevPodTable scope="mine" />);
    await screen.findByRole("link", { name: "alice-a" });
    fireEvent.click(screen.getByLabelText("选择 alice-a"));
    const bar = within(screen.getByRole("toolbar", { name: "批量操作" }));
    // 迁移 is open to everyone now: the node rule decides, not the role
    expect(bar.getAllByRole("button").map((b) => b.textContent)).toEqual(["迁移…", "休眠", "启动", "协作者…", "设置镜像…", "重新部署…", "删除…", "取消选择"]);
  });

  it("bulk 设置镜像 patches every selected DevPod; 迁移 sends the node", async () => {
    const ok = { devpod: list.items[0], rebuild: true };
    const calls = stubFetch({
      "/api/devpods": list,
      "/api/nodes": { items: ["g206", "g207", "g208"].map((name) => ({ name, ready: true, selectable: true, held: null, next: null, cpu: { allocatable: 64, allocated: 0 }, mem: { allocatable: 1 << 30, allocated: 0 }, gpuTotal: 0, gpuUsed: 0 })).concat([{ name: "g209", ready: true, selectable: false, cpu: { allocatable: 64, allocated: 0 }, mem: { allocatable: 1 << 30, allocated: 0 }, gpuTotal: 0, gpuUsed: 0, held: { user: "bob", until: "2026-11-12T09:00:00Z" } as never, next: null }]) },
      "PATCH /api/devpods/alice-a": ok,
      "PATCH /api/devpods/bob-c": ok,
    });
    renderWithProviders(<DevPodTable scope="all" />, { me: stubMe({ admin: true }), route: "/admin/devpods" });
    await screen.findByRole("link", { name: "alice-a" });
    fireEvent.click(screen.getByLabelText("选择 alice-a"));
    fireEvent.click(screen.getByLabelText("选择 bob-c"));
    fireEvent.click(screen.getByRole("button", { name: "设置镜像…" }));
    const apply = screen.getByRole("button", { name: "应用" }) as HTMLButtonElement;
    expect(apply.disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("镜像"), { target: { value: "ubuntu:25.04" } });
    fireEvent.click(apply);
    await waitFor(() => expect(calls.filter((c) => c.method === "PATCH").map((c) => `${c.url.pathname} ${c.body}`).sort()).toEqual(['/api/devpods/alice-a {"image":"ubuntu:25.04"}', '/api/devpods/bob-c {"image":"ubuntu:25.04"}']));

    // 迁移: one node takes every DevPod; several nodes pair up in the order picked
    fireEvent.click(await screen.findByLabelText("选择 bob-c"));
    fireEvent.click(screen.getByLabelText("选择 alice-a"));
    fireEvent.click(screen.getByRole("button", { name: "迁移…" }));
    const apply2 = () => screen.getByRole("button", { name: "应用" }) as HTMLButtonElement;
    expect(apply2().disabled).toBe(true);
    // a node someone else holds cannot be picked
    expect(((await screen.findByRole("checkbox", { name: "g209" })) as HTMLInputElement).disabled).toBe(true);
    fireEvent.click(await screen.findByRole("checkbox", { name: "g207" }));
    fireEvent.click(screen.getByRole("checkbox", { name: "g206" }));
    fireEvent.click(screen.getByRole("checkbox", { name: "g208" }));
    expect(screen.getByRole("alert").textContent).toMatch(/3 个节点、2 个 DevPod/);
    expect(apply2().disabled).toBe(true);
    fireEvent.click(screen.getByRole("checkbox", { name: "g208" }));
    expect(screen.getByRole("list", { name: "迁移计划" }).textContent).toBe("bob-c → g207alice-a → g206");
    fireEvent.click(apply2());
    await waitFor(() =>
      expect(calls.filter((c) => c.method === "PATCH" && c.body?.includes("node")).map((c) => `${c.url.pathname} ${c.body}`).sort()).toEqual(['/api/devpods/alice-a {"node":"g206"}', '/api/devpods/bob-c {"node":"g207"}']),
    );
  });

  it("migrateTargets: one node for all, or one per DevPod in order", () => {
    expect(migrateTargets(["a", "b"], ["n1"])).toEqual([["a", "n1"], ["b", "n1"]]);
    expect(migrateTargets(["a", "b"], ["n2", "n1"])).toEqual([["a", "n2"], ["b", "n1"]]);
    expect(migrateTargets(["a", "b"], ["n1", "n2", "n3"])).toBeNull();
    expect(migrateTargets(["a", "b"], [])).toBeNull();
  });

  it("bulk 重新部署 asks first, then posts redeploy", async () => {
    const calls = stubFetch({ "/api/devpods": list, "POST /api/devpods/alice-a/redeploy": new Response(null, { status: 204 }) });
    renderWithProviders(<DevPodTable scope="mine" />);
    await screen.findByRole("link", { name: "alice-a" });
    fireEvent.click(screen.getByLabelText("选择 alice-a"));
    fireEvent.click(screen.getByRole("button", { name: "重新部署…" }));
    expect(calls.some((c) => c.method === "POST")).toBe(false);
    fireEvent.click(screen.getByRole("button", { name: "确认重新部署" }));
    await waitFor(() => expect(calls.some((c) => c.method === "POST" && c.url.pathname === "/api/devpods/alice-a/redeploy")).toBe(true));
  });

  it("collabPatch adds, removes and replaces", () => {
    expect(collabPatch("add", ["b", "c"], ["a", "b"])).toEqual(["a", "b", "c"]);
    expect(collabPatch("remove", ["b"], ["a", "b"])).toEqual(["a"]);
    expect(collabPatch("set", [], ["a"])).toEqual([]);
  });

  it("draws the CPU sparkline from history on load", async () => {
    const t = Math.floor(Date.now() / 1000);
    stubFetch({
      "/api/devpods": { items: [list.items[0]], pods: list.pods },
      "/api/live/devpods/recent": { step: 15, ts: [t - 60, t - 45, t - 30], items: { "alice-a": { cpu: [1, 2, 3], mem: [1, 1, 1] } } },
    });
    const me = stubMe();
    me.features.telemetry = true;
    renderWithProviders(<DevPodTable scope="mine" />, { me, liveEnabled: true });
    const row = (await screen.findByRole("link", { name: "alice-a" })).closest("tr")!;
    await waitFor(() => expect(row.querySelector("polyline")).toBeTruthy());
  });

  it("DevPods launched together are one row; stopping it stops each of them", async () => {
    const member = (i: number): DevPod => ({
      ...mk(`alice-t-${i}`, "alice", "Running"),
      metadata: { name: `alice-t-${i}`, uid: `t${i}`, labels: { "devpod.io/group": "alice-t" }, annotations: { "devpod.io/group-index": String(i), "devpod.io/group-size": "3" } },
    });
    const calls = stubFetch({
      "/api/devpods": { items: [member(0), member(1), member(2), mk("alice-a", "alice", "Running")], pods: {} },
      "PATCH /api/devpods/alice-t-0": { devpod: member(0) },
      "PATCH /api/devpods/alice-t-1": { devpod: member(1) },
      "PATCH /api/devpods/alice-t-2": { devpod: member(2) },
    });
    renderWithProviders(<DevPodTable scope="mine" />);
    const row = (await screen.findByText(/alice-t ×3/)).closest("tr")!;
    expect(row.textContent).toContain("3/3 运行中");
    expect(screen.queryByRole("link", { name: "alice-t-0" })).toBeNull(); // folded
    fireEvent.keyDown(within(row).getByRole("button", { name: "alice-t 操作" }), { key: "Enter" });
    fireEvent.click(await screen.findByRole("menuitem", { name: "整组休眠" }));
    await waitFor(() => expect(calls.filter((c) => c.method === "PATCH").map((c) => c.url)).toHaveLength(3));
    expect(calls.filter((c) => c.method === "PATCH").every((c) => c.body === '{"running":false}')).toBe(true);
    // unfolded, its members are rows of their own
    fireEvent.click(within(row).getByRole("button", { name: "展开 alice-t" }));
    expect(await screen.findByRole("link", { name: "alice-t-0" })).toBeTruthy();
  });

  it("a right click offers a DevPod's own actions, or the selection's on a row of it", async () => {
    stubFetch({ "/api/devpods": list });
    renderWithProviders(<DevPodTable scope="all" />, { me: stubMe({ admin: true }), route: "/admin/devpods" });
    const rowOf = async (name: string) => (await screen.findByRole("link", { name })).closest("tr")!;
    fireEvent.contextMenu(await rowOf("alice-a"));
    let menu = within(await screen.findByRole("menu"));
    expect(menu.getByRole("menuitem", { name: "复制 SSH 命令" })).toBeTruthy();
    expect(menu.getByRole("menuitem", { name: "删除…" })).toBeTruthy();
    fireEvent.keyDown(screen.getByRole("menu"), { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("menu")).toBeNull());
    fireEvent.click(screen.getByLabelText("选择 alice-a"));
    fireEvent.click(screen.getByLabelText("选择 bob-c"));
    fireEvent.contextMenu(await rowOf("bob-c"));
    menu = within(await screen.findByRole("menu"));
    expect(menu.getByText("已选 2")).toBeTruthy();
    expect(menu.getByRole("menuitem", { name: "迁移…" })).toBeTruthy();
  });

  it("sorts by what DevPods use now: CPU most first, those not running last either way", async () => {
    stubFetch({ "/api/devpods": list });
    const conns: { url: string; h: SseHandlers }[] = [];
    const hub = new LiveHub({ open: (url, h) => (conns.push({ url, h }), () => {}) });
    renderWithProviders(<DevPodTable scope="all" />, { me: stubMe({ admin: true }), hub, liveEnabled: true, route: "/admin/devpods?sort=-cpu" });
    await screen.findByRole("link", { name: "alice-a" });
    const item = (name: string, cpu: number | null) => ({ name, owner: "x", cpu, cpuLimit: 4, mem: null, memLimit: 0, gpu: [], netRx: null, netTx: null, netKind: "", rdmaRx: null, rdmaTx: null, rdmaKind: "", ioR: null, ioW: null, sessions: [], idle: false });
    await waitFor(() => expect(conns.length).toBeGreaterThan(0));
    act(() => conns.forEach((c) => c.h.onMessage({ ts: new Date().toISOString(), items: [item("alice-a", 1), item("bob-c", 3), item("bob-d", 2)] })));
    // bob-d failed and alice-b is stopped: nothing to sort them by (bob-d's numbers are not shown either)
    await waitFor(() => expect(rowNames()).toEqual(["bob-c", "alice-a", "alice-b", "bob-d"]), { timeout: 7000 });
    // the other way: least first, those still last
    fireEvent.click(screen.getByRole("button", { name: /CPU/ }));
    await waitFor(() => expect(rowNames()).toEqual(["alice-a", "bob-c", "alice-b", "bob-d"]));
  }, 15000);

  it("node and uptime of a console of several clusters (pods keyed by cluster/name)", async () => {
    const a = { ...mk("alice-a", "alice", "Running"), cluster: "zjusct" };
    stubFetch({ "/api/devpods": { items: [a], pods: { "zjusct/alice-a": { node: "g206", ip: "10.0.0.1", startedAt: "2026-11-10T10:00:00Z" } } } });
    renderWithProviders(<DevPodTable scope="mine" />);
    const row = (await screen.findByRole("link", { name: "alice-a" })).closest("tr")!;
    expect(row.textContent).toContain("g206");
  });

  it("every cluster's DevPods: same-named ones are two rows, each acted on in its cluster; the filter narrows them", async () => {
    const x = (cluster: string) => ({ ...mk("alice-x", "alice", "Running"), cluster });
    const calls = stubFetch({
      "/api/devpods": { items: [x("a"), x("b")], pods: { "a/alice-x": { node: "na" }, "b/alice-x": { node: "nb" } } },
      "PATCH /api/clusters/b/devpods/alice-x": { devpod: x("b"), rebuild: false },
    });
    const clusters = [
      { name: "a", connected: true },
      { name: "b", connected: false },
    ];
    const first = renderWithProviders(<DevPodTable scope="mine" />, { clusters });
    await waitFor(() => expect(screen.getAllByRole("link", { name: "alice-x" })).toHaveLength(2));
    const hrefs = screen.getAllByRole("link", { name: "alice-x" }).map((l) => l.getAttribute("href"));
    expect(hrefs).toEqual(["/devpods/a/alice-x", "/devpods/b/alice-x"]);
    const rowB = screen.getAllByRole("row").find((r) => within(r).queryByText("nb"))!;
    expect(within(rowB).getByText("离线")).toBeTruthy();
    fireEvent.keyDown(within(rowB).getByRole("button", { name: "alice-x 操作" }), { key: "Enter" });
    fireEvent.click(await screen.findByRole("menuitem", { name: "休眠" }));
    await waitFor(() => expect(calls.some((c) => c.method === "PATCH" && c.url.pathname === "/api/clusters/b/devpods/alice-x")).toBe(true));
    first.unmount();

    renderWithProviders(<DevPodTable scope="mine" />, { clusters, route: "/?cluster=a" });
    await screen.findByText("na");
    expect(screen.getAllByRole("link", { name: "alice-x" })).toHaveLength(1);
  });

  it("migration is for DevPods of one cluster: a selection across clusters cannot be migrated", async () => {
    const x = (cluster: string) => ({ ...mk("alice-x", "alice", "Running"), cluster });
    stubFetch({ "/api/devpods": { items: [x("a"), x("b")], pods: {} } });
    const clusters = [
      { name: "a", connected: true },
      { name: "b", connected: true },
    ];
    renderWithProviders(<DevPodTable scope="mine" />, { clusters });
    await waitFor(() => expect(screen.getAllByRole("link", { name: "alice-x" })).toHaveLength(2));
    fireEvent.click(screen.getByLabelText("选择 a/alice-x"));
    expect((screen.getByRole("button", { name: "迁移…" }) as HTMLButtonElement).disabled).toBe(false);
    fireEvent.click(screen.getByLabelText("选择 b/alice-x"));
    const migrate = screen.getByRole("button", { name: "迁移…" }) as HTMLButtonElement;
    expect(migrate.disabled).toBe(true);
    expect(migrate.title).toMatch(/不同集群/);
  });

  it("live numbers of same-named DevPods in two clusters: each row its own", async () => {
    const x = (cluster: string) => ({ ...mk("alice-x", "alice", "Running"), cluster });
    stubFetch({ "/api/devpods": { items: [x("a"), x("b")], pods: { "a/alice-x": { node: "na" }, "b/alice-x": { node: "nb" } } } });
    const conns: { url: string; h: SseHandlers }[] = [];
    const hub = new LiveHub({ open: (url, h) => (conns.push({ url, h }), () => {}) });
    const clusters = [
      { name: "a", connected: true },
      { name: "b", connected: true },
    ];
    renderWithProviders(<DevPodTable scope="mine" />, { hub, liveEnabled: true, clusters });
    await waitFor(() => expect(screen.getAllByRole("link", { name: "alice-x" })).toHaveLength(2));
    const item = (cluster: string, cpu: number) => ({ cluster, name: "alice-x", owner: "alice", cpu, cpuLimit: 4, mem: null, memLimit: 0, gpu: [], netRx: null, netTx: null, netKind: "", rdmaRx: null, rdmaTx: null, rdmaKind: "", ioR: null, ioW: null, sessions: [], idle: false });
    await waitFor(() => expect(conns.length).toBeGreaterThan(0));
    act(() => conns.forEach((c) => c.h.onMessage({ ts: new Date().toISOString(), items: [item("a", 1), item("b", 3)] })));
    const rowOf = (node: string) => screen.getAllByRole("row").find((r) => within(r).queryByText(node))!;
    await waitFor(() => expect(rowOf("na").textContent).toContain("1/4"));
    expect(rowOf("nb").textContent).toContain("3/4");
  });

  it("GPU memory: its own column, used of total over the cards; power in the GPU column's hint", async () => {
    stubFetch({ "/api/devpods": { items: [list.items[0]], pods: list.pods } });
    const conns: { url: string; h: SseHandlers }[] = [];
    const hub = new LiveHub({ open: (url, h) => (conns.push({ url, h }), () => {}) });
    renderWithProviders(<DevPodTable scope="mine" />, { hub, liveEnabled: true });
    await screen.findByRole("link", { name: "alice-a" });
    const card = (index: number, memUsed: number, power: number) => ({ index, uuid: `GPU-${index}`, name: "A100", util: 50, memUsed, memTotal: 40960, power });
    const item = { name: "alice-a", owner: "alice", cpu: 1, cpuLimit: 4, mem: null, memLimit: 0, gpu: [card(0, 10240, 250), card(1, 20480, 300)], netRx: null, netTx: null, netKind: "", rdmaRx: null, rdmaTx: null, rdmaKind: "", ioR: null, ioW: null, sessions: [], idle: false };
    await waitFor(() => expect(conns.length).toBeGreaterThan(0));
    act(() => conns.forEach((c) => c.h.onMessage({ ts: new Date().toISOString(), items: [item] })));
    expect(screen.getByRole("columnheader", { name: /显存/ })).toBeTruthy();
    const row = (await screen.findByRole("link", { name: "alice-a" })).closest("tr")!;
    await waitFor(() => expect(row.textContent).toContain("30.0G/80.0G"));
    expect(within(row).getByTitle(/GPU0 50% · 10240\/40960 MiB · 250 W/)).toBeTruthy();
  });
});
