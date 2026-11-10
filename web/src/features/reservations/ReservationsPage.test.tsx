import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { ReservationsPage } from "./ReservationsPage";
import { renderWithProviders, stubFetch, stubMe } from "../../test/render";

const now = "2026-11-12T02:10:00Z"; // 10:10 in Shanghai
const rules = { granularitySeconds: 1800, maxDurationSeconds: 86400, maxAdvanceSeconds: 1209600, clearNoticeSeconds: 1800, timezone: "Asia/Shanghai", now };
const node = (name: string, reservable = true) => ({ name, ready: true, cpu: { allocatable: 64, allocated: 8 }, mem: { allocatable: 1, allocated: 0 }, gpuModel: "A100", gpuTotal: 8, gpuUsed: 0, reservable, pending: 0, held: null, next: null, selectable: true });
const block = (o: object) => ({ id: "r1", segments: ["r1"], node: "g206", user: "alice", start: "2026-11-12T04:00:00Z", end: "2026-11-12T06:00:00Z", phase: "Upcoming", mine: true, ...o });

describe("ReservationsPage", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });
  const at = () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date(now));
  };

  it("lists only reservable nodes; the form checks the rules, previews, then reserves", async () => {
    at();
    const calls = stubFetch({
      "/api/reservations/rules": rules,
      "/api/nodes": { items: [node("g206"), node("c101", false)] },
      "/api/reservations": { items: [block({ id: "b1", user: "bob", mine: false })] },
      "POST /api/reservations": (u: URL) => (u.searchParams.get("dryRun") ? { willHibernate: 2 } : block({ id: "new" })),
    });
    renderWithProviders(<ReservationsPage />, { me: stubMe({ features: { ...stubMe().features, reservations: true } }) });
    expect(await screen.findByRole("group", { name: "g206 的预约" })).toBeTruthy();
    expect(screen.queryByRole("group", { name: "c101 的预约" })).toBeNull();
    // someone else's block shows the user name and is not a button
    expect(await screen.findByText(/bob · 12:00–14:00/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: /bob/ })).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "预约节点" }));
    const dialog = within(screen.getByRole("dialog"));
    // default: the next slot for two slots (10:30–11:30)
    expect((dialog.getByLabelText("开始") as HTMLInputElement).value).toBe("2026-11-12T10:30");
    fireEvent.change(dialog.getByLabelText("结束"), { target: { value: "2026-11-12T12:30" } });
    expect(dialog.getByRole("alert").textContent).toMatch(/和 bob 的预约/); // runs into bob's 12:00
    expect((dialog.getByRole("button", { name: "下一步" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(dialog.getByLabelText("结束"), { target: { value: "2026-11-12T11:45" } });
    expect(dialog.getByRole("alert").textContent).toMatch(/对齐/);
    fireEvent.change(dialog.getByLabelText("结束"), { target: { value: "2026-11-12T12:00" } });
    fireEvent.click(dialog.getByRole("button", { name: "下一步" }));
    expect(await dialog.findByText(/2 个 DevPod 会被自动休眠/)).toBeTruthy();
    expect(calls.filter((c) => c.method === "POST").length).toBe(1); // only the preview so far
    fireEvent.click(dialog.getByRole("button", { name: "确认预约" }));
    await waitFor(() => {
      const real = calls.find((c) => c.method === "POST" && !c.url.searchParams.get("dryRun"));
      expect(real?.body).toBe('{"node":"g206","start":"2026-11-12T02:30:00.000Z","end":"2026-11-12T04:00:00.000Z"}');
    });
  });

  it("my reservations: an active one extends or releases early, an upcoming one cancels", async () => {
    at();
    const calls = stubFetch({
      "/api/reservations/rules": rules,
      "/api/nodes": { items: [node("g206")] },
      "/api/reservations": { items: [block({ id: "act", start: "2026-11-12T01:00:00Z", end: "2026-11-12T03:00:00Z", phase: "Active" }), block({ id: "up" })] },
      "PATCH /api/reservations/act": block({ id: "act" }),
      "DELETE /api/reservations/up": new Response(null, { status: 204 }),
    });
    renderWithProviders(<ReservationsPage />, { me: stubMe({ features: { ...stubMe().features, reservations: true } }) });
    fireEvent.click(await screen.findByRole("button", { name: "延长" }));
    const dialog = within(screen.getByRole("dialog"));
    expect((dialog.getByLabelText("开始") as HTMLInputElement).disabled).toBe(true);
    expect((dialog.getByLabelText("结束") as HTMLInputElement).value).toBe("2026-11-12T11:30"); // one slot later
    fireEvent.click(dialog.getByRole("button", { name: "延长" }));
    await waitFor(() => expect(calls.find((c) => c.method === "PATCH")?.body).toBe('{"end":"2026-11-12T03:30:00.000Z"}'));

    fireEvent.click(await screen.findByRole("button", { name: "取消" }));
    fireEvent.click(screen.getByRole("button", { name: "取消预约" }));
    await waitFor(() => expect(calls.some((c) => c.method === "DELETE" && c.url.pathname === "/api/reservations/up")).toBe(true));
    expect(screen.getByRole("button", { name: "提前释放" })).toBeTruthy();
  });
});
