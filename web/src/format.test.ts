import { describe, expect, it } from "vitest";
import { fmtAge, fmtBytes, fmtDuration, failureRetryIn, sumLimits } from "./format";

describe("fmtBytes", () => {
  it("uses 1024-based units with one decimal", () => {
    expect(fmtBytes(0)).toBe("0");
    expect(fmtBytes(512)).toBe("512");
    expect(fmtBytes(1536)).toBe("1.5K");
    expect(fmtBytes(58.1 * 1024 ** 3)).toBe("58.1G");
    expect(fmtBytes(3 * 1024 ** 4)).toBe("3.0T");
  });
});

describe("fmtDuration", () => {
  it("shows the two largest units", () => {
    expect(fmtDuration(42_000)).toBe("42s");
    expect(fmtDuration(7_920_000)).toBe("2h12m");
    expect(fmtDuration(194_400_000)).toBe("2d6h");
    expect(fmtDuration(-5)).toBe("0s");
  });
  it("fmtAge measures from an ISO timestamp", () => {
    const now = new Date("2026-11-09T12:00:00Z");
    expect(fmtAge("2026-11-09T11:15:00Z", now)).toBe("45m");
  });
});

describe("failureRetryIn", () => {
  it("mirrors the controller backoff 10s·2^(n-1) capped at 300s", () => {
    const t = new Date("2026-11-09T12:00:00Z");
    expect(failureRetryIn(3, "2026-11-09T11:59:30Z", t)).toBe(10); // 40s backoff, 30s elapsed
    expect(failureRetryIn(1, "2026-11-09T11:59:55Z", t)).toBe(5);
    expect(failureRetryIn(10, "2026-11-09T11:59:00Z", t)).toBe(240);
    expect(failureRetryIn(1, "2026-11-09T11:00:00Z", t)).toBe(0);
  });
});

describe("sumLimits", () => {
  it("adds container limits as quantities", () => {
    const spec = {
      containers: [
        { resources: { limits: { cpu: "4", memory: "8Gi", "nvidia.com/gpu": "2" } } },
        { resources: { limits: { cpu: "500m", memory: "512Mi" } } },
      ],
    };
    expect(sumLimits(spec)).toEqual({ cpu: 4.5, memory: 8.5 * 1024 ** 3, gpu: 2 });
    expect(sumLimits(undefined)).toEqual({ cpu: 0, memory: 0, gpu: 0 });
  });
});
