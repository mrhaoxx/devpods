// 我的镜像: the images saved from DevPods — copy the address, start a
// DevPod from one, watch one still being saved, forget one.
import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { ApiFailure, deleteSnapshot, retrySnapshot, type SnapshotView } from "../../data/api";
import { useMe, useSnapshots } from "../../data/queries";
import { Badge } from "../../design/Badge";
import { ConfirmDialog } from "../../design/ConfirmDialog";
import { useCopied } from "../../design/Copy";
import { EmptyState } from "../../design/EmptyState";
import { Panel } from "../../design/Panel";
import { fmtAge, fmtBytes } from "../../lib/format";
import { SnapshotDialog } from "./SnapshotDialog";

const phaseBadge: Record<SnapshotView["phase"], { tone: "neutral" | "run" | "fail" | "warm"; label: string }> = {
  Pending: { tone: "warm", label: "等待中" },
  Running: { tone: "warm", label: "保存中" },
  Succeeded: { tone: "run", label: "已保存" },
  Failed: { tone: "fail", label: "失败" },
};

function CopyRef({ image }: { image: string }) {
  const [done, copy] = useCopied();
  return (
    <button type="button" onClick={() => copy(image)} className="text-muted hover:text-accent">
      {done ? "已复制" : "复制地址"}
    </button>
  );
}

export function ImagesPage() {
  const me = useMe().data;
  const qc = useQueryClient();
  const [all, setAll] = useState(false);
  const q = useSnapshots({ all: all && !!me?.admin });
  const [follow, setFollow] = useState<SnapshotView | null>(null);
  const [forget, setForget] = useState<SnapshotView | null>(null);
  const del = useMutation({ mutationFn: (id: string) => deleteSnapshot(id), onSettled: () => void qc.invalidateQueries({ queryKey: ["snapshots"] }) });
  const retry = useMutation({ mutationFn: (id: string) => retrySnapshot(id), onSettled: () => void qc.invalidateQueries({ queryKey: ["snapshots"] }) });
  const items = q.data?.items ?? [];
  const busy = (s: SnapshotView) => s.phase === "Pending" || s.phase === "Running";

  return (
    <div className="space-y-3 p-3">
      <Panel
        title="我的镜像"
        actions={
          me?.admin && (
            <label className="flex items-center gap-1.5 text-[11px] text-muted">
              <input type="checkbox" checked={all} onChange={(e) => setAll(e.target.checked)} />
              全部用户
            </label>
          )
        }
      >
        {retry.isError && (
          <p role="alert" className="border-b border-line bg-fail-soft px-3 py-1.5 text-xs text-fail">
            重试失败：{retry.error instanceof ApiFailure ? retry.error.body.message : String(retry.error)}
          </p>
        )}
        <p className="border-b border-line px-3 py-2 text-xs text-muted">
          在 DevPod 详情页点「保存为镜像」，把装好环境的容器存成镜像；之后新建 DevPod 时可以直接选它。镜像不包含持久卷（home 目录）。
        </p>
        {q.isError ? (
          <EmptyState tone="error" title="加载失败" />
        ) : items.length === 0 ? (
          <EmptyState title={q.isLoading ? "加载中…" : "还没有保存过镜像"} />
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full border-collapse text-xs">
              <thead>
                <tr className="h-7 border-b border-line text-left text-[11px] text-muted">
                  <th className="px-3 font-normal">镜像</th>
                  {all && <th className="px-2 font-normal">用户</th>}
                  <th className="px-2 font-normal">来源 DevPod</th>
                  <th className="px-2 font-normal">大小</th>
                  <th className="px-2 font-normal">时间</th>
                  <th className="px-2 font-normal">状态</th>
                  <th className="px-3" />
                </tr>
              </thead>
              <tbody>
                {items.map((s) => (
                  <tr key={s.id} className="border-b border-line align-top last:border-0">
                    <td className="max-w-[28rem] px-3 py-1.5">
                      <div className="mono break-all">{s.image}</div>
                      {s.customRegistry && <div className="text-[11px] text-faint">自定义仓库</div>}
                      {s.phase === "Failed" && s.message && <div className="text-[11px] text-fail">{s.message}</div>}
                    </td>
                    {all && <td className="mono px-2 py-1.5">{s.owner}</td>}
                    <td className="px-2 py-1.5">
                      <Link to={`/devpods/${s.devpod}`} className="mono text-ink hover:text-accent">
                        {s.devpod}
                      </Link>
                    </td>
                    <td className="mono px-2 py-1.5 whitespace-nowrap">{s.sizeBytes ? fmtBytes(s.sizeBytes) : "—"}</td>
                    <td className="px-2 py-1.5 whitespace-nowrap" title={new Date(s.createdAt).toLocaleString()}>
                      {fmtAge(s.createdAt)} 前
                    </td>
                    <td className="px-2 py-1.5">
                      <Badge tone={phaseBadge[s.phase].tone}>{phaseBadge[s.phase].label}</Badge>
                    </td>
                    <td className="px-3 py-1.5 text-right whitespace-nowrap">
                      <span className="inline-flex gap-3">
                        {busy(s) && (
                          <button type="button" onClick={() => setFollow(s)} className="text-accent hover:underline">
                            查看进度
                          </button>
                        )}
                        {s.phase === "Succeeded" && (
                          <>
                            <CopyRef image={s.image} />
                            <Link to={`/?new=1&image=${encodeURIComponent(s.image)}`} className="text-accent hover:underline">
                              新建 DevPod
                            </Link>
                          </>
                        )}
                        {s.phase === "Failed" && (
                          <button type="button" disabled={retry.isPending} onClick={() => retry.mutate(s.id)} className="text-accent hover:underline disabled:opacity-50" title="用同样的设置再保存一次">
                            重试
                          </button>
                        )}
                        {!busy(s) && (
                          <button type="button" onClick={() => setForget(s)} className="text-muted hover:text-fail" aria-label={`删除 ${s.image}`}>
                            删除
                          </button>
                        )}
                      </span>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Panel>
      {follow && <SnapshotDialog devpod={follow.devpod} owner={follow.owner} following={follow} onClose={() => setFollow(null)} />}
      <ConfirmDialog
        open={!!forget}
        onOpenChange={(o) => !o && setForget(null)}
        title="删除这条镜像记录？"
        confirmLabel="删除记录"
        danger
        onConfirm={() => {
          if (forget) del.mutate(forget.id);
          setForget(null);
        }}
      >
        只删除控制台里的记录，仓库里的镜像 <span className="mono text-ink">{forget?.image}</span> 不会被删除。
      </ConfirmDialog>
    </div>
  );
}
