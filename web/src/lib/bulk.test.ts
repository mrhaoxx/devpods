import { describe, expect, it } from "vitest";
import { runBulk } from "./bulk";

describe("runBulk", () => {
  it("runs at most 4 at a time", async () => {
    let active = 0;
    let peak = 0;
    const res = await runBulk([1, 2, 3, 4, 5, 6, 7, 8, 9], async () => {
      active++;
      peak = Math.max(peak, active);
      await new Promise((r) => setTimeout(r, 5));
      active--;
    });
    expect(peak).toBe(4);
    expect(res).toHaveLength(9);
    expect(res.every((r) => r.ok)).toBe(true);
  });

  it("reports per-item failures without aborting the rest", async () => {
    const res = await runBulk(["a", "b", "c"], async (x) => {
      if (x === "b") throw new Error("quota exceeded");
    });
    expect(res.map((r) => [r.item, r.ok, r.error])).toEqual([
      ["a", true, undefined],
      ["b", false, "quota exceeded"],
      ["c", true, undefined],
    ]);
  });
});
