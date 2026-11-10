import { describe, expect, it } from "vitest";
import { chartOption, fmtAxisTime, latest, nextHidden, type ChartTheme } from "./chartData";

const th: ChartTheme = { ink: "#111", muted: "#666", faint: "#999", line: "#ddd", lineStrong: "#bbb", surface: "#fff", color: (n) => (n === "--chart-1" ? "#2a78d6" : "#eb6834") };
type S = { name: string; data: unknown[]; stack?: string; areaStyle?: unknown; markLine?: { data: unknown[]; label: { formatter: string } }; lineStyle: { color: string; width: number } };

describe("chart option", () => {
  const base = {
    ts: [1000, 2000],
    series: [
      { label: "a", values: [1, null], color: "--chart-1", fill: true },
      { label: "b", values: [2, 3], color: "--chart-2" },
    ],
    yFormat: (v: number) => `${v}c`,
  };
  it("pairs values with timestamps, keeps gaps, colors by slot", () => {
    const s = chartOption(base, th).series as S[];
    expect(s.map((x) => x.name)).toEqual(["a", "b"]);
    expect(s[0].data).toEqual([[1000, 1], [2000, null]]);
    expect(s[0].lineStyle).toMatchObject({ color: "#2a78d6", width: 1.5 });
    expect(s[0].areaStyle).toBeTruthy();
    expect(s[1].areaStyle).toBeUndefined();
  });
  it("draws the limit as one labelled reference line", () => {
    const s = chartOption({ ...base, limit: 8 }, th).series as S[];
    expect(s[0].markLine).toMatchObject({ data: [{ yAxis: 8 }], label: { formatter: "限额 8c" } });
    expect(s[1].markLine).toBeUndefined();
  });
  it("stacks when asked and drops hidden series", () => {
    const s = chartOption({ ...base, stacked: true, hidden: new Set(["a"]) }, th).series as S[];
    expect(s.map((x) => x.name)).toEqual(["b"]);
    expect(s[0].stack).toBe("all");
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
