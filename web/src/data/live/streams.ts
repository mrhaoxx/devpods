// The live streams of UI spec §6.1 and how each splits into row keys.
import type { StreamSpec } from "./LiveHub";
import type { LiveListFrame, OverviewFrame } from "../types";
import { apiIn, keyOf, type Ref } from "../cluster";

/** /api/live/devpods: every cluster's rows, keyed <cluster>/<name>. */
export function devpodsStream(scope: "mine" | "all"): StreamSpec {
  return {
    id: `devpods:${scope}`,
    url: `/api/live/devpods?scope=${scope}`,
    rows: (m) => ((m as LiveListFrame).items ?? []).map((it) => [keyOf(it), it] as [string, unknown]),
  };
}

/** /api/admin/live/overview: kpi, attention, nodes, users, plus one row
 *  per node (node:<cluster>/<name>, or node:<name> in a console of one)
 *  and per user (user:<name>). With a cluster: that cluster's numbers. */
export function overviewStream(cluster?: string): StreamSpec {
  return {
    id: `overview${cluster ? `:${cluster}` : ""}`,
    url: apiIn(cluster, "/api/admin/live/overview"),
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
        ...nodes.map((n) => [`node:${keyOf(n)}`, n] as [string, unknown]),
        ...users.map((u) => [`user:${u.user}`, u] as [string, unknown]),
      ];
    },
  };
}

/** /api/devpods/{name}/stream: rows "metrics" and "processes"; DevPod
 *  and event messages go to onOther. */
export function devpodStream(r: Ref, allProcs: boolean, onOther?: (msg: unknown) => void): StreamSpec {
  return {
    id: `devpod:${keyOf(r)}${allProcs ? ":all" : ""}`,
    url: apiIn(r.cluster, `/api/devpods/${r.name}/stream${allProcs ? "?procs=all" : ""}`),
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
