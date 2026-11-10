// 容器 (UI spec §5.2): what the agent reported about the container, plus
// the Kore CPU layout when the DevPod is bound.
import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { ApiFailure, devpodTopology, parseCpuList } from "../../../data/api";
import { useInfo } from "../../../data/queries";
import { Panel } from "../../../design/Panel";
import { EmptyState } from "../../../design/EmptyState";
import { Badge } from "../../../design/Badge";
import { fmtBytes, fmtCores } from "../../../lib/format";
import { NodeZones, TopologyLegend } from "../../../kore";

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

export function ContainerTab({ name }: { name: string }) {
  const info = useInfo(name);
  const topo = useQuery({ queryKey: ["devpod-topo", name], queryFn: () => devpodTopology(name), refetchInterval: 10_000, retry: false });
  const kore = topo.data?.node ? (
    <Panel title={`CPU 布局 · ${topo.data.node.node}`} actions={<TopologyLegend mine />}>
      <div className="p-3">
        <NodeZones node={topo.data.node} reveal={false} mine={parseCpuList(topo.data.cpuset)} />
      </div>
    </Panel>
  ) : null;

  if (info.isError) {
    const e = info.error;
    return (
      <div className="space-y-3">
        <EmptyState
          tone={e instanceof ApiFailure && e.code === "NO_TELEMETRY" ? "neutral" : "error"}
          title={e instanceof ApiFailure && e.code === "NO_TELEMETRY" ? "暂无容器信息" : "容器信息暂不可用"}
          hint={e instanceof ApiFailure ? e.body.message : String(e)}
        />
        {kore}
      </div>
    );
  }
  if (!info.data) return <EmptyState title="加载中…" />;
  const i = info.data.info;
  const snap = info.data.snapshot;
  return (
    <div className="grid gap-3 lg:grid-cols-2">
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
        <KV rows={(i.gpus ?? []).map((g) => [`GPU${g.index}`, `${g.name} · ${Math.round(g.mem_total)} MiB${g.driver ? ` · 驱动 ${g.driver}` : ""}${g.cuda ? ` · CUDA ${g.cuda}` : ""}`])} />
      </Panel>
      <Panel title="RDMA 设备">
        <KV rows={(i.rdma ?? []).map((r) => [`${r.ibdev}/${r.port}`, <Badge key="s">{r.scope === "vf_exclusive" ? "VF" : "shared"}</Badge>])} />
      </Panel>
      <Panel title="监听端口">
        <KV rows={(snap?.listen ?? []).map((l) => [`${l.proto} ${l.port}`, `${l.addr ?? ""}${l.pid ? ` · pid ${l.pid}` : ""}`])} />
      </Panel>
      <Panel title="存储">
        <KV rows={(snap?.storage ?? []).map((s) => [s.mount, `${fmtBytes(s.used)} / ${fmtBytes(s.size)}`])} />
      </Panel>
      {i.collectors && (
        <Panel title="采集器">
          <KV rows={Object.entries(i.collectors).map(([k, v]) => [k, v])} />
        </Panel>
      )}
      {kore && <div className="lg:col-span-2">{kore}</div>}
    </div>
  );
}
