// Live quota check for the create/edit drawer (UI spec §5.6): the same
// limit-based accounting as the server, so most violations show before
// submitting. A blank quota field means unlimited.
import type { Me } from "../../data/api";
import { fmtBytes, fmtCores, parseQuantity } from "../../lib/format";

export type QuotaField = "cpu" | "memory" | "gpu" | "storage" | "count";
type Form = { cpu: string; memory: string; gpu: string; storage?: string };
/** editing: the DevPod's own current limits; a hibernated DevPod takes
 *  compute quota only when it starts. */
type Own = { cpu: number; memory: number; gpu: number; running: boolean };

export function quotaErrors(f: Form, quota: Me["quota"], usage: Me["usage"], editing?: Own): Partial<Record<QuotaField, string>> {
  const out: Partial<Record<QuotaField, string>> = {};
  const compute = quota.compute ?? {};
  const check = (field: "cpu" | "memory" | "gpu", key: string, want: number, fmt: (n: number) => string) => {
    const lim = compute[key];
    if (!lim || (editing && !editing.running)) return;
    const used = parseQuantity(usage.compute?.[key]) - (editing ? editing[field] : 0);
    const remaining = parseQuantity(lim) - used;
    if (want > remaining + 1e-9) out[field] = `超出配额：剩余 ${fmt(Math.max(0, remaining))}`;
  };
  check("cpu", "cpu", parseQuantity(f.cpu), fmtCores);
  check("memory", "memory", parseQuantity(f.memory), fmtBytes);
  check("gpu", "nvidia.com/gpu", parseQuantity(f.gpu), (n) => String(n));
  if (!editing) {
    if (quota.storage && f.storage) {
      const rem = parseQuantity(quota.storage) - parseQuantity(usage.storage);
      if (parseQuantity(f.storage) > rem + 1e-9) out.storage = `超出存储配额：剩余 ${fmtBytes(Math.max(0, rem))}`;
    }
    if (quota.maxDevPods != null && usage.devpods >= quota.maxDevPods) out.count = `DevPod 数量已达上限（${quota.maxDevPods}）`;
  }
  return out;
}

const fieldOf: Record<string, QuotaField> = { cpu: "cpu", memory: "memory", "nvidia.com/gpu": "gpu", storage: "storage", devpods: "count" };

/** serverQuotaErrors maps a QUOTA_EXCEEDED detail to the form's fields. */
export function serverQuotaErrors(detail: unknown): Partial<Record<QuotaField, string>> {
  const out: Partial<Record<QuotaField, string>> = {};
  if (!Array.isArray(detail)) return out;
  for (const v of detail as { resource: string; requested: string; used: string; limit: string }[]) {
    const f = fieldOf[v.resource];
    if (f) out[f] = `超出配额：申请 ${v.requested}，已用 ${v.used}，上限 ${v.limit}`;
  }
  return out;
}
