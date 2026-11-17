import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { ReservationsPage } from "./ReservationsPage";
import { renderWithProviders, stubFetch, stubMe } from "../../test/render";

const now = "2026-11-12T02:10:00Z"; // 10:10 in Shanghai
const rules = { granularitySeconds: 600, maxDurationSeconds: 0, maxAdvanceSeconds: 1209600, clearNoticeSeconds: 1800, timezone: "Asia/Shanghai", now };
const node = (name: string, o: object = {}) => ({
  name, ready: true, cpu: { allocatable: 64, allocated: 8 }, mem: { allocatable: 1, allocated: 0 }, gpuModel: "A100", gpuTotal: 8, gpuUsed: 0,
  reservable: true, reservationRequired: true, access: "reservation", pending: 0, mine: null, selectable: false, ...o,
});
const block = (o: object) => ({ id: "r1", node: "g206", user: "alice", start: "2026-11-12T04:00:00Z", end: "2026-11-12T06:00:00Z", phase: "Upcoming", mine: true, ...o });
const me = () => stubMe({ features: { ...stubMe().features, reservations: true } });
type Dialog = ReturnType<typeof within>;
const input = (d: Dialog, label: string) => d.getByLabelText(label) as HTMLInputElement;

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
    // the system's own pickers; from the slot now is in (no gap before the reservation), an hour long
    expect(input(dialog, "开始").value).toBe("2026-11-12T10:10");
    expect(input(dialog, "结束").value).toBe("2026-11-12T11:10");
    // not on a ten-minute mark: rounded out, said, not refused
    fireEvent.change(input(dialog, "结束"), { target: { value: "2026-11-12T12:25" } }); // into bob's: allowed
    expect(dialog.queryByRole("alert")).toBeNull();
    expect(dialog.getByText(/实际预约：11\/12 四 10:10 – 11\/12 四 12:30/)).toBeTruthy();
    fireEvent.click(dialog.getByRole("button", { name: "预约" }));
    await waitFor(() => expect(calls.find((c) => c.method === "POST")?.body).toBe('{"node":"g206","start":"2026-11-12T02:10:00.000Z","end":"2026-11-12T04:30:00.000Z"}'));
  });

  it("a drag across rows picks those machines and that time", async () => {
    at();
    stubFetch({
      "/api/reservations/rules": rules,
      "/api/nodes": { items: [node("g206"), node("g207"), node("g208")] },
      "/api/reservations": { items: [] },
    });
    renderWithProviders(<ReservationsPage />, { me: me() });
    const tracks = await Promise.all(["g206", "g207", "g208"].map((n) => screen.findByRole("group", { name: `${n} 的预约` })));
    // the page is not laid out here: say where each row is, 1000 px for the 3 days shown
    tracks.forEach((t, i) => (t.getBoundingClientRect = () => ({ left: 0, width: 1000, top: i * 50, height: 50, right: 1000, bottom: (i + 1) * 50, x: 0, y: i * 50, toJSON: () => ({}) })));
    const elementFromPoint = (_x: number, y: number) => tracks[Math.min(2, Math.floor(y / 50))];
    if (!("PointerEvent" in window)) {
      // jsdom: pointer events as mouse events, so they carry their coordinates
      class PointerEvent extends MouseEvent {
        pointerId: number;
        constructor(type: string, init: PointerEventInit = {}) {
          super(type, init);
          this.pointerId = init.pointerId ?? 0;
        }
      }
      vi.stubGlobal("PointerEvent", PointerEvent);
    }
    Object.defineProperty(document, "elementFromPoint", { value: elementFromPoint, configurable: true });
    try {
      // 3 days from 11/12 00:00 Shanghai over 1000 px: 166.7 px ≈ 12:00, 194.4 px ≈ 13:59
      fireEvent.pointerDown(tracks[0], { clientX: 166.7, clientY: 10, pointerId: 1 });
      fireEvent.pointerMove(tracks[0], { clientX: 194.4, clientY: 120, pointerId: 1 });
      fireEvent.pointerUp(tracks[0], { clientX: 194.4, clientY: 120, pointerId: 1 });
      const dialog = within(await screen.findByRole("dialog"));
      for (const n of ["g206", "g207", "g208"]) expect((dialog.getByLabelText(n) as HTMLInputElement).checked).toBe(true);
      expect(input(dialog, "开始").value).toBe("2026-11-12T12:00");
      expect(input(dialog, "结束").value).toBe("2026-11-12T14:00");
      expect(dialog.getByRole("button", { name: "预约 3 台" })).toBeTruthy();
    } finally {
      Reflect.deleteProperty(document, "elementFromPoint");
    }
  });

  it("reserves several machines at once; one refused is said and stays picked, the others are made", async () => {
    at();
    const calls = stubFetch({
      "/api/reservations/rules": rules,
      "/api/nodes": { items: [node("g206"), node("g207"), node("g208")] },
      "/api/reservations": { items: [] },
      "POST /api/reservations": (_u: URL, init?: RequestInit) =>
        String(init?.body).includes('"g207"')
          ? new Response(JSON.stringify({ code: "NOT_RESERVABLE", message: "g207 is not reservable" }), { status: 409 })
          : block({ id: "new" }),
    });
    renderWithProviders(<ReservationsPage />, { me: me() });
    fireEvent.click(await screen.findByRole("button", { name: "预约节点" }));
    const dialog = within(screen.getByRole("dialog"));
    expect((dialog.getByLabelText("g206") as HTMLInputElement).checked).toBe(true); // the one it was opened on
    fireEvent.click(dialog.getByLabelText("g207"));
    fireEvent.click(dialog.getByLabelText("g208"));
    fireEvent.click(dialog.getByRole("button", { name: "预约 3 台" }));
    await waitFor(() => expect(calls.filter((c) => c.method === "POST")).toHaveLength(3));
    expect(calls.filter((c) => c.method === "POST").map((c) => JSON.parse(c.body!).node).sort()).toEqual(["g206", "g207", "g208"]);
    expect(await dialog.findByText(/g207：这台机器没有开放预约/)).toBeTruthy();
    expect(dialog.getByText(/2 台已预约/)).toBeTruthy();
    expect((dialog.getByLabelText("g207") as HTMLInputElement).checked).toBe(true);
    expect((dialog.getByLabelText("g206") as HTMLInputElement).checked).toBe(false);
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
    // it has started: its start stays; its end is offered one step later
    expect(input(dialog, "开始").disabled).toBe(true);
    expect(input(dialog, "结束").value).toBe("2026-11-12T11:10");
    fireEvent.click(dialog.getByRole("button", { name: "保存" }));
    await waitFor(() => expect(calls.find((c) => c.method === "PATCH")?.body).toBe('{"end":"2026-11-12T03:10:00.000Z"}'));

    const upcoming = screen.getByText("即将开始").closest("tr")!;
    fireEvent.click(within(upcoming as HTMLElement).getByRole("button", { name: "取消" }));
    fireEvent.click(screen.getByRole("button", { name: "取消预约" }));
    await waitFor(() => expect(calls.some((c) => c.method === "DELETE" && c.url.pathname === "/api/reservations/up")).toBe(true));
    expect(screen.getByRole("button", { name: "提前释放" })).toBeTruthy();
  });
});
