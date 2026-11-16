import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { ReservationsPage } from "./ReservationsPage";
import { renderWithProviders, stubFetch, stubMe } from "../../test/render";

const now = "2026-11-12T02:10:00Z"; // 10:10 in Shanghai
const rules = { granularitySeconds: 1800, maxDurationSeconds: 86400, maxAdvanceSeconds: 1209600, clearNoticeSeconds: 1800, timezone: "Asia/Shanghai", now };
const node = (name: string, o: object = {}) => ({
  name, ready: true, cpu: { allocatable: 64, allocated: 8 }, mem: { allocatable: 1, allocated: 0 }, gpuModel: "A100", gpuTotal: 8, gpuUsed: 0,
  reservable: true, reservationRequired: true, access: "reservation", pending: 0, mine: null, selectable: false, ...o,
});
const block = (o: object) => ({ id: "r1", node: "g206", user: "alice", start: "2026-11-12T04:00:00Z", end: "2026-11-12T06:00:00Z", phase: "Upcoming", mine: true, ...o });
const me = () => stubMe({ features: { ...stubMe().features, reservations: true } });

describe("ReservationsPage", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });
  const at = () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date(now));
  };

  it("lists the reservable machines the rules ask a reservation for; overlapping ones share a row; the form reserves at once", async () => {
    at();
    const calls = stubFetch({
      "/api/reservations/rules": rules,
      "/api/nodes": { items: [node("g206"), node("g207", { reservationRequired: false, access: "ok", selectable: true }), node("c101", { reservable: false })] },
      "/api/reservations": { items: [block({ id: "b1", user: "bob", mine: false }), block({ id: "b2", user: "carol", mine: false, start: "2026-11-12T04:30:00Z", end: "2026-11-12T05:30:00Z" })] },
      "POST /api/reservations": block({ id: "new" }),
    });
    renderWithProviders(<ReservationsPage />, { me: me() });
    const row = await screen.findByRole("group", { name: "g206 的预约" });
    expect(screen.queryByRole("group", { name: "g207 的预约" })).toBeNull(); // no reservation needed there
    expect(screen.queryByRole("group", { name: "c101 的预约" })).toBeNull();
    expect(await within(row).findByText(/bob · 12:00–14:00/)).toBeTruthy();
    expect(within(row).getByText(/carol · 12:30–13:30/)).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "预约节点" }));
    const dialog = within(screen.getByRole("dialog"));
    expect((dialog.getByLabelText("开始") as HTMLInputElement).value).toBe("2026-11-12T10:30");
    fireEvent.change(dialog.getByLabelText("结束"), { target: { value: "2026-11-12T11:45" } });
    expect(dialog.getByRole("alert").textContent).toMatch(/对齐/);
    fireEvent.change(dialog.getByLabelText("结束"), { target: { value: "2026-11-12T12:30" } }); // into bob's: allowed
    expect(dialog.queryByRole("alert")).toBeNull();
    fireEvent.click(dialog.getByRole("button", { name: "预约" }));
    await waitFor(() => expect(calls.find((c) => c.method === "POST")?.body).toBe('{"node":"g206","start":"2026-11-12T02:30:00.000Z","end":"2026-11-12T04:30:00.000Z"}'));
  });

  it("my reservations: an active one has its end moved or is released, an upcoming one is cancelled", async () => {
    at();
    const calls = stubFetch({
      "/api/reservations/rules": rules,
      "/api/nodes": { items: [node("g206")] },
      "/api/reservations": { items: [block({ id: "act", start: "2026-11-12T01:00:00Z", end: "2026-11-12T03:00:00Z", phase: "Active" }), block({ id: "up" })] },
      "PATCH /api/reservations/act": block({ id: "act" }),
      "DELETE /api/reservations/up": new Response(null, { status: 204 }),
    });
    renderWithProviders(<ReservationsPage />, { me: me() });
    const active = (await screen.findByText("进行中")).closest("tr")!;
    fireEvent.click(within(active as HTMLElement).getByRole("button", { name: "修改" }));
    const dialog = within(screen.getByRole("dialog"));
    expect((dialog.getByLabelText("开始") as HTMLInputElement).disabled).toBe(true);
    expect((dialog.getByLabelText("结束") as HTMLInputElement).value).toBe("2026-11-12T11:30"); // one slot later
    fireEvent.click(dialog.getByRole("button", { name: "保存" }));
    await waitFor(() => expect(calls.find((c) => c.method === "PATCH")?.body).toBe('{"end":"2026-11-12T03:30:00.000Z"}'));

    const upcoming = screen.getByText("即将开始").closest("tr")!;
    fireEvent.click(within(upcoming as HTMLElement).getByRole("button", { name: "取消" }));
    fireEvent.click(screen.getByRole("button", { name: "取消预约" }));
    await waitFor(() => expect(calls.some((c) => c.method === "DELETE" && c.url.pathname === "/api/reservations/up")).toBe(true));
    expect(screen.getByRole("button", { name: "提前释放" })).toBeTruthy();
  });
});
