// 容器 (UI spec §5.2): the DevPod's container from both sides, top down —
// the Pod behind it as Kubernetes tells it (where it runs, its conditions,
// the state of each container and why: what to look at when it does not
// start), what the agent reports from inside (system, limits, GPUs, RDMA,
// the Kore CPU layout), its storage, how it was scheduled, the collectors.
import type { Ref } from "../../../data/cluster";
import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { ApiFailure, devpodTopology, parseCpuList } from "../../../data/api";
import { useInfo } from "../../../data/queries";
import { Panel } from "../../../design/Panel";
import { EmptyState } from "../../../design/EmptyState";
import { Badge } from "../../../design/Badge";
import { fmtBytes, fmtCores } from "../../../lib/format";
import { NodeZones, TopologyLegend } from "../../nodes/KoreTopology";
import { PodScheduling, PodStatus, PodVolumes, usePod } from "./PodTab";

function KV({ rows }: { rows: [string, ReactNode][] }) {
  const shown = rows.filter(([, v]) => v !== undefined && v !== null && v !== "");
  if (!shown.length) return <EmptyState title="无" />;
  return (
    <dl className="grid grid-cols-[7rem_1fr] gap-x-3 gap-y-1 p-3 text-xs">
      {shown.map(([k, v]) => (
        <div key={k} className="contents">
          <dt className="text-muted">{k}</dt>
          <dd className="mono min-w-0 break-words">{v}</dd>
        </div>
      ))}
    </dl>
  );
}

export function ContainerTab({ r, ports = false }: { r: Ref; /** the 端口 tab lists the listeners already */ ports?: boolean }) {
  const pod = usePod(r);
  const info = useInfo(r);
  const topo = useQuery({ queryKey: ["devpod-topo", r.cluster ?? "", r.name], queryFn: () => devpodTopology(r), refetchInterval: 10_000, retry: false });
  const p = pod.data?.pod;
  // no Pod (hibernated, or not made yet): that is all there is to tell
  if (pod.data && !p) return <PodStatus q={pod} />;

  const kore = topo.data?.node ? (
    <Panel title={`CPU 布局 · ${topo.data.node.node}`} actions={<TopologyLegend mine />}>
      <div className="p-3">
        <NodeZones cluster={r.cluster} node={topo.data.node} reveal={false} mine={parseCpuList(topo.data.cpuset)} />
      </div>
    </Panel>
  ) : null;

  let inside: ReactNode;
  let storage: ReactNode = null;
  let collectors: ReactNode = null;
  if (info.isError) {
    const e = info.error;
    const none = e instanceof ApiFailure && e.code === "NO_TELEMETRY";
    inside = <EmptyState tone={none ? "neutral" : "error"} title={none ? "暂无容器信息" : "容器信息暂不可用"} hint={e instanceof ApiFailure ? e.body.message : String(e)} />;
  } else if (!info.data) {
    inside = <EmptyState title="加载中…" />;
  } else {
    const i = info.data.info;
    const snap = info.data.snapshot;
    inside = (
      <div className="grid grid-cols-1 gap-3 lg:grid-cols-2 [&>*]:min-w-0">
        <Panel title="系统">
          <KV
            rows={[
              ["OS", i.os],
              ["内核", i.kernel],
              ["Shell", i.shell],
              ["采集器版本", i.agent_version],
              ["进程 / 线程", snap ? `${snap.procs} / ${snap.threads}${snap.zombies ? `（僵尸 ${snap.zombies}）` : ""}` : undefined],
            ]}
          />
        </Panel>
        <Panel title="限额">
          <KV
            rows={[
              ["CPU", i.cpu_limit ? `${fmtCores(i.cpu_limit)} 核` : "不限"],
              ["cpuset", i.cpuset],
              ["内存", i.mem_limit ? fmtBytes(i.mem_limit) : "不限"],
            ]}
          />
        </Panel>
        <Panel title="GPU">
          {/* listed in index order: the position is the GPU's index in the container */}
          <KV rows={(i.gpus ?? []).map((g, n) => [`GPU${n}`, `${g.name} · ${Math.round(g.mem_total)} MiB${g.driver ? ` · 驱动 ${g.driver}` : ""}${g.cuda ? ` · CUDA ${g.cuda}` : ""}`])} />
        </Panel>
        <Panel title="RDMA 设备">
          <KV rows={(i.rdma ?? []).map((d) => [`${d.ibdev}/${d.port}`, <Badge key="s">{d.scope === "vf_exclusive" ? "VF" : "shared"}</Badge>])} />
        </Panel>
        {!ports && (
          <Panel title="监听端口">
            <KV rows={(snap?.listen ?? []).map((l) => [`${l.proto} ${l.port}`, `${l.addr ?? ""}${l.pid ? ` · pid ${l.pid}` : ""}`])} />
          </Panel>
        )}
      </div>
    );
    storage = (
      <Panel title="存储">
        <KV rows={(snap?.storage ?? []).map((s) => [s.mount, `${fmtBytes(s.used)} / ${fmtBytes(s.size)}`])} />
      </Panel>
    );
    if (i.collectors) {
      collectors = (
        <Panel title="采集器">
          <KV rows={Object.entries(i.collectors).map(([k, v]) => [k, v])} />
        </Panel>
      );
    }
  }

  return (
    <div className="space-y-3">
      <PodStatus q={pod} />
      {inside}
      {kore}
      {(storage || (p && p.volumes.length > 0)) && (
        <div className="grid grid-cols-1 gap-3 lg:grid-cols-2 [&>*]:min-w-0">
          {storage}
          {p && <PodVolumes pod={p} />}
        </div>
      )}
      {p && <PodScheduling pod={p} />}
      {collectors}
    </div>
  );
}
