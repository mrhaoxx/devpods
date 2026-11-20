import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { ManagedNodesPage } from "./ManagedNodesPage";
import { renderWithProviders, stubFetch, stubMe } from "../../test/render";

describe("ManagedNodesPage", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("shows the machines one manages and switches one", async () => {
    const calls = stubFetch({
      "/api/nodes/managed": { items: [{ name: "g1", reservable: false, ready: true, gpuModel: "NVIDIA-H100", gpuTotal: 8 }] },
      "PUT /api/admin/nodes/g1/reservable": { reservable: true, pending: 0 },
    });
    renderWithProviders(<ManagedNodesPage />, { me: stubMe({ permissions: { setReservable: true, viewAudit: false, disconnectSessions: false } }), route: "/nodes/managed" });
    const toggle = (await screen.findByLabelText("g1 可预约")) as HTMLInputElement;
    expect(toggle.checked).toBe(false);
    fireEvent.click(toggle);
    await waitFor(() => expect(calls.some((c) => c.method === "PUT")).toBe(true));
    expect(calls.find((c) => c.method === "PUT")!.body).toBe('{"reservable":true}');
  });
});
