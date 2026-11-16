import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen } from "@testing-library/react";
import { NodePicker, nodeLabel, nodeWarning } from "./NodePicker";
import { renderWithProviders, stubFetch } from "../../test/render";
import type { NodeChoice } from "../../data/api";

const n = (o: Partial<NodeChoice>): NodeChoice => ({
  name: "g206", ready: true, cpu: { allocatable: 64, allocated: 8 }, mem: { allocatable: 2 ** 30, allocated: 0 }, gpuTotal: 0, gpuUsed: 0,
  reservable: true, pending: 0, access: "ok", mine: null, selectable: true, ...o,
});

describe("NodePicker", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("explains a node: no access, needs a reservation, mine until, not ready", () => {
    expect(nodeWarning(n({ selectable: false, access: "denied" }))).toBe("g206：你没有使用这台机器的权限。");
    expect(nodeWarning(n({ selectable: false, access: "reservation", reservationRequired: true }))).toMatch(/^g206 需要预约才能使用/);
    expect(nodeWarning(n({ ready: false }))).toMatch(/未就绪/);
    expect(nodeWarning(n({}))).toBeNull();
    expect(nodeLabel(n({ selectable: false, access: "denied" }))).toMatch(/ · 无权限$/);
    expect(nodeLabel(n({ selectable: false, access: "reservation", reservationRequired: true }))).toMatch(/ · 需预约$/);
    expect(nodeLabel(n({ gpuModel: "A100", gpuTotal: 8, reservationRequired: true, mine: { user: "me", until: "2026-11-12T09:00:00Z" } }))).toMatch(/^g206 · A100 ×8 · .*你的预约至 11-12 17:00$/);
  });

  it("offers the scheduler's choice and disables the machines the user may not pick", async () => {
    stubFetch({
      "/api/nodes": { items: [n({ name: "free" }), n({ name: "barred", selectable: false, access: "denied" }), n({ name: "booked", selectable: false, access: "reservation", reservationRequired: true })] },
    });
    const seen: string[] = [];
    function Host() {
      const [v, setV] = useState("");
      return <NodePicker value={v} onChange={(x) => (seen.push(x), setV(x))} />;
    }
    renderWithProviders(<Host />);
    const barred = (await screen.findByRole("option", { name: /^barred/ })) as HTMLOptionElement;
    expect(barred.disabled).toBe(true);
    expect((screen.getByRole("option", { name: /^booked/ }) as HTMLOptionElement).disabled).toBe(true);
    expect((screen.getByRole("option", { name: /^free/ }) as HTMLOptionElement).disabled).toBe(false);
    expect((screen.getByRole("option", { name: "不指定（调度器选择）" }) as HTMLOptionElement).selected).toBe(true);
    fireEvent.change(screen.getByLabelText("节点"), { target: { value: "free" } });
    expect(seen).toEqual(["free"]);
  });
});
