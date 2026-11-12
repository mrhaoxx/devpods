// The top bar's cluster filter: every page shows all clusters at once;
// this narrows what lists show to one of them. And a note naming the
// clusters that are offline: what is shown of them is their last state.
import { useClusterFilter, useClusterList } from "../../data/clusterFilter";
import type { Cluster } from "./api";

const label = (c: Cluster) => c.displayName || c.name;

export function ClusterFilter() {
  const { filter, setFilter, several, clusters } = useClusterFilter();
  if (!several) return null;
  return (
    <select
      aria-label="集群筛选"
      value={filter}
      onChange={(e) => setFilter(e.target.value)}
      className="h-7 max-w-40 rounded-md border border-line bg-surface px-1 text-xs"
    >
      <option value="">全部集群</option>
      {clusters.map((c) => (
        <option key={c.name} value={c.name}>
          {label(c) + (c.paused ? "（已暂停）" : c.connected ? "" : "（离线）")}
        </option>
      ))}
    </select>
  );
}

const hhmm = (iso?: string) => {
  const d = new Date(iso ?? "");
  return Number.isNaN(d.getTime()) ? "" : d.toLocaleString("zh-CN", { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit", hour12: false });
};

export function OfflineBar() {
  const items = useClusterList().data?.items ?? [];
  const away = items.length >= 2 ? items.filter((c) => !c.connected && !c.paused) : [];
  if (away.length === 0) return null;
  return (
    <div role="status" className="shrink-0 border-b border-line bg-warm-soft px-3 py-1.5 text-xs text-warm">
      {away.map((c) => `集群 ${label(c)} 离线${hhmm(c.lastSeen) ? `（自 ${hhmm(c.lastSeen)}）` : ""}`).join("；")}
      ，显示的是它最后的状态；对它的修改会被拒绝。
    </div>
  );
}
