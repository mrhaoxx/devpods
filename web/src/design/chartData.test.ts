import { describe, expect, it } from "vitest";
import { fmtAxisTime, latest, nextHidden, plotData } from "./chartData";


describe("chart data", () => {
  const base = {
    ts: [1000, 2000],
    series: [
      { label: "a", values: [1, null], color: "--chart-1", fill: true },
      { label: "b", values: [2, 3], color: "--chart-2" },
    ],
    yFormat: (v: number) => `${v}c`,
  };
  it("plots seconds on x, keeps gaps, and leaves room above the highest value", () => {
    const p = plotData(base);
    expect(p.x).toEqual([1, 2]);
    expect(p.series.map((s) => [s.label, s.fill, s.drawn])).toEqual([
      ["a", true, [1, null]],
      ["b", false, [2, 3]],
    ]);
    expect(p.yMax).toBeCloseTo(3 * 1.06);
  });
  it("the limit is always inside the y range", () => {
    expect(plotData({ ...base, limit: 8 }).yMax).toBeCloseTo(8 * 1.06);
  });
  it("stacks cumulatively, top layer first, and drops hidden series", () => {
    const p = plotData({ ...base, stacked: true });
    expect(p.series.map((s) => [s.label, s.drawn, s.raw])).toEqual([
      ["b", [3, 3], [2, 3]],
      ["a", [1, null], [1, null]],
    ]);
    // a hidden series stays, drawn as nothing (so hiding does not rebuild the chart), and is out of the stack
    const h = plotData({ ...base, stacked: true, hidden: new Set(["a"]) });
    expect(h.series.map((s) => [s.label, s.hidden, s.drawn])).toEqual([
      ["b", false, [2, 3]],
      ["a", true, [null, null]],
    ]);
  });
  it("latest skips trailing gaps; the axis shows dates only for long ranges", () => {
    expect(latest([1, 2, null])).toBe(2);
    expect(latest([null])).toBeNull();
    const t = new Date(2026, 10, 11, 9, 5).getTime();
    expect(fmtAxisTime(t, 3600e3)).toBe("09:05");
    expect(fmtAxisTime(t, 7 * 86400e3)).toBe("11-11 09:05");
  });
  it("a legend click isolates a series, a second click restores, ctrl-click toggles", () => {
    const all = ["a", "b", "c"];
    const one = nextHidden(new Set(), all, "a", false);
    expect([...one].sort()).toEqual(["b", "c"]);
    expect(nextHidden(one, all, "a", false).size).toBe(0);
    expect([...nextHidden(one, all, "b", false)].sort()).toEqual(["a", "c"]);
    expect([...nextHidden(new Set(), all, "b", true)]).toEqual(["b"]);
    expect(nextHidden(new Set(["a", "b"]), all, "c", true).size).toBe(0); // never hide everything
  });
});
