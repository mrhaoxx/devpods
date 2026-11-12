import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, screen, waitFor } from "@testing-library/react";
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

function Where() {
  const l = useLocation();
  return <div data-testid="where">{l.search}</div>;
}

let calls: ReturnType<typeof stubFetch> = [];

function setup(opts: { route?: string; admin?: boolean; live?: boolean } = {}) {
  const cs = stubFetch({
    "/api/devpods/alice-x": { devpod: dp, pod: { node: "g206", ip: "10.1.2.3", startedAt: "2026-11-10T10:00:00Z" } },
    "/api/audit/sessions": { items: [], next: "" },
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
  const me = stubMe({ admin: !!opts.admin, user: opts.admin ? "root" : "alice" });
  me.features.telemetry = opts.live !== false;
  renderWithProviders(
    <Routes>
      <Route
        path="/devpods/:name"
        element={
          <>
            <DetailPage />
            <Where />
          </>
        }
      />
    </Routes>,
    { route: opts.route ?? "/devpods/alice-x", me, hub, liveEnabled: opts.live !== false },
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
  afterEach(() => vi.unstubAllGlobals());

  it("cards show values from the metrics event", async () => {
    const conns = setup();
    await screen.findByRole("heading", { name: "alice-x" });
    act(() => conns[0].h.onMessage({ kind: "metrics", metrics: sample }));
    expect(screen.getByTestId("card-cpu").textContent).toContain("3.5/8");
    expect(screen.getByTestId("card-mem").textContent).toContain("4.0G/16.0G");
  });

  it("cards show — without telemetry", async () => {
    setup({ live: false });
    await screen.findByRole("heading", { name: "alice-x" });
    for (const id of ["cpu", "mem", "gpu", "net", "rdma", "io"]) {
      expect(screen.getByTestId(`card-${id}`).textContent).toContain("—");
    }
  });

  it("tab follows ?tab= and switching tab updates the URL", async () => {
    setup({ route: "/devpods/alice-x?tab=events" });
    await screen.findByRole("heading", { name: "alice-x" });
    expect(screen.getByRole("tab", { name: "事件" }).getAttribute("aria-selected")).toBe("true");
    fireEvent.click(screen.getByRole("tab", { name: "会话" }));
    expect(screen.getByTestId("where").textContent).toBe("?tab=sessions");
  });

  it("编辑 opens the YAML tab", async () => {
    setup({ route: "/devpods/alice-x" });
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
});
