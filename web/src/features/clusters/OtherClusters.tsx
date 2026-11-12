// 我的 DevPod, below the table of the cluster in use: what I have in the
// other clusters, so that nothing is out of sight because of where the
// switcher stands.
import { useQuery } from "@tanstack/react-query";
import { req, type DevPod } from "../../data/api";
import { rememberCluster, useCurrentCluster } from "../../data/cluster";
import { Button } from "../../design/Button";
import { StatusDot } from "../../design/StatusDot";
import { useClusters } from "./ClusterSwitcher";

type Row = DevPod & { cluster?: string };
type Everywhere = { items: Row[]; stale?: Record<string, string> };

/** Every cluster at once: the plain path, whatever cluster is in use. */
const listEverywhere = () => req<Everywhere>("GET", "/api/devpods", undefined, { global: true });

export function OtherClusters({ go = () => window.location.assign("/") }: { go?: () => void }) {
  const clusters = useClusters().data?.items ?? [];
  const cur = useCurrentCluster();
  const several = clusters.length > 1;
  const q = useQuery({ queryKey: ["devpods", "everywhere"], queryFn: listEverywhere, enabled: several, refetchInterval: 30_000 });
  if (!several || !q.data) return null;
  const elsewhere = q.data.items.filter((d) => d.cluster && d.cluster !== cur);
  if (elsewhere.length === 0) return null;
  const names = [...new Set(elsewhere.map((d) => d.cluster!))].sort();
  const label = (n: string) => clusters.find((c) => c.name === n)?.displayName || n;
  return (
    <section aria-label="其它集群里的 DevPod" className="m-3 rounded-md border border-line bg-surface text-xs">
      <header className="border-b border-line px-3 py-2 font-medium">其它集群里的 DevPod</header>
      {names.map((n) => {
        const mine = elsewhere.filter((d) => d.cluster === n);
        return (
          <div key={n} className="border-b border-line px-3 py-2 last:border-0">
            <div className="mb-1 flex items-center gap-2">
              <span className="font-medium">{label(n)}</span>
              <span className="text-muted">{mine.length} 个</span>
              {q.data.stale?.[n] && <span className="text-warm">离线，显示的是最后状态</span>}
              <span className="flex-1" />
              <Button
                aria-label={`切换到 ${n}`}
                onClick={() => {
                  rememberCluster(n);
                  go();
                }}
              >
                切换到该集群
              </Button>
            </div>
            <ul className="flex flex-wrap gap-x-4 gap-y-1">
              {mine.map((d) => (
                <li key={d.metadata.name} className="flex items-center gap-1.5">
                  <StatusDot phase={d.status?.phase} showLabel={false} />
                  <span className="mono">{d.metadata.name}</span>
                </li>
              ))}
            </ul>
          </div>
        );
      })}
    </section>
  );
}
