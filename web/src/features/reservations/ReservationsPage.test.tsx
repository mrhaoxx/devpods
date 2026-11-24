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

  it("lists the reservable machines I may use, needing a reservation or not; overlapping ones share a row; the form reserves at once", async () => {
    at();
    const calls = stubFetch({
      "/api/reservations/rules": rules,
      "/api/nodes": {
        items: [
          node("g206"),
          node("g207", { reservationRequired: false, access: "ok", selectable: true }),
          node("g208", { reservationRequired: false, access: "denied" }),
          node("c101", { reservable: false }),
        ],
      },
      "/api/reservations": { items: [block({ id: "b1", user: "bob", mine: false }), block({ id: "b2", user: "carol", mine: false, start: "2026-11-12T04:30:00Z", end: "2026-11-12T05:30:00Z" })] },
      "POST /api/reservations": block({ id: "new" }),
    });
    renderWithProviders(<ReservationsPage />, { me: me() });
    const row = await screen.findByRole("group", { name: "g206 的预约" });
    expect(screen.getByRole("group", { name: "g207 的预约" })).toBeTruthy(); // no reservation needed there: reservable all the same
    expect(screen.queryByRole("group", { name: "g208 的预约" })).toBeNull(); // barred
    expect(screen.queryByRole("group", { name: "c101 的预约" })).toBeNull(); // not reservable
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
    // the page is not laid out here: say where each row is — the track holds the whole bookable window (15 days here), 3 days a 1000 px as the 3-day zoom shows it
    tracks.forEach((t, i) => (t.getBoundingClientRect = () => ({ left: 0, width: 5000, top: i * 50, height: 50, right: 5000, bottom: (i + 1) * 50, x: 0, y: i * 50, toJSON: () => ({}) })));
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
      // 15 days from 11/12 00:00 Shanghai over 5000 px (3 days a 1000 px): 166.7 px ≈ 12:00, 194.4 px ≈ 13:59
      // a press there starts no text selection
      expect(fireEvent.pointerDown(tracks[0], { clientX: 166.7, clientY: 10, pointerId: 1, cancelable: true })).toBe(false);
      fireEvent.pointerMove(tracks[0], { clientX: 194.4, clientY: 120, pointerId: 1 });
      // while dragging: no text is selected, and the time is said once, on the top row
      expect(tracks[0].closest("[data-row]")!.parentElement!.className).toMatch(/select-none/);
      expect(screen.getAllByText(/12:00–14:00 ·/)).toHaveLength(1);
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

  it("a drag from the past into the future starts at the slot now is in; one wholly past cannot be reserved", async () => {
    at();
    stubFetch({
      "/api/reservations/rules": rules,
      "/api/nodes": { items: [node("g206")] },
      "/api/reservations": { items: [] },
    });
    renderWithProviders(<ReservationsPage />, { me: me() });
    const track = await screen.findByRole("group", { name: "g206 的预约" });
    track.getBoundingClientRect = () => ({ left: 0, width: 5000, top: 0, height: 50, right: 5000, bottom: 50, x: 0, y: 0, toJSON: () => ({}) });
    if (!("PointerEvent" in window)) {
      class PointerEvent extends MouseEvent {
        pointerId: number;
        constructor(type: string, init: PointerEventInit = {}) {
          super(type, init);
          this.pointerId = init.pointerId ?? 0;
        }
      }
      vi.stubGlobal("PointerEvent", PointerEvent);
    }
    Object.defineProperty(document, "elementFromPoint", { value: () => track, configurable: true });
    try {
      // 15 days over 5000 px (3 days a 1000 px): 111.1 px ≈ 08:00, 125 px ≈ 09:00, 194.4 px ≈ 13:59; now is 10:1x
      fireEvent.pointerDown(track, { clientX: 111.2, clientY: 10, pointerId: 1, cancelable: true });
      fireEvent.pointerMove(track, { clientX: 125.1, clientY: 10, pointerId: 1 });
      expect(screen.getByText(/不可预约/)).toBeTruthy();
      fireEvent.pointerUp(track, { clientX: 125.1, clientY: 10, pointerId: 1 });
      expect(screen.queryByRole("dialog")).toBeNull();

      fireEvent.pointerDown(track, { clientX: 125.1, clientY: 10, pointerId: 1, cancelable: true });
      fireEvent.pointerMove(track, { clientX: 194.4, clientY: 10, pointerId: 1 });
      expect(screen.getByText(/10:10–14:00 ·/)).toBeTruthy();
      expect(screen.queryByText(/不可预约/)).toBeNull();
      fireEvent.pointerUp(track, { clientX: 194.4, clientY: 10, pointerId: 1 });
      const dialog = within(await screen.findByRole("dialog"));
      expect(input(dialog, "开始").value).toBe("2026-11-12T10:10");
      expect(input(dialog, "结束").value).toBe("2026-11-12T14:00");
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

  it("my reservations come a page at a time: 加载更多 brings the earlier ones", async () => {
    at();
    const calls = stubFetch({
      "/api/reservations/rules": rules,
      "/api/nodes": { items: [node("g206")] },
      "/api/reservations": (url: URL) =>
        url.searchParams.get("mine") !== "1"
          ? { items: [] } // the timeline's
          : url.searchParams.get("cursor") === "c1"
            ? { items: [block({ id: "older", start: "2026-10-01T01:00:00Z", end: "2026-10-01T02:00:00Z", phase: "Ended" })] }
            : { items: [block({ id: "newer" })], next: "c1" },
    });
    renderWithProviders(<ReservationsPage />, { me: me() });
    const table = within(await screen.findByRole("table", { name: "我的预约" }));
    await waitFor(() => expect(table.getAllByRole("row")).toHaveLength(2)); // the header and one
    fireEvent.click(screen.getByRole("button", { name: "加载更多" }));
    await waitFor(() => expect(table.getAllByRole("row")).toHaveLength(3));
    expect(table.getByText("已结束")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "加载更多" })).toBeNull(); // no page after
    const mine = calls.filter((c) => c.url.searchParams.get("mine") === "1");
    expect(mine[0].url.searchParams.get("limit")).toBe("50");
  });

  it("my reservations are cancelled together: picked, confirmed, each released or cancelled", async () => {
    at();
    const calls = stubFetch({
      "/api/reservations/rules": rules,
      "/api/nodes": { items: [node("g206"), node("g207")] },
      "/api/reservations": {
        items: [
          block({ id: "act", start: "2026-11-12T01:00:00Z", end: "2026-11-12T03:00:00Z", phase: "Active" }),
          block({ id: "up", node: "g207" }),
          block({ id: "old", start: "2026-11-11T01:00:00Z", end: "2026-11-11T02:00:00Z", phase: "Ended" }),
        ],
      },
      "DELETE /api/reservations/act": new Response(null, { status: 204 }),
      "DELETE /api/reservations/up": new Response(null, { status: 204 }),
    });
    renderWithProviders(<ReservationsPage />, { me: me() });
    // the list's own table: every row picked; an ended one is not cancelled
    const table = within(await screen.findByRole("table", { name: "我的预约" }));
    fireEvent.click(table.getByLabelText("全选"));
    fireEvent.click(screen.getByRole("button", { name: "取消所选（2）" }));
    const confirm = within(screen.getByRole("dialog"));
    expect(confirm.getByText(/1 份进行中的会提前释放/)).toBeTruthy();
    fireEvent.click(confirm.getByRole("button", { name: "取消 2 份预约" }));
    await waitFor(() => expect(calls.filter((c) => c.method === "DELETE").map((c) => c.url.pathname).sort()).toEqual(["/api/reservations/act", "/api/reservations/up"]));
    expect(await screen.findByText(/取消预约：成功 2，失败 0/)).toBeTruthy();
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
    const menu = async (state: string) => {
      const row = (await screen.findByText(state)).closest("tr") as HTMLElement;
      fireEvent.keyDown(within(row).getByRole("button", { name: /操作$/ }), { key: "Enter" });
      return within(await screen.findByRole("menu"));
    };
    fireEvent.click((await menu("进行中")).getByRole("menuitem", { name: "修改" }));
    const dialog = within(screen.getByRole("dialog"));
    // it has started: its start stays; its end is what it is, moved by hand
    expect(input(dialog, "开始").disabled).toBe(true);
    expect(input(dialog, "结束").value).toBe("2026-11-12T11:00");
    fireEvent.change(input(dialog, "结束"), { target: { value: "2026-11-12T11:10" } });
    fireEvent.click(dialog.getByRole("button", { name: "保存" }));
    await waitFor(() => expect(calls.find((c) => c.method === "PATCH")?.body).toBe('{"end":"2026-11-12T03:10:00.000Z"}'));

    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    fireEvent.click((await menu("即将开始")).getByRole("menuitem", { name: "取消" }));
    fireEvent.click(screen.getByRole("button", { name: "取消预约" }));
    await waitFor(() => expect(calls.some((c) => c.method === "DELETE" && c.url.pathname === "/api/reservations/up")).toBe(true));
    fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" });
    expect((await menu("进行中")).getByRole("menuitem", { name: "提前释放" })).toBeTruthy();
  });

  it("anyone opens a reservation's details; another's I may not manage has no actions", async () => {
    at();
    stubFetch({
      "/api/reservations/rules": rules,
      "/api/nodes": { items: [node("g206")] },
      "/api/reservations": { items: [block({ id: "b1", user: "bob", mine: false, canManage: false, purpose: "跑基准" })] },
    });
    renderWithProviders(<ReservationsPage />, { me: me() });
    const row = await screen.findByRole("group", { name: "g206 的预约" });
    fireEvent.click(await within(row).findByRole("button", { name: /bob/ }));
    const dialog = within(await screen.findByRole("dialog"));
    expect(dialog.getByText(/跑基准/)).toBeTruthy();
    expect(dialog.getAllByText(/bob/).length).toBeGreaterThan(0);
    for (const name of [/修改/, /取消预约/, /提前释放/, /取消这个预约/]) expect(dialog.queryByRole("button", { name })).toBeNull();
  });

  it("another's reservation I may manage (a grant) has 修改… and 取消预约", async () => {
    at();
    stubFetch({
      "/api/reservations/rules": rules,
      "/api/nodes": { items: [node("g206")] },
      "/api/reservations": { items: [block({ id: "b2", user: "bob", mine: false, canManage: true })] },
    });
    renderWithProviders(<ReservationsPage />, { me: me() });
    const row = await screen.findByRole("group", { name: "g206 的预约" });
    fireEvent.click(await within(row).findByRole("button", { name: /bob/ }));
    const dialog = within(await screen.findByRole("dialog"));
    expect(dialog.getByRole("button", { name: "修改…" })).toBeTruthy();
    expect(dialog.getByRole("button", { name: "取消预约" })).toBeTruthy();
  });

  it("the purpose goes with the reservation; 为谁预约 only for one who may manage reservations", async () => {
    at();
    const calls = stubFetch({
      "/api/reservations/rules": rules,
      "/api/nodes": { items: [node("g206")] },
      "/api/reservations": { items: [] },
      "POST /api/reservations": block({ id: "new" }),
    });
    renderWithProviders(<ReservationsPage />, { me: me() });
    fireEvent.click(await screen.findByRole("button", { name: "预约节点" }));
    const dialog = within(screen.getByRole("dialog"));
    expect(dialog.queryByLabelText("为谁预约")).toBeNull();
    fireEvent.change(dialog.getByLabelText("用途"), { target: { value: "跑基准" } });
    fireEvent.click(dialog.getByRole("button", { name: "预约" }));
    await waitFor(() => expect(calls.some((c) => c.method === "POST")).toBe(true));
    expect(JSON.parse(calls.find((c) => c.method === "POST")!.body!).purpose).toBe("跑基准");
  });

  it("one granted 管理预约 may reserve for someone else", async () => {
    at();
    const calls = stubFetch({
      "/api/reservations/rules": rules,
      "/api/nodes": { items: [node("g206", { manageReservations: true })] },
      "/api/reservations": { items: [] },
      "POST /api/reservations": block({ id: "new", user: "bob", mine: false }),
    });
    const m = me();
    m.permissions = { setReservable: false, viewAudit: false, disconnectSessions: false, manageReservations: true };
    renderWithProviders(<ReservationsPage />, { me: m });
    fireEvent.click(await screen.findByRole("button", { name: "预约节点" }));
    const dialog = within(screen.getByRole("dialog"));
    fireEvent.change(dialog.getByLabelText("为谁预约"), { target: { value: "bob" } });
    fireEvent.click(dialog.getByRole("button", { name: "预约" }));
    await waitFor(() => expect(calls.some((c) => c.method === "POST")).toBe(true));
    expect(JSON.parse(calls.find((c) => c.method === "POST")!.body!).user).toBe("bob");
  });

  it("我的预约 shows each one's purpose", async () => {
    at();
    stubFetch({
      "/api/reservations/rules": rules,
      "/api/nodes": { items: [node("g206")] },
      "/api/reservations": { items: [block({ id: "m1", purpose: "训练 7B" })] },
    });
    renderWithProviders(<ReservationsPage />, { me: me() });
    const table = await screen.findByRole("table");
    expect(await within(table).findByText("训练 7B")).toBeTruthy();
  });

  // the page is not laid out in jsdom: these say the sizes a browser would
  const pointerEvents = () => {
    if (!("PointerEvent" in window)) {
      class PointerEvent extends MouseEvent {
        pointerId: number;
        pointerType: string;
        constructor(type: string, init: PointerEventInit = {}) {
          super(type, init);
          this.pointerId = init.pointerId ?? 0;
          this.pointerType = init.pointerType ?? "mouse";
        }
      }
      vi.stubGlobal("PointerEvent", PointerEvent);
    }
  };

  it("zoom: 3 days of a 15-day window make the track 5 screens wide, 14 days about one", async () => {
    at();
    stubFetch({ "/api/reservations/rules": rules, "/api/nodes": { items: [node("g206")] }, "/api/reservations": { items: [] } });
    const was = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientWidth");
    Object.defineProperty(HTMLElement.prototype, "clientWidth", {
      configurable: true,
      get(this: HTMLElement) {
        return this.hasAttribute("data-timeline") ? 600 : 0;
      },
    });
    try {
      renderWithProviders(<ReservationsPage />, { me: me() });
      const track = await screen.findByRole("group", { name: "g206 的预约" });
      await waitFor(() => expect(track.style.width).toBe("3000px"));
      fireEvent.click(screen.getByRole("radio", { name: "14 天" }));
      await waitFor(() => expect(track.style.width).toBe("643px"));
    } finally {
      if (was) Object.defineProperty(HTMLElement.prototype, "clientWidth", was);
    }
  });

  it("touch: a drag never selects (it scrolls); a tap opens the form at that machine and slot", async () => {
    at();
    stubFetch({ "/api/reservations/rules": rules, "/api/nodes": { items: [node("g206"), node("g207")] }, "/api/reservations": { items: [] } });
    renderWithProviders(<ReservationsPage />, { me: me() });
    const tracks = await Promise.all(["g206", "g207"].map((n) => screen.findByRole("group", { name: `${n} 的预约` })));
    tracks.forEach((t, i) => (t.getBoundingClientRect = () => ({ left: 0, width: 5000, top: i * 50, height: 50, right: 5000, bottom: (i + 1) * 50, x: 0, y: i * 50, toJSON: () => ({}) })));
    pointerEvents();
    Object.defineProperty(document, "elementFromPoint", { value: (_x: number, y: number) => tracks[Math.min(1, Math.floor(y / 50))], configurable: true });
    try {
      // a swipe: the browser scrolls; no selection, no form
      fireEvent.pointerDown(tracks[1], { clientX: 166.7, clientY: 60, pointerId: 2, pointerType: "touch" });
      fireEvent.pointerMove(tracks[1], { clientX: 400, clientY: 62, pointerId: 2, pointerType: "touch" });
      expect(screen.queryAllByText(/12:00–/)).toHaveLength(0);
      fireEvent.pointerUp(tracks[1], { clientX: 400, clientY: 62, pointerId: 2, pointerType: "touch" });
      expect(screen.queryByRole("dialog")).toBeNull();
      // a tap: the form, for g207, from the tapped slot
      fireEvent.pointerDown(tracks[1], { clientX: 166.7, clientY: 60, pointerId: 3, pointerType: "touch" });
      fireEvent.pointerUp(tracks[1], { clientX: 168, clientY: 61, pointerId: 3, pointerType: "touch" });
      const dialog = within(await screen.findByRole("dialog"));
      expect((dialog.getByLabelText("g207") as HTMLInputElement).checked).toBe(true);
      expect((dialog.getByLabelText("g206") as HTMLInputElement).checked).toBe(false);
      expect(input(dialog, "开始").value).toBe("2026-11-12T12:00");
    } finally {
      Reflect.deleteProperty(document, "elementFromPoint");
    }
  });

  it("a purpose-only change to one under way sends the purpose alone (its end stays)", async () => {
    at();
    const calls = stubFetch({
      "/api/reservations/rules": rules,
      "/api/nodes": { items: [node("g206")] },
      "/api/reservations": { items: [block({ id: "act", start: "2026-11-12T01:00:00Z", end: "2026-11-12T03:00:00Z", phase: "Active", canManage: true, purpose: "旧" })] },
      "PATCH /api/reservations/act": block({ id: "act" }),
    });
    renderWithProviders(<ReservationsPage />, { me: me() });
    const row = (await screen.findByText("进行中")).closest("tr") as HTMLElement;
    fireEvent.keyDown(within(row).getByRole("button", { name: /操作$/ }), { key: "Enter" });
    fireEvent.click(within(await screen.findByRole("menu")).getByRole("menuitem", { name: "修改" }));
    const dialog = within(screen.getByRole("dialog"));
    fireEvent.change(dialog.getByLabelText("用途"), { target: { value: "新" } });
    fireEvent.click(dialog.getByRole("button", { name: "保存" }));
    await waitFor(() => expect(calls.find((c) => c.method === "PATCH")?.body).toBe('{"purpose":"新"}'));
  });

  it("the timeline's reservations are fetched for the whole window it shows", async () => {
    at();
    const calls = stubFetch({
      "/api/reservations/rules": { ...rules, maxAdvanceSeconds: 20 * 86400 },
      "/api/nodes": { items: [node("g206")] },
      "/api/reservations": { items: [] },
    });
    renderWithProviders(<ReservationsPage />, { me: me() });
    await screen.findByRole("group", { name: "g206 的预约" });
    // 11/12 00:00 Shanghai + 21 days (now 10:10 + 20 days reaches into the 21st)
    await waitFor(() =>
      expect(calls.some((c) => c.url.pathname === "/api/reservations" && !c.url.searchParams.get("mine") && c.url.searchParams.get("to") === "2026-12-02T16:00:00.000Z")).toBe(true),
    );
  });

  it("a pen is like a finger: a drag never selects, a tap opens the form there", async () => {
    at();
    stubFetch({ "/api/reservations/rules": rules, "/api/nodes": { items: [node("g206")] }, "/api/reservations": { items: [] } });
    renderWithProviders(<ReservationsPage />, { me: me() });
    const track = await screen.findByRole("group", { name: "g206 的预约" });
    track.getBoundingClientRect = () => ({ left: 0, width: 5000, top: 0, height: 50, right: 5000, bottom: 50, x: 0, y: 0, toJSON: () => ({}) });
    pointerEvents();
    Object.defineProperty(document, "elementFromPoint", { value: () => track, configurable: true });
    try {
      fireEvent.pointerDown(track, { clientX: 166.7, clientY: 10, pointerId: 4, pointerType: "pen" });
      fireEvent.pointerMove(track, { clientX: 300, clientY: 10, pointerId: 4, pointerType: "pen" });
      fireEvent.pointerUp(track, { clientX: 300, clientY: 10, pointerId: 4, pointerType: "pen" });
      expect(screen.queryByRole("dialog")).toBeNull();
      fireEvent.pointerDown(track, { clientX: 166.7, clientY: 10, pointerId: 5, pointerType: "pen" });
      fireEvent.pointerUp(track, { clientX: 167, clientY: 10, pointerId: 5, pointerType: "pen" });
      expect(input(within(await screen.findByRole("dialog")), "开始").value).toBe("2026-11-12T12:00");
    } finally {
      Reflect.deleteProperty(document, "elementFromPoint");
    }
  });

  it("a short reservation is still wide enough to tap", async () => {
    at();
    stubFetch({
      "/api/reservations/rules": rules,
      "/api/nodes": { items: [node("g206")] },
      "/api/reservations": { items: [block({ id: "s1", user: "bob", mine: false, start: "2026-11-12T04:00:00Z", end: "2026-11-12T04:10:00Z" })] },
    });
    renderWithProviders(<ReservationsPage />, { me: me() });
    const b = await within(await screen.findByRole("group", { name: "g206 的预约" })).findByRole("button", { name: /bob/ });
    expect(b.style.minWidth).toBe("16px");
  });
});
