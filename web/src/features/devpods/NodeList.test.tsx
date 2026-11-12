import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, within } from "@testing-library/react";
import { useState } from "react";
import { NodeList } from "./NodeList";
import { renderWithProviders, stubFetch } from "../../test/render";
import type { NodeChoice } from "../../data/api";

const node = (name: string, o: Partial<NodeChoice> = {}): NodeChoice => ({
  name,
  ready: true,
  cpu: { allocatable: 128, allocated: 8 },
  mem: { allocatable: 512 << 30, allocated: 0 },
  gpuTotal: 0,
  gpuUsed: 0,
  reservable: false,
  pending: 0,
  held: null,
  next: null,
  selectable: true,
  ...o,
});
const nodes = {
  items: [
    node("m601"),
    node("m602", { gpuModel: "NVIDIA-A100-80GB-PCIe", gpuTotal: 2, gpuUsed: 2 }),
    node("m604", { ready: false }),
    node("m800", { gpuModel: "NVIDIA-GeForce-RTX-4090", gpuTotal: 4, gpuUsed: 2 }),
    node("zjusct-920b-1", { selectable: false, held: { user: "bob", until: new Date(Date.now() + 3600e3).toISOString() } }),
  ],
};

function Host({ single, max, initial = [] }: { single?: boolean; max?: number; initial?: string[] }) {
  const [picked, setPicked] = useState<string[]>(initial);
  return (
    <>
      <NodeList picked={picked} setPicked={setPicked} single={single} max={max} />
      <output data-testid="picked">{picked.join(",")}</output>
    </>
  );
}
const picked = () => screen.getByTestId("picked").textContent;
const box = (name: string) => (screen.queryByRole("checkbox", { name }) ?? screen.getByRole("radio", { name })) as HTMLInputElement;

describe("NodeList", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("a row per node: its GPUs free, its state; one held by someone else cannot be picked", async () => {
    stubFetch({ "/api/nodes": nodes });
    renderWithProviders(<Host />);
    const m800 = (await screen.findByRole("checkbox", { name: "m800" })).closest("label")!;
    expect(m800.textContent).toContain("RTX-4090");
    expect(m800.textContent).toContain("空闲 2/4");
    expect(box("m604").closest("label")!.textContent).toContain("未就绪");
    expect(box("zjusct-920b-1").disabled).toBe(true);
    expect(box("zjusct-920b-1").closest("label")!.textContent).toContain("bob");
  });

  it("picked in order, numbered from 0; × takes one out", async () => {
    stubFetch({ "/api/nodes": nodes });
    renderWithProviders(<Host />);
    fireEvent.click(await screen.findByRole("checkbox", { name: "m800" }));
    fireEvent.click(box("m601"));
    expect(picked()).toBe("m800,m601");
    const chosen = within(screen.getByRole("list", { name: "已选节点" }));
    expect(chosen.getAllByRole("listitem").map((li) => li.textContent?.replace("×", "").trim())).toEqual(["0 m800", "1 m601"]);
    fireEvent.click(chosen.getByRole("button", { name: "取消 m800" }));
    expect(picked()).toBe("m601");
    fireEvent.click(box("m601"));
    expect(picked()).toBe("");
  });

  it("a search by name or GPU model; only the usable; only with GPUs free", async () => {
    stubFetch({ "/api/nodes": nodes });
    renderWithProviders(<Host />);
    await screen.findByRole("checkbox", { name: "m601" });
    fireEvent.change(screen.getByLabelText("搜索节点"), { target: { value: "a100" } });
    expect(screen.queryByRole("checkbox", { name: "m601" })).toBeNull();
    expect(box("m602")).toBeTruthy();
    fireEvent.change(screen.getByLabelText("搜索节点"), { target: { value: "" } });
    fireEvent.click(screen.getByLabelText("只看可用"));
    expect(screen.queryByRole("checkbox", { name: "m604" })).toBeNull();
    expect(screen.queryByRole("checkbox", { name: "zjusct-920b-1" })).toBeNull();
    fireEvent.click(screen.getByLabelText("只看有空闲 GPU"));
    expect(screen.getAllByRole("checkbox", { name: /^(m|z)/ }).map((b) => b.getAttribute("aria-label"))).toEqual(["m800"]);
  });

  it("all of what the filter shows, up to the most allowed", async () => {
    stubFetch({ "/api/nodes": nodes });
    renderWithProviders(<Host max={2} />);
    await screen.findByRole("checkbox", { name: "m601" });
    fireEvent.click(screen.getByRole("button", { name: "全选筛选结果" }));
    expect(picked()).toBe("m601,m602");
    expect(screen.getByText(/最多选 2 个/)).toBeTruthy();
    expect(box("m800").disabled).toBe(true); // full
  });

  it("single: one node, a click replaces it", async () => {
    stubFetch({ "/api/nodes": nodes });
    renderWithProviders(<Host single />);
    fireEvent.click(await screen.findByRole("radio", { name: "m601" }));
    fireEvent.click(box("m800"));
    expect(picked()).toBe("m800");
    expect(screen.queryByRole("button", { name: "全选筛选结果" })).toBeNull();
  });
});
