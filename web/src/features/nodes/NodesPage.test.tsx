import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { useLocation } from "react-router-dom";
import { NodesPage } from "./NodesPage";
import { renderWithProviders, stubFetch, stubMe } from "../../test/render";

const node = (name: string, ready: boolean, labels: Record<string, string>, gpuTotal = 0) => ({
  name,
  ready,
  labels,
  cpu: { allocatable: 64, allocated: 16 },
  mem: { allocatable: 256 * 1024 ** 3, allocated: 32 * 1024 ** 3 },
  gpu: { model: gpuTotal ? "A100" : undefined, total: gpuTotal, used: gpuTotal ? 1 : 0, cards: [] },
  rdma: [],
  devpods: 2,
});

const nodes = {
  items: [node("g206", true, { gpu: "a100" }, 4), node("c101", false, {}), node("c102", true, {})],
  live: true,
};

function Where() {
  const l = useLocation();
  return <div data-testid="where">{l.search}</div>;
}

const tableNames = () =>
  screen
    .getAllByRole("row")
    .slice(1)
    .map((r) => within(r).queryByTestId("node-name")?.textContent)
    .filter(Boolean);

describe("nodes page", () => {
  afterEach(() => vi.unstubAllGlobals());

  function setup(route = "/admin/nodes") {
    stubFetch({
      "/api/admin/nodes": nodes,
      "/api/admin/nodes/g206": {
        node: { ...node("g206", true, { gpu: "a100" }, 4), gpu: { model: "A100", total: 4, used: 1, cards: [{ index: 0, uuid: "GPU-1", devpod: "alice-train", owner: "alice", util: 87 }] } },
        devpods: [{ name: "alice-train", owner: "alice", phase: "Running", cpu: 3, mem: 1024, gpu: 1 }],
        live: true,
      },
    });
    renderWithProviders(
      <>
        <NodesPage />
        <Where />
      </>,
      { me: stubMe({ admin: true }), route },
    );
  }

  it("opens the drawer from ?node=g206 and closing clears it", async () => {
    setup("/admin/nodes?node=g206");
    expect(await screen.findByRole("dialog", { name: /g206/ })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "关闭" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.getByTestId("where").textContent).toBe("");
  });

  it("NotReady filter keeps only not-ready nodes", async () => {
    setup();
    await screen.findByText("c101");
    fireEvent.click(screen.getByRole("button", { name: /^NotReady/ }));
    await waitFor(() => expect(tableNames()).toEqual(["c101"]));
  });

  it("label filter gpu=a100 matches labels", async () => {
    setup("/admin/nodes?label=gpu%3Da100");
    await screen.findByText("g206");
    expect(tableNames()).toEqual(["g206"]);
  });

  it("drawer lists GPU occupants", async () => {
    setup("/admin/nodes?node=g206");
    const dlg = await screen.findByRole("dialog", { name: /g206/ });
    expect(await within(dlg).findByText(/GPU0/)).toBeTruthy();
    expect(within(dlg).getAllByText("alice-train").length).toBeGreaterThan(0);
    expect(within(dlg).getByText("87%")).toBeTruthy();
  });

  it("every cluster's nodes: a name in two clusters is two rows; the filter narrows them", async () => {
    stubFetch({ "/api/admin/nodes": { items: [{ ...node("m602", true, {}), cluster: "a" }, { ...node("m602", true, {}), cluster: "b" }], live: true } });
    const clusters = [
      { name: "a", connected: true },
      { name: "b", connected: true },
    ];
    const first = renderWithProviders(<NodesPage />, { me: stubMe({ admin: true }), route: "/admin/nodes", clusters });
    await waitFor(() => expect(tableNames()).toEqual(["m602", "m602"]));
    expect(screen.getByRole("columnheader", { name: /集群/ })).toBeTruthy();
    first.unmount();
    renderWithProviders(<NodesPage />, { me: stubMe({ admin: true }), route: "/admin/nodes?cluster=b", clusters });
    await waitFor(() => expect(tableNames()).toEqual(["m602"]));
  });
});
