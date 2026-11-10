// The live streams of UI spec §6.1 and how each splits into row keys.
import type { StreamSpec } from "./LiveHub";
import type { LiveListFrame, OverviewFrame } from "../types";

/** /api/live/devpods: rows keyed by DevPod name. */
export function devpodsStream(scope: "mine" | "all"): StreamSpec {
  return {
    id: `devpods:${scope}`,
    url: `/api/live/devpods?scope=${scope}`,
    rows: (m) => ((m as LiveListFrame).items ?? []).map((it) => [it.name, it] as [string, unknown]),
  };
}

/** /api/admin/live/overview: kpi, attention, nodes, users, plus one row
 *  per node (node:<name>) and per user (user:<name>). */
export function overviewStream(): StreamSpec {
  return {
    id: "overview",
    url: "/api/admin/live/overview",
    rows: (m) => {
      const f = m as OverviewFrame;
      if (!f || !f.kpi) return [];
      const nodes = f.nodes ?? [];
      const users = f.users ?? [];
      return [
        ["kpi", f.kpi],
        ["attention", f.attention ?? []],
        ["nodes", nodes],
        ["users", users],
        ...nodes.map((n) => [`node:${n.name}`, n] as [string, unknown]),
        ...users.map((u) => [`user:${u.user}`, u] as [string, unknown]),
      ];
    },
  };
}

/** /api/devpods/{name}/stream: rows "metrics" and "processes"; DevPod
 *  and event messages go to onOther. */
export function devpodStream(name: string, allProcs: boolean, onOther?: (msg: unknown) => void): StreamSpec {
  return {
    id: `devpod:${name}${allProcs ? ":all" : ""}`,
    url: `/api/devpods/${name}/stream${allProcs ? "?procs=all" : ""}`,
    rows: (m) => {
      const msg = m as { kind?: string; metrics?: unknown; processes?: unknown };
      if (msg.kind === "metrics" && msg.metrics) return [["metrics", msg.metrics]];
      if (msg.kind === "processes") return [["processes", msg.processes ?? []]];
      return [];
    },
    onOther: (m) => {
      const kind = (m as { kind?: string }).kind;
      if (kind === "devpod" || kind === "event") onOther?.(m);
    },
  };
}
