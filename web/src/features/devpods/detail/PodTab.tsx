// The Pod behind a DevPod, the way `kubectl describe pod` tells it: where
// it runs, the state of each container and why, what is mounted, how it
// was scheduled — sections of the 容器 tab. For the question "why does my
// DevPod not start".
import type { Ref } from "../../../data/cluster";
import type { ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { ApiFailure, getDevPodPod, type PodContainer, type PodDetail } from "../../../data/api";
import { Button } from "../../../design/Button";
import { EmptyState } from "../../../design/EmptyState";
import { Notice } from "../../../design/Form";
import { Panel } from "../../../design/Panel";

const when = (t?: string) => (t ? new Date(t).toLocaleString() : "");
const toneClass = { ok: "text-run", wait: "text-warm", fail: "text-fail", "": "text-muted" } as const;
const pairs = (m?: Record<string, string>) =>
  Object.entries(m ?? {})
    .map(([k, v]) => `${k}=${v}`)
    .sort();

function Item({ k, children }: { k: string; children: ReactNode }) {
  return (
    <div className="min-w-0">
      <dt className="text-[11px] text-faint">{k}</dt>
      <dd className="mono truncate text-xs" title={typeof children === "string" ? children : undefined}>
        {children || <span className="text-faint">—</span>}
      </dd>
    </div>
  );
}

function Containers({ label, items }: { label: string; items: PodContainer[] }) {
  if (items.length === 0) return null;
  return (
    <Panel title={label}>
      <ul aria-label={label} className="divide-y divide-line text-xs">
        {items.map((c) => {
          const req = pairs(c.requests);
          const lim = pairs(c.limits);
          return (
            <li key={c.name} className="space-y-0.5 px-3 py-2">
              <div className="flex flex-wrap items-baseline gap-x-3">
                <span className="mono font-semibold">{c.name}</span>
                <span className="mono min-w-0 truncate text-muted">{c.image}</span>
                {c.restarts > 0 && <span className="text-warm">重启 {c.restarts} 次</span>}
              </div>
              <div className={`mono break-words ${toneClass[c.tone ?? ""]}`}>{c.state || "还没有状态"}</div>
              {c.lastState && <div className="mono break-words text-muted">上次：{c.lastState}</div>}
              {(req.length > 0 || lim.length > 0) && (
                <div className="mono text-muted">
                  {req.length > 0 && <>requests {req.join(" ")}</>}
                  {req.length > 0 && lim.length > 0 && " · "}
                  {lim.length > 0 && <>limits {lim.join(" ")}</>}
                </div>
              )}
              {c.mounts && c.mounts.length > 0 && <div className="mono break-words text-faint">{c.mounts.join("　")}</div>}
            </li>
          );
        })}
      </ul>
    </Panel>
  );
}

function Chips({ label, items }: { label: string; items: string[] }) {
  if (items.length === 0) return null;
  return (
    <Panel title={label}>
      <ul aria-label={label} className="flex flex-wrap gap-1.5 p-3">
        {items.map((x) => (
          <li key={x} className="mono rounded border border-line bg-sunk px-1.5 py-0.5 text-[11px]">
            {x}
          </li>
        ))}
      </ul>
    </Panel>
  );
}

/** usePod: the DevPod's Pod (null: it has none), kept fresh. */
export function usePod(r: Ref) {
  return useQuery({ queryKey: ["devpod-pod", r.cluster ?? "", r.name], queryFn: () => getDevPodPod(r), refetchInterval: 5000, retry: false });
}
type PodQuery = ReturnType<typeof usePod>;

function Refresh({ q }: { q: PodQuery }) {
  return (
    <Button onClick={() => void q.refetch()} disabled={q.isFetching}>
      刷新
    </Button>
  );
}

/** PodStatus: where the Pod runs, its conditions and the state of each
 *  container — or that there is no Pod. */
export function PodStatus({ q }: { q: PodQuery }) {
  if (q.isError) {
    return (
      <div className="space-y-2">
        <Notice>加载失败：{q.error instanceof ApiFailure ? q.error.body.message : String(q.error)}</Notice>
        <Refresh q={q} />
      </div>
    );
  }
  if (!q.data) return <EmptyState title="加载中…" />;
  const pod = q.data.pod;
  if (!pod) {
    return (
      <Panel title="Pod" actions={<Refresh q={q} />}>
        <EmptyState title="没有 Pod" hint="DevPod 已休眠，或者刚启动、Pod 还没有创建出来。" />
      </Panel>
    );
  }
  const phaseTone = pod.phase === "Running" || pod.phase === "Succeeded" ? "text-run" : pod.phase === "Failed" ? "text-fail" : "text-warm";
  return (
    <>
      <Panel title={<span className="mono">Pod — {pod.name}</span>} actions={<Refresh q={q} />}>
        <dl role="group" aria-label="概要" className="grid grid-cols-2 gap-x-4 gap-y-2 p-3 sm:grid-cols-4">
          <Item k="阶段">
            <span className={phaseTone}>{pod.phase}</span>
            {pod.deletingSince && <span className="text-warm">（删除中）</span>}
          </Item>
          <Item k="就绪">{pod.ready}</Item>
          <Item k="重启">{String(pod.restarts)}</Item>
          <Item k="QoS">{pod.qos}</Item>
          <Item k="节点">{pod.node}</Item>
          <Item k="主机 IP">{pod.hostIP}</Item>
          <Item k="Pod IP">{pod.podIP}</Item>
          <Item k="网络">{pod.hostNetwork ? "主机网络" : "Pod 网络"}</Item>
          <Item k="创建时间">{when(pod.createdAt)}</Item>
          <Item k="启动时间">{when(pod.startedAt)}</Item>
          <Item k="重启策略">{pod.restartPolicy}</Item>
          <Item k="服务账号">{pod.serviceAccount}</Item>
          <Item k="终止宽限">{pod.gracePeriodSeconds == null ? "" : `${pod.gracePeriodSeconds} 秒`}</Item>
        </dl>
        {(pod.reason || pod.message) && (
          <div className="border-t border-line px-3 py-2 text-xs text-warm">
            {pod.reason}
            {pod.reason && pod.message && "："}
            {pod.message}
          </div>
        )}
      </Panel>

      <Panel title="条件">
        {pod.conditions.length === 0 ? (
          <div className="px-3 py-2 text-xs text-muted">还没有条件（Pod 还没有被调度）。</div>
        ) : (
          <ul aria-label="条件" className="divide-y divide-line text-xs">
            {pod.conditions.map((c) => (
              <li key={c.type} className="flex flex-wrap items-baseline gap-x-3 px-3 py-1.5">
                <span className="mono w-52 shrink-0 font-semibold max-[700px]:w-auto">{c.type}</span>
                <span className={`mono w-12 shrink-0 ${c.status === "True" ? "text-run" : c.status === "False" ? "text-fail" : "text-warm"}`}>{c.status}</span>
                <span className="min-w-0 flex-1 break-words text-muted max-[700px]:basis-full">
                  {c.reason}
                  {c.reason && c.message && "："}
                  {c.message}
                </span>
                <time className="mono shrink-0 text-faint">{when(c.since)}</time>
              </li>
            ))}
          </ul>
        )}
      </Panel>

      <Containers label="初始化容器" items={pod.initContainers} />
      <Containers label="容器" items={pod.containers} />
    </>
  );
}

/** PodVolumes: what the Pod mounts, by source. */
export function PodVolumes({ pod }: { pod: PodDetail }) {
  if (pod.volumes.length === 0) return null;
  return (
    <Panel title="卷">
      <ul aria-label="卷" className="divide-y divide-line text-xs">
        {pod.volumes.map((v) => (
          <li key={v.name} className="flex gap-3 px-3 py-1.5 max-[700px]:flex-col max-[700px]:gap-0">
            <span className="mono w-52 shrink-0 truncate font-semibold max-[700px]:w-auto" title={v.name}>
              {v.name}
            </span>
            <span className="mono min-w-0 flex-1 break-words text-muted">
              {v.kind}
              {v.detail ? `: ${v.detail}` : ""}
            </span>
          </li>
        ))}
      </ul>
    </Panel>
  );
}

/** PodScheduling: the Pod's labels, node selector and tolerations. */
export function PodScheduling({ pod }: { pod: PodDetail }) {
  return (
    <>
      <Chips label="标签" items={pairs(pod.labels)} />
      <Chips label="节点选择" items={pairs(pod.nodeSelector)} />
      <Chips label="容忍" items={pod.tolerations} />
    </>
  );
}
