import { describe, expect, it } from "vitest";
import { amountOf, bookedAt, capOf, cpuShare, dayStart, fmtAmount, fmtDay, fmtSpan, fmtTime, freeIn, fromInput, fromNow, isWhole, periodError, presets, selection, slotStart, stackBand, toInput } from "./reservations";
import type { NodeChoice, Reservation, ReservationRules } from "../data/api";

const G = 30 * 60_000;
const at = (s: string) => Date.parse(s);
const rules: ReservationRules = { granularitySeconds: 1800, maxDurationSeconds: 86400, maxAdvanceSeconds: 14 * 86400, clearNoticeSeconds: 1800, timezone: "Asia/Shanghai", now: "" };
const now = at("2026-11-12T02:10:00Z"); // 10:10 in Shanghai

describe("reservation time math", () => {
  it("snaps to slots and days in Shanghai time", () => {
    expect(slotStart(now, G)).toBe(at("2026-11-12T02:00:00Z"));
    expect(dayStart(now)).toBe(at("2026-11-11T16:00:00Z")); // local midnight
    expect(fmtTime(now)).toBe("11-12 10:10");
    expect(fmtDay(now)).toBe("11/12 四");
    expect(toInput(now)).toBe("2026-11-12T10:10");
    expect(fromInput("2026-11-12T10:30")).toBe(at("2026-11-12T02:30:00Z"));
  });
  it("a drag covers whole slots, in either direction, at least one", () => {
    const a = at("2026-11-12T03:05:00Z");
    const b = at("2026-11-12T04:20:00Z");
    expect(selection(a, b, G)).toEqual([at("2026-11-12T03:00:00Z"), at("2026-11-12T04:30:00Z")]);
    expect(selection(b, a, G)).toEqual(selection(a, b, G));
    expect(selection(a, a, G)).toEqual([at("2026-11-12T03:00:00Z"), at("2026-11-12T03:30:00Z")]);
  });
  it("a drag that starts in the past starts at the slot now is in; one wholly past stays as it is", () => {
    const first = slotStart(now, G); // 10:00
    expect(fromNow([at("2026-11-12T01:00:00Z"), at("2026-11-12T04:00:00Z")], first)).toEqual([first, at("2026-11-12T04:00:00Z")]);
    expect(fromNow([at("2026-11-12T03:00:00Z"), at("2026-11-12T04:00:00Z")], first)).toEqual([at("2026-11-12T03:00:00Z"), at("2026-11-12T04:00:00Z")]);
    expect(fromNow([at("2026-11-12T00:00:00Z"), first], first)).toEqual([at("2026-11-12T00:00:00Z"), first]);
  });
  it("checks a period against the rules like the server", () => {
    const slot = at("2026-11-12T02:00:00Z");
    expect(periodError(slot, slot + 2 * G, rules, now)).toBeNull(); // the current slot is allowed
    expect(periodError(slot + 5 * 60_000, slot + 2 * G, rules, now)).toMatch(/对齐/);
    expect(periodError(slot + G, slot + G, rules, now)).toMatch(/晚于/);
    expect(periodError(slot, slot + 49 * G, rules, now)).toMatch(/最长 1 天/);
    expect(periodError(slot - G, slot + G, rules, now)).toMatch(/已经过去/);
    expect(periodError(slot + 15 * 86_400_000, slot + 15 * 86_400_000 + G, rules, now)).toMatch(/最多提前 14 天/);
    expect(fmtSpan(90 * 60_000)).toBe("1 小时 30 分钟");
  });
});

describe("reservation amounts", () => {
  const cap = { gpu: 8, cpu: 192 };
  const r = (id: string, start: string, end: string, o: Partial<Reservation> = {}): Reservation => ({ id, node: "g1", user: "u", start: `2026-11-12T${start}:00Z`, end: `2026-11-12T${end}:00Z`, phase: "Upcoming", mine: false, ...o });
  const t = (hm: string) => at(`2026-11-12T${hm}:00Z`);
  const whole = r("w", "02:00", "03:00");
  const half = r("h", "03:00", "04:00", { gpu: 4, cpu: 96 });

  it("a reservation without amounts is the whole machine", () => {
    expect(isWhole(whole)).toBe(true);
    expect(isWhole(half)).toBe(false);
    expect(amountOf(whole, cap)).toEqual(cap);
    expect(amountOf(half, cap)).toEqual({ gpu: 4, cpu: 96 });
    expect(amountOf({ cpu: 16 }, cap)).toEqual({ gpu: 0, cpu: 16 }); // one amount said: none of the other
    expect(capOf({ gpuTotal: 8, cpu: { allocatable: 64, allocated: 0 } } as NodeChoice)).toEqual({ gpu: 8, cpu: 64 });
    expect(cpuShare(4, cap)).toBe(96);
    expect(cpuShare(3, { gpu: 8, cpu: 100 })).toBe(37); // whole cores, rounded down
  });

  it("what is booked at a time, and what is free over a window", () => {
    expect(bookedAt([whole, half], t("02:30"), cap)).toEqual({ gpu: 8, cpu: 192 });
    expect(bookedAt([whole, half], t("03:30"), cap)).toEqual({ gpu: 4, cpu: 96 });
    expect(freeIn([whole, half], t("03:00"), t("04:00"), cap)).toEqual({ gpu: 4, cpu: 96 });
    expect(freeIn([whole, half], t("02:30"), t("03:30"), cap)).toEqual({ gpu: 0, cpu: 0 }); // a whole one overlaps
    expect(freeIn([whole, half], t("02:30"), t("03:30"), cap, "w")).toEqual({ gpu: 4, cpu: 96 }); // the one being edited left out
  });

  it("stacks a band from the bottom, first fit; without room, over", () => {
    const a = r("a", "02:00", "04:00", { gpu: 4, cpu: 96 });
    const b = r("b", "02:00", "03:00", { gpu: 2, cpu: 48 });
    const c = r("c", "02:30", "04:00", { gpu: 2, cpu: 48 });
    const gpu = (x: Reservation) => amountOf(x, cap).gpu;
    const st = stackBand([c, b, a], 8, gpu);
    expect(st.get("a")).toEqual({ y0: 0, y1: 4, over: false });
    expect(st.get("b")).toEqual({ y0: 4, y1: 6, over: false });
    expect(st.get("c")).toEqual({ y0: 6, y1: 8, over: false });
    // 2 at the bottom all along, 2 above it till 03:00, 2 above that all along: at 03:00-04:00 the free 4 cards are not together
    const f = stackBand([r("p", "02:00", "04:00", { gpu: 2 }), r("q", "02:00", "03:00", { gpu: 2 }), r("s", "02:00", "04:00", { gpu: 2 }), r("u", "03:00", "04:00", { gpu: 4 })], 8, gpu);
    expect(f.get("u")).toEqual({ y0: 4, y1: 8, over: true });
  });

  it("presets: the whole machine, then halves down to one card; a CPU machine by cores", () => {
    const g = presets(cap);
    expect(g.map((p) => p.label)).toEqual(["整台", "4 卡", "2 卡", "1 卡"]);
    expect(g.map((p) => [p.gpu, p.cpu])).toEqual([[undefined, undefined], [4, 96], [2, 48], [1, 24]]);
    const c = presets({ gpu: 0, cpu: 64 });
    expect(c.map((p) => p.label)).toEqual(["整台", "32 核", "16 核"]);
    expect(c.map((p) => p.cpu)).toEqual([undefined, 32, 16]);
  });

  it("says an amount", () => {
    expect(fmtAmount({})).toBe("整台");
    expect(fmtAmount({ gpu: 4, cpu: 96 })).toBe("4 卡 · 96 核");
    expect(fmtAmount({ gpu: 0, cpu: 16 })).toBe("16 核");
    expect(fmtAmount({ cpu: 1.5 })).toBe("1.5 核");
  });
});
