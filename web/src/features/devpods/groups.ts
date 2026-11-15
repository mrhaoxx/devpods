// DevPods launched together are shown as one row (config-centric create
// spec §5.2), open unless folded: its members follow it, in their order.
// Sorted, a group goes where its totals put it, its members with it,
// themselves in that order.
import type { Row } from "./columns";
import { provenance } from "./provenance";
import { keyOf } from "../../data/cluster";

// a group is launched in one cluster: the same name in another is another group
export const groupKey = (r: Row) => {
  const g = provenance(r.dp).group;
  return g ? keyOf({ cluster: r.cluster, name: g }) : undefined;
};

const byIndex = (a: Row, b: Row) => (provenance(a.dp).index ?? 0) - (provenance(b.dp).index ?? 0) || a.name.localeCompare(b.name);

/** groupRows: rows with every group of two or more made one row, its
 *  members after it unless the group is folded (folded: their keys);
 *  order (if any) sorts the rows, a group as one with its members. */
export function groupRows(rows: Row[], folded: ReadonlySet<string>, order?: (a: Row, b: Row) => number): Row[] {
  const members = new Map<string, Row[]>();
  for (const r of rows) {
    const k = groupKey(r);
    if (k) members.set(k, [...(members.get(k) ?? []), r]);
  }
  const units: Row[][] = [];
  const done = new Set<string>();
  for (const r of rows) {
    const g = provenance(r.dp).group;
    const k = groupKey(r);
    const ms = k ? members.get(k)! : [];
    if (!g || !k || ms.length < 2) {
      units.push([r]);
      continue;
    }
    if (done.has(k)) continue;
    done.add(k);
    const sorted = [...ms].sort(byIndex);
    const phases = new Set(sorted.map((m) => m.phase));
    const head: Row = {
      ...sorted[0],
      name: g,
      phase: phases.size === 1 ? sorted[0].phase : phases.has("Running") ? "Running" : sorted[0].phase,
      node: "",
      gpu: sorted.reduce((n, m) => n + m.gpu, 0),
      cpuLimit: sorted.reduce((n, m) => n + m.cpuLimit, 0),
      memLimit: sorted.reduce((n, m) => n + m.memLimit, 0),
      group: { id: g, key: k, members: sorted },
    };
    const shown = folded.has(k) ? [] : (order ? [...sorted].sort((a, b) => order(a, b) || byIndex(a, b)) : sorted).map((m) => ({ ...m, inGroup: true }));
    units.push([head, ...shown]);
  }
  if (order) units.sort((a, b) => order(a[0], b[0]));
  return units.flat();
}
