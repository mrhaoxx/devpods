// DevPods launched together are shown as one row (config-centric create
// spec §5.2); opened, their own rows follow it, in their order.
import type { Row } from "./columns";
import { provenance } from "./provenance";
import { keyOf } from "../../data/cluster";

// a group is launched in one cluster: the same name in another is another group
const groupKey = (r: Row) => {
  const g = provenance(r.dp).group;
  return g ? keyOf({ cluster: r.cluster, name: g }) : undefined;
};

/** groupRows: rows with every group of two or more folded into one. */
export function groupRows(rows: Row[], open: ReadonlySet<string>): Row[] {
  const members = new Map<string, Row[]>();
  for (const r of rows) {
    const k = groupKey(r);
    if (k) members.set(k, [...(members.get(k) ?? []), r]);
  }
  const out: Row[] = [];
  const done = new Set<string>();
  for (const r of rows) {
    const g = provenance(r.dp).group;
    const k = groupKey(r);
    const ms = k ? members.get(k)! : [];
    if (!g || !k || ms.length < 2) {
      out.push(r);
      continue;
    }
    if (done.has(k)) continue;
    done.add(k);
    const sorted = [...ms].sort((a, b) => (provenance(a.dp).index ?? 0) - (provenance(b.dp).index ?? 0) || a.name.localeCompare(b.name));
    const phases = new Set(sorted.map((m) => m.phase));
    out.push({
      ...sorted[0],
      name: g,
      phase: phases.size === 1 ? sorted[0].phase : phases.has("Running") ? "Running" : sorted[0].phase,
      node: "",
      gpu: sorted.reduce((n, m) => n + m.gpu, 0),
      cpuLimit: sorted.reduce((n, m) => n + m.cpuLimit, 0),
      memLimit: sorted.reduce((n, m) => n + m.memLimit, 0),
      group: { id: g, members: sorted },
    });
    if (open.has(g)) out.push(...sorted);
  }
  return out;
}
