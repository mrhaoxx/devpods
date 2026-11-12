import { describe, expect, it } from "vitest";
import { groupRows } from "./groups";
import { toRow } from "./columns";
import type { DevPod } from "../../data/api";

const dp = (name: string, group?: string, index?: number, running = true): DevPod => ({
  metadata: {
    name,
    labels: group ? { "devpod.io/group": group } : undefined,
    annotations: group ? { "devpod.io/group-index": String(index), "devpod.io/group-size": "3" } : undefined,
  },
  spec: { owner: "alice", running, pod: { spec: { containers: [{ name: "dev", image: "x", resources: { limits: { "nvidia.com/gpu": "1" } } }] } } },
  status: { phase: running ? "Running" : "Stopped" },
});
// listed out of order on purpose
const rows = [dp("alice-t-2", "alice-t", 2), dp("alice-plain"), dp("alice-t-0", "alice-t", 0, false), dp("alice-t-1", "alice-t", 1)].map((d) => toRow(d));

describe("groupRows", () => {
  it("a group of several is one row, where its first member was", () => {
    const out = groupRows(rows, new Set());
    expect(out.map((r) => r.name)).toEqual(["alice-t", "alice-plain"]);
    expect(out[0].group?.members.map((m) => m.name)).toEqual(["alice-t-0", "alice-t-1", "alice-t-2"]);
    expect(out[0].gpu).toBe(3);
  });

  it("open, its members follow it in their order", () => {
    expect(groupRows(rows, new Set(["alice-t"])).map((r) => r.name)).toEqual(["alice-t", "alice-t-0", "alice-t-1", "alice-t-2", "alice-plain"]);
  });

  it("a group down to one member is a plain row", () => {
    const one = [dp("alice-t-1", "alice-t", 1), dp("alice-plain")].map((d) => toRow(d));
    const out = groupRows(one, new Set());
    expect(out.map((r) => r.name)).toEqual(["alice-t-1", "alice-plain"]);
    expect(out[0].group).toBeUndefined();
  });
});
