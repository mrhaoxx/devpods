// The cluster in use: chosen in the top bar, settled before any page asks
// for something that is a cluster's.
import { useEffect, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { currentCluster, pickCluster, rememberCluster, useCurrentCluster, useStaleSince } from "../../data/cluster";
import { EmptyState } from "../../design/EmptyState";
import { listClusters, type Cluster } from "./api";

export const useClusters = () => useQuery({ queryKey: ["clusters"], queryFn: listClusters, refetchInterval: 15_000, staleTime: 10_000 });

const label = (c: Cluster) => c.displayName || c.name;

/** ClusterGate holds its children back until the cluster in use is one
 *  that exists (or it is known that there is nothing to choose). */
export function ClusterGate({ children }: { children: ReactNode }) {
  const q = useClusters();
  const cur = useCurrentCluster();
  const want = q.data ? pickCluster(q.data.items, cur) : undefined;
  useEffect(() => {
    if (want !== undefined && want !== currentCluster()) rememberCluster(want);
  }, [want]);
  if (q.isError) return <EmptyState tone="error" title="无法加载集群列表" hint={String(q.error)} />;
  if (want === undefined || want !== cur) return null;
  return <>{children}</>;
}

/** ClusterSwitcher: shown when there is more than one cluster. Switching
 *  reloads the console (go), so nothing of the other cluster stays on
 *  screen or in a stream. */
export function ClusterSwitcher({ go = () => window.location.assign("/") }: { go?: () => void }) {
  const q = useClusters();
  const cur = useCurrentCluster();
  const items = q.data?.items ?? [];
  if (items.length < 2) return null;
  return (
    <select
      aria-label="集群"
      value={cur}
      onChange={(e) => {
        rememberCluster(e.target.value);
        go();
      }}
      className="h-7 max-w-40 rounded-md border border-line bg-surface px-1 text-xs"
    >
      {items.map((c) => (
        <option key={c.name} value={c.name}>
          {label(c) + (c.paused ? "（已暂停）" : c.connected ? "" : "（离线）")}
        </option>
      ))}
    </select>
  );
}

const hhmm = (iso: string) => {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString("zh-CN", { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit", hour12: false });
};

/** StaleBar: the cluster in use is away; what is shown is what was last
 *  known of it. */
export function StaleBar() {
  const since = useStaleSince();
  const cur = useCurrentCluster();
  if (!since) return null;
  return (
    <div role="status" className="shrink-0 border-b border-line bg-warm-soft px-3 py-1.5 text-xs text-warm">
      集群 {cur} 当前离线：显示的是 {hhmm(since)} 的最后状态，恢复连接前无法修改。
    </div>
  );
}
