// Usage-vs-quota per user for the table's bars (UI spec §5.5): the
// allocation the quota counts (running DevPods' limits); a user's own
// quota field overrides the cluster default.
import type { AdminUserRow, UserQuota } from "../../data/api";
import { meterLevel } from "../../design/Meter";
import { parseQuantity } from "../../lib/format";

export type Bar = { used: number; limit: number; custom: boolean };

export function bars(u: AdminUserRow, def: UserQuota) {
  const lim = (k: string) => u.quota?.compute?.[k] ?? def.compute?.[k];
  const bar = (used: string | undefined, k: string): Bar | null => {
    const l = lim(k);
    return l ? { used: parseQuantity(used), limit: parseQuantity(l), custom: u.quota?.compute?.[k] !== undefined } : null;
  };
  const maxPods = u.quota?.maxDevPods ?? def.maxDevPods;
  return {
    cpu: bar(u.usage.cpu, "cpu"),
    mem: bar(u.usage.memory, "memory"),
    gpu: bar(u.usage.gpu, "nvidia.com/gpu"),
    pods: maxPods != null ? { used: u.devpods, limit: maxPods, custom: u.quota?.maxDevPods !== undefined } : null,
  };
}

/** high: any bar at 80 % or more. */
export function isHigh(u: AdminUserRow, def: UserQuota): boolean {
  return Object.values(bars(u, def)).some((b) => b && meterLevel(b.used, b.limit) !== "ok");
}
