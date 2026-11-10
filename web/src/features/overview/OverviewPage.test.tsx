import { afterEach, describe, expect, it, vi } from "vitest";
import { act, screen } from "@testing-library/react";
import { OverviewPage } from "./OverviewPage";
import { renderWithProviders, stubFetch, stubMe } from "../../test/render";
import { LiveHub } from "../../data/live/LiveHub";
import type { SseHandlers } from "../../data/sse";

const kpi = { cpuUsed: 7.5, cpuAllocatable: 64, memUsed: 0, memAllocatable: 0, gpuUsed: 3, gpuTotal: 8, running: 7, pending: 1, failed: 2, stopped: 30, sessions: 4, sessionUsers: 3, rdma: 0 };
const node = (name: string, ready: boolean) => ({ name, ready, cpuAllocatable: 32, cpuAllocated: 8, cpuUsed: 16, memAllocatable: 0, memAllocated: 0, memUsed: 0, gpuTotal: 4, gpuUsed: 1, devpods: 2 });

function setup(sessions: unknown) {
  stubFetch({ "/api/audit/sessions": sessions, "/api/admin/nodes": { items: [], live: true } });
  let h: SseHandlers | undefined;
  const hub = new LiveHub({
    open: (_u, handlers) => {
      h = handlers;
      return () => {};
    },
  });
  const me = stubMe({ admin: true });
  me.features.telemetry = true;
  renderWithProviders(<OverviewPage />, { me, hub, route: "/admin" });
  act(() => h!.onMessage({ ts: "", kpi, nodes: [node("g1", false), node("g2", true)], users: [{ user: "bob", devpods: 2, running: 1, cpu: 3, mem: 0, gpu: 1 }], attention: [] }));
}

describe("overview", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("renders KPI values from the overview stream", () => {
    setup({ items: [], next: "" });
    expect(screen.getByText("7.5/64")).toBeTruthy();
    expect(screen.getByText("3/8")).toBeTruthy();
    expect(screen.getByText("7 · 1 · 2")).toBeTruthy();
  });

  it("NotReady node cell is gray and says NotReady", () => {
    setup({ items: [], next: "" });
    const cell = screen.getByRole("button", { name: /g1/ });
    expect(cell.textContent).toContain("NotReady");
    expect(cell.className).toContain("bg-idle");
    expect(screen.getByRole("button", { name: /g2/ }).textContent).not.toContain("NotReady");
  });

  it("recent activity shows an error panel when sessions fail", async () => {
    setup(new Response(JSON.stringify({ code: "AUDIT_UNAVAILABLE", message: "clickhouse down" }), { status: 503 }));
    expect(await screen.findByText("审计数据暂不可用")).toBeTruthy();
  });
});
