import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, screen, within } from "@testing-library/react";
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
  access: "ok",
  mine: null,
  selectable: true,
  ...o,
});
const nodes = {
  items: [
    node("m601"),
    node("m602", { gpuModel: "NVIDIA-A100-80GB-PCIe", gpuTotal: 2, gpuUsed: 2 }),
    node("m604", { ready: false }),
    node("m800", { gpuModel: "NVIDIA-GeForce-RTX-4090", gpuTotal: 4, gpuUsed: 2 }),
    node("zjusct-920b-1", { selectable: false, access: "denied" }),
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

// rows 30 px tall from the top, in the order shown: the row under a point
function rowsAt() {
  const rows = () => [...document.querySelectorAll<HTMLElement>("li[data-name]")];
  Object.defineProperty(document, "elementFromPoint", { value: (_x: number, y: number) => rows()[Math.floor(y / 30)] ?? null, configurable: true });
  return (name: string) => rows().findIndex((r) => r.dataset.name === name) * 30 + 10;
}
const dragFrom = (from: string, to: string, yOf: (n: string) => number) => {
  const start = screen.getByRole("checkbox", { name: from }).closest("label")!;
  fireEvent.mouseDown(start, { button: 0, clientX: 5, clientY: yOf(from) });
  fireEvent.mouseMove(document, { clientX: 5, clientY: yOf(to) });
  fireEvent.mouseUp(document, { clientX: 5, clientY: yOf(to) });
  return start;
};

describe("NodeList", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    Reflect.deleteProperty(document, "elementFromPoint");
    localStorage.clear();
  });

  it("a drag across rows picks them in the order passed over; from a picked row it unpicks", async () => {
    stubFetch({ "/api/nodes": nodes });
    renderWithProviders(<Host />);
    await screen.findByRole("checkbox", { name: "m601" });
    const yOf = rowsAt();
    const start = dragFrom("m601", "m604", yOf);
    expect(picked()).toBe("m601,m602,m604");
    // the click that ends the drag picks nothing more
    fireEvent.click(start);
    expect(picked()).toBe("m601,m602,m604");
    // from a picked row: those passed over are unpicked
    dragFrom("m602", "m604", yOf);
    expect(picked()).toBe("m601");
  });

  it("dragged upwards, the order is the drag's; rows that cannot be picked are passed over; no more than max", async () => {
    stubFetch({ "/api/nodes": nodes });
    renderWithProviders(<Host max={3} />);
    await screen.findByRole("checkbox", { name: "m601" });
    const yOf = rowsAt();
    dragFrom("zjusct-920b-1", "m601", yOf); // starts on a row that cannot be picked: nothing
    expect(picked()).toBe("");
    dragFrom("m800", "m601", yOf);
    expect(picked()).toBe("m800,m604,m602");
  });

  it("remembers the filters for next time", async () => {
    stubFetch({ "/api/nodes": nodes });
    const first = renderWithProviders(<Host />);
    fireEvent.click(await screen.findByRole("checkbox", { name: "只看可用" }));
    fireEvent.click(screen.getByRole("checkbox", { name: "只看有空闲 GPU" }));
    first.unmount();
    renderWithProviders(<Host />);
    expect((await screen.findByRole("checkbox", { name: "只看可用" }) as HTMLInputElement).checked).toBe(true);
    expect((screen.getByRole("checkbox", { name: "只看有空闲 GPU" }) as HTMLInputElement).checked).toBe(true);
    expect(await screen.findByRole("checkbox", { name: "m800" })).toBeTruthy();
    expect(screen.queryByRole("checkbox", { name: "m601" })).toBeNull(); // no free GPU
  });

  it("rows of differing heights: scrolling never makes the measured height go back and forth (React #185)", async () => {
    stubFetch({ "/api/nodes": { items: Array.from({ length: 600 }, (_, i) => node(`n${i}`)) } });
    const ownH = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientHeight");
    Object.defineProperty(HTMLElement.prototype, "clientHeight", {
      configurable: true,
      get() {
        return (this as HTMLElement).tagName === "UL" ? 290 : 0;
      },
    });
    const ownRect = HTMLElement.prototype.getBoundingClientRect;
    HTMLElement.prototype.getBoundingClientRect = function (this: HTMLElement) {
      const name = this.hasAttribute("data-node") ? (this.querySelector("input")?.getAttribute("aria-label") ?? "") : "";
      const h = name ? (Number(name.slice(1)) % 2 ? 58 : 29) : 0;
      return { x: 0, y: 0, top: 0, left: 0, width: 100, height: h, right: 100, bottom: h, toJSON: () => ({}) };
    };
    const errors = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      renderWithProviders(<Host />);
      await screen.findByRole("checkbox", { name: "n0" });
      const list = screen.getAllByRole("list").find((l) => l.tagName === "UL")!;
      for (const top of [29 * 101, 29 * 333, 29 * 501]) {
        list.scrollTop = top;
        fireEvent.scroll(list);
        await act(() => new Promise((r) => setTimeout(r, 30)));
      }
      expect(errors.mock.calls.some((c) => /Maximum update depth|#185/.test(String(c[0])))).toBe(false);
    } finally {
      errors.mockRestore();
      HTMLElement.prototype.getBoundingClientRect = ownRect;
      if (ownH) Object.defineProperty(HTMLElement.prototype, "clientHeight", ownH);
      else delete (HTMLElement.prototype as { clientHeight?: number }).clientHeight;
    }
  });

  it("a row per node: its GPUs free, its state; one the user has no access to cannot be picked", async () => {
    stubFetch({ "/api/nodes": nodes });
    renderWithProviders(<Host />);
    const m800 = (await screen.findByRole("checkbox", { name: "m800" })).closest("label")!;
    expect(m800.textContent).toContain("RTX-4090");
    expect(m800.textContent).toContain("空闲 2/4");
    expect(box("m604").closest("label")!.textContent).toContain("未就绪");
    expect(box("zjusct-920b-1").disabled).toBe(true);
    expect(box("zjusct-920b-1").closest("label")!.textContent).toContain("无权限");
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
