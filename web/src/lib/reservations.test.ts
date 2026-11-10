import { describe, expect, it } from "vitest";
import { dayStart, fmtDay, fmtSpan, fmtTime, fromInput, overlapping, periodError, selection, slotStart, toInput } from "./reservations";
import type { Reservation, ReservationRules } from "../data/api";

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
  it("finds the block a period runs into; touching ends do not overlap", () => {
    const blocks = [
      { id: "x", node: "n1", start: "2026-11-12T03:00:00Z", end: "2026-11-12T04:00:00Z", phase: "Upcoming" },
      { id: "c", node: "n1", start: "2026-11-12T05:00:00Z", end: "2026-11-12T06:00:00Z", phase: "Cancelled" },
    ] as Reservation[];
    expect(overlapping(blocks, "n1", at("2026-11-12T03:30:00Z"), at("2026-11-12T04:30:00Z"))?.id).toBe("x");
    expect(overlapping(blocks, "n1", at("2026-11-12T04:00:00Z"), at("2026-11-12T05:30:00Z"))).toBeUndefined();
    expect(overlapping(blocks, "n2", at("2026-11-12T03:30:00Z"), at("2026-11-12T04:30:00Z"))).toBeUndefined();
    expect(overlapping(blocks, "n1", at("2026-11-12T03:30:00Z"), at("2026-11-12T04:30:00Z"), "x")).toBeUndefined();
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
