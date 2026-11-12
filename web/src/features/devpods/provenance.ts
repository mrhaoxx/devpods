// Where a DevPod came from (config-centric create spec §2.2): the config
// it was launched from, and — launched with others — its group.
import type { DevPod } from "../../data/api";

export type Provenance = { config?: string; group?: string; index?: number; size?: number; leader?: string };

const num = (s?: string): number | undefined => (s && /^\d+$/.test(s) ? Number(s) : undefined);

export function provenance(dp: DevPod): Provenance {
  const l = dp.metadata.labels ?? {};
  const a = dp.metadata.annotations ?? {};
  const out: Provenance = {};
  if (l["devpod.io/config"]) out.config = l["devpod.io/config"];
  if (l["devpod.io/group"]) out.group = l["devpod.io/group"];
  const index = num(a["devpod.io/group-index"]);
  const size = num(a["devpod.io/group-size"]);
  if (index !== undefined) out.index = index;
  if (size !== undefined) out.size = size;
  if (a["devpod.io/group-leader"]) out.leader = a["devpod.io/group-leader"];
  return out;
}
