import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { DevPodTable } from "./DevPodTable";
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
    expect(screen.queryByLabelText("选择 alice-a")).toBeNull();
  });
});
