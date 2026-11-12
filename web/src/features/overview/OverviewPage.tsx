// 集群总览 (UI spec §5.3), refreshed every second from the overview stream.
import { devpodHref } from "../../data/cluster";
import { Link, useNavigate } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { ApiFailure, patchDevPod, type AttentionItem, type KPI, type NodeLive, type UserLive } from "../../data/api";
import { useAuditSessions, useMe } from "../../data/queries";
import { overviewStream } from "../../data/live/streams";
import { useLiveRow } from "../../data/live/hooks";
import { Kpi } from "../../design/Kpi";
import { Panel } from "../../design/Panel";
import { Button } from "../../design/Button";
import { EmptyState } from "../../design/EmptyState";
import { Badge } from "../../design/Badge";
import { fmtAge, fmtBytes, fmtCores, fmtRate } from "../../lib/format";
import { useNow } from "../../lib/useNow";
import { attentionAction, attentionTitle } from "./attention";

const spec = overviewStream();

function KpiStrip({ k }: { k?: KPI }) {
  const v = (s: string) => (k ? s : "—");
  return (
    <div className="grid grid-cols-6 divide-x divide-line rounded-lg border border-line bg-surface max-[1100px]:grid-cols-3 max-[700px]:grid-cols-2">
      <Kpi label="CPU 使用/可分配（核）" value={v(k ? `${fmtCores(k.cpuUsed)}/${fmtCores(k.cpuAllocatable)}` : "")} />
      <Kpi label="内存 使用/可分配" value={v(k ? `${fmtBytes(k.memUsed)}/${fmtBytes(k.memAllocatable)}` : "")} />
      <Kpi label="GPU 已占/总数" value={v(k ? `${k.gpuUsed}/${k.gpuTotal}` : "")} />
      <Kpi label="DevPod 运行 · 启动中 · 失败" value={v(k ? `${k.running} · ${k.pending} · ${k.failed}` : "")} sub={k ? `休眠 ${k.stopped}` : undefined} />
      <Kpi label="在线会话 · 用户" value={v(k ? `${k.sessions} · ${k.sessionUsers}` : "")} />
      <Kpi label="RDMA 合计" value={v(k ? fmtRate(k.rdma) : "")} />
    </div>
  );
}

function HeatStrip({ nodes }: { nodes: NodeLive[] }) {
  const nav = useNavigate();
  if (!nodes.length) return <EmptyState title="没有节点数据" />;
  return (
    <div className="flex flex-wrap gap-1 p-2">
      {nodes.map((n) => {
        const pct = n.cpuAllocatable > 0 ? Math.min(100, Math.round((n.cpuUsed / n.cpuAllocatable) * 100)) : 0;
        return (
          <button
            key={n.name}
            type="button"
            onClick={() => nav(`/admin/nodes?node=${encodeURIComponent(n.name)}`)}
            aria-label={`${n.name} ${n.ready ? `CPU ${pct}%` : "NotReady"}${n.gpuTotal ? ` GPU ${n.gpuUsed}/${n.gpuTotal}` : ""}`}
            title={`${n.name} · CPU ${pct}%${n.gpuTotal ? ` · GPU ${n.gpuUsed}/${n.gpuTotal}` : ""}`}
            className={`relative h-11 w-[72px] rounded border border-line text-left ${n.ready ? "" : "bg-idle-soft text-muted"}`}
            style={n.ready ? { background: `color-mix(in srgb, var(--color-accent) ${pct}%, var(--color-surface))` } : undefined}
          >
            <span className="mono absolute left-1 top-0.5 max-w-[64px] truncate text-[10px]">{n.name}</span>
            <span className="mono absolute bottom-0.5 left-1 text-[10px]">{n.ready ? `${pct}%` : "NotReady"}</span>
            {n.gpuTotal > 0 && <span className="mono absolute bottom-0.5 right-1 text-[10px]">{`${n.gpuUsed}/${n.gpuTotal}`}</span>}
          </button>
        );
      })}
    </div>
  );
}

function Attention({ items }: { items: AttentionItem[] }) {
  const qc = useQueryClient();
  const [done, setDone] = useState<Record<string, string>>({});
  const now = useNow(30_000);
  if (!items.length) return <EmptyState title="没有需要处理的事项" />;
  return (
    <ul className="divide-y divide-line text-xs">
      {items.map((a, i) => {
        const act = attentionAction(a);
        const key = `${a.kind}:${a.uid ?? a.owner}:${i}`;
        return (
          <li key={key} className="flex flex-wrap items-center gap-2 px-3 py-1.5">
            <Badge tone={a.kind === "alert" ? "fail" : "warm"}>{attentionTitle[a.kind] ?? a.kind}</Badge>
            {a.devpod && (
              <Link to={devpodHref({ cluster: a.cluster, name: a.devpod })} className="mono hover:text-accent">
                {a.devpod}
              </Link>
            )}
            {a.owner && <span className="mono text-muted">{a.owner}</span>}
            <span className="min-w-0 flex-1 truncate text-muted" title={a.detail}>
              {a.detail}
            </span>
            {a.since && !a.since.startsWith("0001") && <span className="mono text-faint">{fmtAge(a.since, now)}</span>}
            {done[key] ? (
              <span className="text-muted">{done[key]}</span>
            ) : act?.op === "hibernate" && a.devpod ? (
              <Button
                onClick={() =>
                  patchDevPod({ cluster: a.cluster, name: a.devpod! }, false)
                    .then(() => setDone((d) => ({ ...d, [key]: "已休眠" })))
                    .catch((e) => setDone((d) => ({ ...d, [key]: e instanceof ApiFailure ? e.body.message : String(e) })))
                    .finally(() => void qc.invalidateQueries({ queryKey: ["devpods"] }))
                }
              >
                {act.label}
              </Button>
            ) : act?.href ? (
              <Link to={act.href} className="text-accent hover:underline">
                {act.label}
              </Link>
            ) : null}
          </li>
        );
      })}
    </ul>
  );
}

function TopUsers({ users }: { users: UserLive[] }) {
  const top = [...users].sort((a, b) => b.cpu - a.cpu || b.gpu - a.gpu).slice(0, 10);
  if (!top.length) return <EmptyState title="没有数据" />;
  return (
    <table className="w-full text-xs">
      <thead>
        <tr className="h-7 border-b border-line text-left text-[11px] text-muted">
          <th className="px-3 font-medium">用户</th>
          <th className="px-2 text-right font-medium">DevPod</th>
          <th className="px-2 text-right font-medium">CPU</th>
          <th className="px-2 text-right font-medium">内存</th>
          <th className="px-3 text-right font-medium">GPU</th>
        </tr>
      </thead>
      <tbody>
        {top.map((u) => (
          <tr key={u.user} className="h-7 border-b border-line">
            <td className="px-3">
              <Link to={`/admin/users?user=${encodeURIComponent(u.user)}`} className="mono hover:text-accent">
                {u.user}
              </Link>
            </td>
            <td className="mono px-2 text-right">
              {u.running}/{u.devpods}
            </td>
            <td className="mono px-2 text-right">{fmtCores(u.cpu)}</td>
            <td className="mono px-2 text-right">{fmtBytes(u.mem)}</td>
            <td className="mono px-3 text-right">{u.gpu}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

function RecentAudit() {
  const me = useMe().data;
  const audit = !!me?.features.audit;
  const q = useAuditSessions({ limit: "20" }, { enabled: audit, refetchInterval: 5000 });
  if (!audit) return <EmptyState title="审计未开启" />;
  if (q.isError) return <EmptyState tone="error" title="审计数据暂不可用" hint={q.error instanceof ApiFailure ? q.error.body.message : String(q.error)} />;
  const items = q.data?.items ?? [];
  if (!items.length) return <EmptyState title={q.isLoading ? "加载中…" : "最近没有会话"} />;
  return (
    <ul className="divide-y divide-line text-xs">
      {items.map((s) => (
        <li key={s.session_id} className="flex items-center gap-2 px-3 py-1">
          <span className="mono">{s.user}</span>
          <span className="text-muted">→</span>
          <span className="mono min-w-0 flex-1 truncate">{s.devpod}</span>
          {s.alerts > 0 && <Badge tone="fail">告警 {s.alerts}</Badge>}
          {!s.closed_at && <Badge tone="run">在线</Badge>}
          <Link to={`/sessions/${s.session_id}`} className="text-accent hover:underline">
            {s.closed_at ? "回放" : "直播"}
          </Link>
        </li>
      ))}
    </ul>
  );
}

export function OverviewPage() {
  const kpi = useLiveRow<KPI>(spec, "kpi");
  const nodes = useLiveRow<NodeLive[]>(spec, "nodes");
  const users = useLiveRow<UserLive[]>(spec, "users");
  const attention = useLiveRow<AttentionItem[]>(spec, "attention");
  const off = kpi.status === "off";
  return (
    <div className="space-y-3 p-3">
      {off && <EmptyState title="实时指标未开启" hint="部署未启用遥测（telemetry.enabled）" />}
      <KpiStrip k={kpi.row?.value} />
      <Panel title="节点">
        <HeatStrip nodes={nodes.row?.value ?? []} />
      </Panel>
      <div className="grid gap-3 lg:grid-cols-2">
        <Panel title="需要处理">
          <Attention items={attention.row?.value ?? []} />
        </Panel>
        <Panel title="用量最大的用户">
          <TopUsers users={users.row?.value ?? []} />
        </Panel>
      </div>
      <Panel title="最近审计活动">
        <RecentAudit />
      </Panel>
    </div>
  );
}
