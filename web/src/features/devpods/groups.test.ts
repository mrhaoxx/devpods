import { describe, expect, it } from "vitest";
import { groupRows } from "./groups";
import { toRow, type Row } from "./columns";
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
  it("a group of several is one row, where its first member was, its members after it in their order", () => {
    const out = groupRows(rows, new Set());
    expect(out.map((r) => r.name)).toEqual(["alice-t", "alice-t-0", "alice-t-1", "alice-t-2", "alice-plain"]);
    expect(out[0].group?.members.map((m) => m.name)).toEqual(["alice-t-0", "alice-t-1", "alice-t-2"]);
    expect(out[0].gpu).toBe(3);
    expect(out[1].inGroup).toBe(true);
  });

  it("folded, its members are not shown", () => {
    expect(groupRows(rows, new Set(["alice-t"])).map((r) => r.name)).toEqual(["alice-t", "alice-plain"]);
  });

  it("sorted, a group goes where its own value puts it, its members with it and sorted among themselves", () => {
    // by GPUs, most first: the group (3) before alice-plain (1); inside it, alice-t-1 has the most
    const gpus: Record<string, number> = { "alice-t": 3, "alice-t-0": 1, "alice-t-1": 5, "alice-t-2": 2, "alice-plain": 4 };
    const desc = (a: Row, b: Row) => gpus[b.name] - gpus[a.name];
    expect(groupRows(rows, new Set(), desc).map((r) => r.name)).toEqual(["alice-plain", "alice-t", "alice-t-1", "alice-t-2", "alice-t-0"]);
    const asc = (a: Row, b: Row) => -desc(a, b);
    expect(groupRows(rows, new Set(), asc).map((r) => r.name)).toEqual(["alice-t", "alice-t-0", "alice-t-2", "alice-t-1", "alice-plain"]);
  });

  it("a group down to one member is a plain row", () => {
    const one = [dp("alice-t-1", "alice-t", 1), dp("alice-plain")].map((d) => toRow(d));
    const out = groupRows(one, new Set());
    expect(out.map((r) => r.name)).toEqual(["alice-t-1", "alice-plain"]);
    expect(out[0].group).toBeUndefined();
  });
});
