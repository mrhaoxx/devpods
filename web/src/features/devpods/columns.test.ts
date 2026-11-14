import { describe, expect, it } from "vitest";
import { toRow } from "./columns";
import type { DevPod } from "../../data/api";

const dp = (running: boolean, pin?: string): DevPod => ({
  metadata: { name: "alice-x" },
  spec: { owner: "alice", running, ...(pin ? { pod: { spec: { nodeSelector: { "kubernetes.io/hostname": pin } } } } : {}) },
});

describe("a row's node", () => {
  it("is where its Pod is; with none (hibernated), where it is pinned", () => {
    expect(toRow(dp(true, "g206"), { node: "g206", ip: "10.0.0.2" }).node).toBe("g206");
    expect(toRow(dp(false, "g206")).node).toBe("g206");
    expect(toRow(dp(false)).node).toBe("");
  });
});
