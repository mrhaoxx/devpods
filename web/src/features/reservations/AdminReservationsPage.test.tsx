import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, within } from "@testing-library/react";
import { renderWithProviders, stubFetch } from "../../test/render";
import { AdminReservationsPage } from "./AdminReservationsPage";

const now = Date.now();
const iso = (h: number) => new Date(now + h * 3600e3).toISOString();
const rules = { granularitySeconds: 1800, maxDurationSeconds: 86400, maxAdvanceSeconds: 14 * 86400, clearNoticeSeconds: 600, timezone: "Asia/Shanghai", now: new Date(now).toISOString() };
const res = (id: string, user: string, node: string, phase: string, from: number, to: number) => ({ id, segments: [id], node, user, start: iso(from), end: iso(to), phase, mine: false });

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
    const row = screen.getByText("bob").closest("tr") as HTMLElement;
    fireEvent.click(within(row).getByRole("button", { name: "取消" }));
    fireEvent.click(await screen.findByRole("button", { name: "取消预约" }));
    await vi.waitFor(() => expect(calls.some((c) => c.method === "DELETE" && c.url.pathname === "/api/reservations/r2")).toBe(true));
  });
});
