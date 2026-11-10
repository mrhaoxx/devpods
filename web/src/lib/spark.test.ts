import { describe, expect, it } from "vitest";
import { sparkSeries } from "./spark";

const now = 1_000_000; // ms
describe("sparkSeries", () => {
  it("fills buckets from the history", () => {
    // 4 buckets over the last 60 s: [940,955) [955,970) [970,985) [985,1000) s
    const s = sparkSeries({ ts: [940, 955, 970], v: [1, null, 3] }, [], now, 60_000, 4);
    expect(s).toEqual([1, null, 3, null]);
  });
  it("live samples land in their buckets and average with history", () => {
    const s = sparkSeries({ ts: [985], v: [2] }, [{ ts: 990_000, v: 4 }, { ts: 999_000, v: 6 }], now, 60_000, 4);
    expect(s[3]).toBe(4);
  });
  it("drops points outside the window", () => {
    const s = sparkSeries({ ts: [100], v: [9] }, [{ ts: now + 5_000, v: 9 }], now, 60_000, 4);
    expect(s).toEqual([null, null, null, null]);
  });
  it("no data is all null", () => {
    expect(sparkSeries(undefined, [], now, 60_000, 3)).toEqual([null, null, null]);
  });
});
