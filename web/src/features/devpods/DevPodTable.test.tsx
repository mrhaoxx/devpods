import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { DevPodTable } from "./DevPodTable";
import { collabPatch } from "./BulkDialog";
import { renderWithProviders, stubFetch, stubMe } from "../../test/render";
import type { DevPod } from "../../data/api";

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

  it("my DevPods can be selected for bulk actions, without 迁移", async () => {
    stubFetch({ "/api/devpods": { items: [list.items[0]], pods: {} } });
    renderWithProviders(<DevPodTable scope="mine" />);
    await screen.findByRole("link", { name: "alice-a" });
    fireEvent.click(screen.getByLabelText("选择 alice-a"));
    const bar = within(screen.getByRole("toolbar", { name: "批量操作" }));
    expect(bar.getAllByRole("button").map((b) => b.textContent)).toEqual(["休眠", "启动", "协作者…", "设置镜像…", "重新部署…", "删除…", "取消选择"]);
  });

  it("bulk 设置镜像 patches every selected DevPod; 迁移 sends the node", async () => {
    const ok = { devpod: list.items[0], rebuild: true };
    const calls = stubFetch({
      "/api/devpods": list,
      "/api/admin/nodes": { items: [{ name: "g206", ready: true }], live: false },
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

    fireEvent.click(await screen.findByLabelText("选择 alice-a"));
    fireEvent.click(screen.getByRole("button", { name: "迁移…" }));
    await screen.findByRole("option", { name: "g206" });
    fireEvent.change(screen.getByLabelText("目标节点"), { target: { value: "g206" } });
    fireEvent.click(screen.getByRole("button", { name: "应用" }));
    await waitFor(() => expect(calls.some((c) => c.method === "PATCH" && c.body === '{"node":"g206"}')).toBe(true));
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
});
