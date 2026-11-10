import { describe, expect, it } from "vitest";
import { stackSeries, toUplotData } from "./chartData";

describe("chart data", () => {
  it("stackSeries adds layers and keeps null gaps", () => {
    expect(
      stackSeries([
        [1, null, 3],
        [2, 2, null],
      ]),
    ).toEqual([
      [1, null, 3],
      [3, 2, null],
    ]);
  });
  it("toUplotData converts ms timestamps to seconds", () => {
    expect(toUplotData([1000, 2000], [[1, null]])).toEqual([
      [1, 2],
      [1, null],
    ]);
  });
});
