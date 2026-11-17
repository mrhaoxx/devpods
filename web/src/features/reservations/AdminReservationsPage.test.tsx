import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, within } from "@testing-library/react";
import { renderWithProviders, stubFetch } from "../../test/render";
import { AdminReservationsPage } from "./AdminReservationsPage";

const now = Date.now();
const iso = (h: number) => new Date(now + h * 3600e3).toISOString();
const rules = { granularitySeconds: 600, maxDurationSeconds: 0, maxAdvanceSeconds: 14 * 86400, clearNoticeSeconds: 600, timezone: "Asia/Shanghai", now: new Date(now).toISOString() };
const res = (id: string, user: string, node: string, phase: string, from: number, to: number) => ({ id, node, user, start: iso(from), end: iso(to), phase, mine: false });

describe("预约管理", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("lists every user's reservations, filters them, and cancels one", async () => {
    const calls = stubFetch({
      "GET /api/reservations/rules": rules,
      "GET /api/reservations": { items: [res("r1", "alice", "g1", "Active", -1, 2), res("r2", "bob", "g2", "Upcoming", 5, 9), res("r3", "carol", "g1", "Cancelled", 3, 4)] },
      "DELETE /api/reservations/r2": new Response(null, { status: 204 }),
    });
    renderWithProviders(<AdminReservationsPage />);
    await screen.findByText("bob");
    expect(screen.getByText("alice")).toBeTruthy();
    // only the upcoming ones
    fireEvent.click(screen.getByRole("button", { name: /即将开始/ }));
    expect(screen.queryByText("alice")).toBeNull();
    // no longest: said so, not "0"
    expect(screen.getByText(/一次最长 不限/)).toBeTruthy();
    // a row's ⋯ offers what a right click does
    const row = screen.getByText("bob").closest("tr") as HTMLElement;
    fireEvent.keyDown(within(row).getByRole("button", { name: "bob g2 操作" }), { key: "Enter" });
    fireEvent.click(within(await screen.findByRole("menu")).getByRole("menuitem", { name: "取消" }));
    fireEvent.click(await screen.findByRole("button", { name: "取消预约" }));
    await vi.waitFor(() => expect(calls.some((c) => c.method === "DELETE" && c.url.pathname === "/api/reservations/r2")).toBe(true));
  });

  it("cancels several at once: the picked ones still on, after a confirm; the outcome is said", async () => {
    const calls = stubFetch({
      "GET /api/reservations/rules": rules,
      "GET /api/reservations": { items: [res("r1", "alice", "g1", "Active", -1, 2), res("r2", "bob", "g2", "Upcoming", 5, 9), res("r3", "carol", "g1", "Cancelled", 3, 4)] },
      "DELETE /api/reservations/r1": new Response(null, { status: 204 }),
      "DELETE /api/reservations/r2": new Response(JSON.stringify({ code: "CONFLICT", message: "changed meanwhile" }), { status: 409 }),
    });
    renderWithProviders(<AdminReservationsPage />);
    await screen.findByText("bob");
    fireEvent.click(screen.getByLabelText("全选"));
    fireEvent.click(screen.getByRole("button", { name: "取消所选（2）" })); // carol's was cancelled already
    const confirm = within(screen.getByRole("dialog"));
    expect(confirm.getByText(/1 份正在进行/)).toBeTruthy();
    fireEvent.click(confirm.getByRole("button", { name: "取消 2 份预约" }));
    expect(await screen.findByText(/取消预约：成功 1，失败 1/)).toBeTruthy();
    expect(screen.getByText(/bob g2.*changed meanwhile/)).toBeTruthy();
    expect(calls.filter((c) => c.method === "DELETE").map((c) => c.url.pathname).sort()).toEqual(["/api/reservations/r1", "/api/reservations/r2"]);
  });
});
