import { describe, expect, it } from "vitest";
import { quotaErrors } from "./quota";

const usage = (cpu: string, memory = "0", devpods = 1) => ({ devpods, running: 1, compute: { cpu, memory }, storage: "0" });

describe("quotaErrors", () => {
  it("flags cpu over the remaining quota", () => {
    const e = quotaErrors({ cpu: "4", memory: "1Gi", gpu: "" }, { compute: { cpu: "8" } }, usage("6"));
    expect(e.cpu).toMatch(/超出配额/);
    expect(e.cpu).toContain("2");
    expect(e.memory).toBeUndefined();
  });
  it("blank quota means unlimited", () => {
    expect(quotaErrors({ cpu: "400", memory: "4Ti", gpu: "8" }, {}, usage("6"))).toEqual({});
  });
  it("an edit does not count its own current usage", () => {
    const own = { cpu: 4, memory: 0, gpu: 0, running: true };
    expect(quotaErrors({ cpu: "4", memory: "", gpu: "" }, { compute: { cpu: "8" } }, usage("8"), own)).toEqual({});
    expect(quotaErrors({ cpu: "5", memory: "", gpu: "" }, { compute: { cpu: "8" } }, usage("8"), own).cpu).toBeTruthy();
    // a hibernated DevPod takes quota only when it starts
    expect(quotaErrors({ cpu: "16", memory: "", gpu: "" }, { compute: { cpu: "8" } }, usage("8"), { ...own, running: false })).toEqual({});
  });
  it("count error when maxDevPods reached", () => {
    expect(quotaErrors({ cpu: "1", memory: "", gpu: "" }, { maxDevPods: 2 }, usage("0", "0", 2)).count).toMatch(/上限/);
    expect(quotaErrors({ cpu: "1", memory: "", gpu: "" }, { maxDevPods: 2 }, usage("0", "0", 2), { cpu: 0, memory: 0, gpu: 0, running: true }).count).toBeUndefined();
  });
});
