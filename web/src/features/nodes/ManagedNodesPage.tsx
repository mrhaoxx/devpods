// 我管理的机器 (grants design 2026-11-20 §6.1): the machines a grant lets
// one switch reservable, and that switch — nothing else of them.
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { listManagedNodes, setReservable, type ManagedNode } from "../../data/api";
import { Panel } from "../../design/Panel";
import { EmptyState } from "../../design/EmptyState";
import { Notice } from "../../design/Form";

export function ManagedNodesPage() {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["nodes-managed"], queryFn: listManagedNodes });
  const flip = useMutation({
    mutationFn: (n: ManagedNode) => setReservable({ cluster: n.cluster, name: n.name }, !n.reservable),
    onSettled: () => qc.invalidateQueries({ queryKey: ["nodes-managed"] }),
  });
  if (q.isError) return <EmptyState tone="error" title="无法加载" hint={String(q.error)} />;
  if (!q.data) return <EmptyState title="加载中…" />;
  const items = q.data.items;
  return (
    <div className="space-y-3 p-3">
      <Panel title="我管理的机器（可以切换是否可预约）">
        {flip.isError && (
          <div className="p-2">
            <Notice>切换失败：{String(flip.error)}</Notice>
          </div>
        )}
        {items.length === 0 ? (
          <EmptyState title="没有你能管理的机器" />
        ) : (
          <table className="w-full text-xs">
            <thead>
              <tr className="border-b border-line text-left text-[11px] text-muted">
                <th className="px-2 py-1">集群</th>
                <th className="px-2">机器</th>
                <th className="px-2">GPU</th>
                <th className="px-2">状态</th>
                <th className="px-2">可预约</th>
              </tr>
            </thead>
            <tbody>
              {items.map((n) => (
                <tr key={`${n.cluster ?? ""}/${n.name}`} className="border-b border-line">
                  <td className="px-2 py-1">{n.cluster ?? ""}</td>
                  <td className="mono px-2">{n.name}</td>
                  <td className="mono px-2">{n.gpuTotal > 0 ? `${(n.gpuModel ?? "GPU").replace(/^NVIDIA-/, "")} × ${n.gpuTotal}` : "—"}</td>
                  <td className="px-2">{n.ready ? "就绪" : <span className="text-warm">未就绪</span>}</td>
                  <td className="px-2">
                    <input type="checkbox" aria-label={`${n.name} 可预约`} checked={n.reservable} disabled={flip.isPending} onChange={() => flip.mutate(n)} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Panel>
    </div>
  );
}
