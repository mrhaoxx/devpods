import { describe, expect, it } from "vitest";
import { usage, groupUsage } from "./usage";
import type { DevPodLive } from "../../data/types";

const live = (o: Partial<DevPodLive>): DevPodLive => ({
  cpu: null,
  cpuLimit: 8,
  mem: null,
  memLimit: 1,
  gpu: [],
  netRx: null,
  netTx: null,
  netKind: "pod",
  rdmaRx: null,
  rdmaTx: null,
  rdmaKind: "",
  ioR: null,
  ioW: null,
  sessions: [],
  idle: false,
  ...o,
});
const card = (util: number) => ({ index: 0, uuid: "", util, memUsed: 0, memTotal: 0 }) as unknown as DevPodLive["gpu"][number];

describe("usage", () => {
  it("what each column sorts by", () => {
    const v = live({ cpu: 2.5, mem: 1024, gpu: [card(50), card(100)], netRx: 10, netTx: 5, rdmaRx: 1, rdmaTx: 2 });
    expect(usage(v, "cpu")).toBe(2.5);
    expect(usage(v, "mem")).toBe(1024);
    expect(usage(v, "net")).toBe(15);
    expect(usage(v, "rdma")).toBe(3);
    // average use, then the number of cards
    expect(usage(v, "gpu")).toBeGreaterThan(usage(live({ gpu: [card(75)] }), "gpu")!);
    expect(Math.floor(usage(v, "gpu")!)).toBe(75);
  });

  it("nothing known is nothing to sort by", () => {
    expect(usage(undefined, "cpu")).toBeUndefined();
    expect(usage(live({}), "cpu")).toBeUndefined();
    expect(usage(live({}), "gpu")).toBeUndefined();
    expect(usage(live({}), "net")).toBeUndefined();
  });

  it("a group: its members added up, GPUs averaged", () => {
    const a = live({ cpu: 1, gpu: [card(20)] });
    const b = live({ cpu: 3, gpu: [card(60)] });
    expect(groupUsage([a, b, undefined], "cpu")).toBe(4);
    expect(Math.floor(groupUsage([a, b], "gpu")!)).toBe(40);
    expect(groupUsage([undefined], "cpu")).toBeUndefined();
  });
});
