// What a DevPod uses now, as the list sorts by it: cores, bytes of
// memory, GPU use (the average, then how many cards), network and RDMA
// traffic (both ways added). undefined: nothing known (stopped, no
// telemetry) — such rows sort last either way.
import type { DevPodLive } from "../../data/types";

export type UsageKey = "cpu" | "mem" | "gpu" | "net" | "rdma";
export const usageKeys: readonly UsageKey[] = ["cpu", "mem", "gpu", "net", "rdma"];

const sum = (a: number | null, b: number | null) => (a == null && b == null ? undefined : (a ?? 0) + (b ?? 0));

export function usage(v: DevPodLive | undefined, key: UsageKey): number | undefined {
  if (!v) return undefined;
  switch (key) {
    case "cpu":
      return v.cpu ?? undefined;
    case "mem":
      return v.mem ?? undefined;
    case "net":
      return sum(v.netRx, v.netTx);
    case "rdma":
      return sum(v.rdmaRx, v.rdmaTx);
    case "gpu": {
      if (!v.gpu.length) return undefined;
      const avg = v.gpu.reduce((a, c) => a + c.util, 0) / v.gpu.length;
      return avg + v.gpu.length / 1000; // equal use: more cards first
    }
  }
}

/** groupUsage: a group's members added up; GPU use averaged over those that have GPUs. */
export function groupUsage(members: (DevPodLive | undefined)[], key: UsageKey): number | undefined {
  const known = members.map((m) => usage(m, key)).filter((x): x is number => x !== undefined);
  if (!known.length) return undefined;
  const total = known.reduce((a, b) => a + b, 0);
  return key === "gpu" ? total / known.length : total;
}
