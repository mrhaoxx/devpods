// The processes as a tree: each under its parent, children by pid; a
// process whose parent is not in the list (or not known) is a root.
import type { ProcLive } from "../../../data/api";

export type TreeRow = { p: ProcLive; depth: number; last: boolean };

export function procTree(procs: ProcLive[]): TreeRow[] {
  const pids = new Set(procs.map((p) => p.pid));
  const kids = new Map<number, ProcLive[]>();
  const roots: ProcLive[] = [];
  for (const p of procs) {
    const parent = p.ppid && p.ppid !== p.pid && pids.has(p.ppid) ? p.ppid : 0;
    if (!parent) roots.push(p);
    else kids.set(parent, [...(kids.get(parent) ?? []), p]);
  }
  const byPid = (a: ProcLive, b: ProcLive) => a.pid - b.pid;
  const out: TreeRow[] = [];
  const seen = new Set<number>();
  const walk = (list: ProcLive[], depth: number) => {
    list.sort(byPid).forEach((p, i) => {
      if (seen.has(p.pid)) return; // a loop in what was reported: once
      seen.add(p.pid);
      out.push({ p, depth, last: i === list.length - 1 });
      walk(kids.get(p.pid) ?? [], depth + 1);
    });
  };
  walk(roots, 0);
  for (const p of procs) if (!seen.has(p.pid)) out.push({ p, depth: 0, last: true }); // only reachable through a loop
  return out;
}
