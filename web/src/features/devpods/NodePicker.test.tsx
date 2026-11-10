import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen } from "@testing-library/react";
import { NodePicker, nodeLabel, nodeWarning } from "./NodePicker";
import { renderWithProviders, stubFetch } from "../../test/render";
import type { NodeChoice } from "../../data/api";

const n = (o: Partial<NodeChoice>): NodeChoice => ({ name: "g206", ready: true, cpu: { allocatable: 64, allocated: 8 }, mem: { allocatable: 2 ** 30, allocated: 0 }, gpuTotal: 0, gpuUsed: 0, reservable: true, pending: 0, held: null, next: null, selectable: true, ...o });

describe("NodePicker", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("explains a node: held by someone else, about to be reserved, not ready", () => {
    expect(nodeWarning(n({ selectable: false, held: { user: "bob", until: "2026-11-12T09:00:00Z" } }))).toBe("g206 正被 bob 预约至 11-12 17:00，现在不能使用。");
    expect(nodeWarning(n({ next: { user: "bob", start: "2026-11-12T06:30:00Z" } }))).toBe("g206 将于 11-12 14:30 被 bob 预约，届时这个 DevPod 会被自动休眠。");
    expect(nodeWarning(n({ ready: false }))).toMatch(/未就绪/);
    expect(nodeWarning(n({}))).toBeNull();
    expect(nodeLabel(n({ gpuModel: "A100", gpuTotal: 8, held: { user: "me", until: "2026-11-12T09:00:00Z" } }))).toMatch(/^g206 · A100 ×8 · .*你的预约至 11-12 17:00$/);
  });

  it("offers the scheduler's choice, disables nodes held by others, warns about upcoming reservations", async () => {
    stubFetch({
      "/api/nodes": {
        items: [n({ name: "free" }), n({ name: "held", selectable: false, held: { user: "bob", until: "2026-11-12T09:00:00Z" } }), n({ name: "soon", next: { user: "bob", start: "2026-11-12T06:30:00Z" } })],
      },
    });
    const seen: string[] = [];
    function Host() {
      const [v, setV] = useState("");
      return <NodePicker value={v} onChange={(x) => (seen.push(x), setV(x))} />;
    }
    renderWithProviders(<Host />);
    const held = (await screen.findByRole("option", { name: /^held/ })) as HTMLOptionElement;
    expect(held.disabled).toBe(true);
    expect((screen.getByRole("option", { name: /^free/ }) as HTMLOptionElement).disabled).toBe(false);
    expect((screen.getByRole("option", { name: "不指定（调度器选择）" }) as HTMLOptionElement).selected).toBe(true);
    fireEvent.change(screen.getByLabelText("节点"), { target: { value: "soon" } });
    expect(seen).toEqual(["soon"]);
    expect(screen.getByRole("note").textContent).toMatch(/将于 11-12 14:30 被 bob 预约/);
  });
});
