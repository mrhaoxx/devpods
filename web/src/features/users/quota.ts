// Usage-vs-quota per user for the table's bars (UI spec §5.5): the
// allocation the quota counts (running DevPods' limits); a user's own
// quota field overrides the cluster default. Quotas are counted per
// cluster: in a console of several, a bar is one cluster's — the one
// asked for, or the one nearest its limit.
import type { AdminUserRow, UserQuota } from "../../data/api";
import { meterLevel } from "../../design/Meter";
import { parseQuantity } from "../../lib/format";

/** cluster: whose usage and quota, in a console of several */
export type Bar = { used: number; limit: number; custom: boolean; cluster?: string };
type Bars = { cpu: Bar | null; mem: Bar | null; gpu: Bar | null; pods: Bar | null };

function barsIn(
  usage: AdminUserRow["usage"],
  devpods: number,
  quota: UserQuota,
  own: (UserQuota | undefined)[],
  cluster?: string,
): Bars {
  const custom = (get: (q: UserQuota) => unknown) => own.some((q) => q && get(q) !== undefined);
  const bar = (used: string | undefined, k: string): Bar | null => {
    const l = quota.compute?.[k];
    return l ? { used: parseQuantity(used), limit: parseQuantity(l), custom: custom((q) => q.compute?.[k]), cluster } : null;
  };
  return {
    cpu: bar(usage.cpu, "cpu"),
    mem: bar(usage.memory, "memory"),
    gpu: bar(usage.gpu, "nvidia.com/gpu"),
    pods: quota.maxDevPods != null ? { used: devpods, limit: quota.maxDevPods, custom: custom((q) => q.maxDevPods), cluster } : null,
  };
}

const ratio = (b: Bar | null) => (b && b.limit > 0 ? b.used / b.limit : -1);

export function bars(u: AdminUserRow, def: UserQuota, cluster?: string): Bars {
  const per = u.clusters;
  if (!per || Object.keys(per).length === 0) {
    const quota: UserQuota = {
      maxDevPods: u.quota?.maxDevPods ?? def.maxDevPods,
      compute: { ...def.compute, ...u.quota?.compute },
      storage: u.quota?.storage ?? def.storage,
    };
    return barsIn(u.usage, u.devpods, quota, [u.quota]);
  }
  const names = cluster && per[cluster] ? [cluster] : Object.keys(per).sort();
  const each = names.map((c) => barsIn(per[c].usage, per[c].devpods, per[c].quota, [u.quota, u.quotas?.[c]], c));
  const worst = (k: keyof Bars) => each.map((b) => b[k]).reduce<Bar | null>((a, b) => (ratio(b) > ratio(a) ? b : a), null);
  return { cpu: worst("cpu"), mem: worst("mem"), gpu: worst("gpu"), pods: worst("pods") };
}

/** high: any bar at 80 % or more (in any cluster, unless one is asked for). */
export function isHigh(u: AdminUserRow, def: UserQuota, cluster?: string): boolean {
  return Object.values(bars(u, def, cluster)).some((b) => b && meterLevel(b.used, b.limit) !== "ok");
}
