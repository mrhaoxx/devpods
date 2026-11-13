// 节点 (UI spec §5.4): one row per node; the drawer (?node=) lists the
// node's DevPods, each GPU card's occupant and the Kore CPU layout.
// Reservation columns arrive with the reservation backend (ruling R1).
import { useClusterFilter } from "../../data/clusterFilter";
import type { Cluster } from "../clusters/api";
import { devpodHref, keyOf, refOfKey, type Ref } from "../../data/cluster";
import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { useQueryClient, useMutation } from "@tanstack/react-query";
import type { ColumnDef, SortingState } from "@tanstack/react-table";
import { patchDevPod, type KoreNode, type NodeLive, type NodeView } from "../../data/api";
import { useMe, useNode, useNodeChoices, useNodes, useNodesRecent } from "../../data/queries";
import { setReservable } from "../../data/api";
import { overviewStream } from "../../data/live/streams";
import { useLiveRow } from "../../data/live/hooks";
import { DataTable } from "../../design/DataTable";
import { Drawer } from "../../design/Drawer";
import { Chip } from "../../design/Chip";
import { Meter } from "../../design/Meter";
import { Sparkline } from "../../design/Sparkline";
import { sparkSeries } from "../../lib/spark";
import { Panel } from "../../design/Panel";
import { Button } from "../../design/Button";
import { Badge } from "../../design/Badge";
import { Input, Notice } from "../../design/Form";
import { EmptyState } from "../../design/EmptyState";
import { StatusDot } from "../../design/StatusDot";
import { useUrlState, type Schema } from "../../lib/urlState";
import { runBulk } from "../../lib/bulk";
import { fmtBytes, fmtCores } from "../../lib/format";
import { NodeZones, TopologyLegend } from "./KoreTopology";

const schema = {
  node: { kind: "string", default: "" },
  ready: { kind: "bool", default: false },
  gpu: { kind: "bool", default: false },
  notready: { kind: "bool", default: false },
  label: { kind: "string", default: "" },
  sort: { kind: "string", default: "name" },
} satisfies Schema;

const spec = overviewStream();

function CpuUsed({ n }: { n: Ref }) {
  const key = keyOf(n); // a node name can be in two clusters
  const { row } = useLiveRow<NodeLive>(spec, `node:${key}`);
  const recent = useNodesRecent().data; // stored history: the curve is there on arrival
  if (!row) return <span className="text-faint">—</span>;
  const hist = recent?.items[key] && { ts: recent.ts, v: recent.items[key].cpu };
  return (
    <span className="inline-flex items-center gap-1.5">
      <Sparkline
        values={sparkSeries(hist, row.history.map((h) => ({ ts: h.ts, v: h.value.cpuUsed })), Date.now())}
        max={row.value.cpuAllocatable || undefined}
        width={48}
        height={14}
      />
      <span className="mono">{fmtCores(row.value.cpuUsed)}</span>
    </span>
  );
}

function matchesLabel(labels: Record<string, string>, f: string): boolean {
  if (!f) return true;
  const [k, v] = f.split("=", 2);
  if (v === undefined) return Object.keys(labels).some((x) => x.includes(k)) || Object.values(labels).some((x) => x.includes(k));
  return Object.entries(labels).some(([lk, lv]) => (lk === k || lk.endsWith(`/${k}`)) && lv === v);
}

/** 可预约: admins open a node for reservation (the node label). Turning it
 *  off leaves reservations already made to run their course. */
function ReservableSwitch({ r }: { r: Ref }) {
  const qc = useQueryClient();
  const me = useMe().data;
  const node = useNodeChoices(!!me?.features.reservations).data?.items.find((n) => n.name === r.name && n.cluster === r.cluster);
  const set = useMutation({
    mutationFn: (on: boolean) => setReservable(r, on),
    onSettled: () => void qc.invalidateQueries({ queryKey: ["node-choices"] }),
  });
  if (!me?.features.reservations) return <span className="text-faint">—</span>;
  const on = set.isPending ? !!set.variables : !!node?.reservable;
  return (
    <span className="inline-flex items-center gap-1.5">
      <input type="checkbox" role="switch" aria-label={`${name} 可预约`} checked={on} disabled={set.isPending || !node} onChange={(e) => set.mutate(e.target.checked)} />
      {!on && !!node?.pending && <span className="text-[11px] text-warm">还有 {node.pending} 个未结束的预约</span>}
      {on && node?.held && <span className="text-[11px] text-muted">{node.held.user} 使用中</span>}
      {set.isError && <span className="text-[11px] text-fail">保存失败</span>}
    </span>
  );
}

/** withCluster: the columns with a 集群 one after the name (offline marked). */
function withCluster(cs: ColumnDef<NodeView, unknown>[], clusters: Cluster[]): ColumnDef<NodeView, unknown>[] {
  const of = new Map(clusters.map((c) => [c.name, c]));
  const col: ColumnDef<NodeView, unknown> = {
    id: "cluster",
    header: "集群",
    accessorFn: (n) => n.cluster ?? "",
    meta: { priority: 2, width: 100 },
    cell: ({ row: { original: n } }) => {
      const c = n.cluster ? of.get(n.cluster) : undefined;
      return (
        <span className="inline-flex items-center gap-1">
          {c?.displayName || n.cluster}
          {c && !c.connected && <Badge tone="warm">离线</Badge>}
        </span>
      );
    },
  };
  return [cs[0], col, ...cs.slice(1)];
}

const columns: ColumnDef<NodeView, unknown>[] = [
  {
    id: "name",
    header: "名称",
    accessorKey: "name",
    meta: { priority: 1, width: 160 },
    cell: ({ row: { original: n } }) => (
      <span className="inline-flex items-center gap-1.5">
        <StatusDot phase={n.ready ? "Running" : "NotReady"} showLabel={false} />
        <span data-testid="node-name" className="mono">
          {n.name}
        </span>
        {!n.ready && <Badge tone="idle">NotReady</Badge>}
      </span>
    ),
  },
  {
    id: "reservable",
    header: "可预约",
    enableSorting: false,
    meta: { priority: 3, width: 150 },
    cell: ({ row: { original: n } }) => <ReservableSwitch r={n} />,
  },
  {
    id: "cpu",
    header: "CPU 已分配/可分配",
    accessorFn: (n) => n.cpu.allocated / (n.cpu.allocatable || 1),
    meta: { priority: 2, width: 150 },
    cell: ({ row: { original: n } }) => <Meter used={n.cpu.allocated} limit={n.cpu.allocatable} label={`${fmtCores(n.cpu.allocated)}/${fmtCores(n.cpu.allocatable)}`} width={48} />,
  },
  { id: "used", header: "DevPod 实际 CPU", enableSorting: false, meta: { priority: 3, width: 110 }, cell: ({ row }) => <CpuUsed n={row.original} /> },
  {
    id: "mem",
    header: "内存已分配",
    accessorFn: (n) => n.mem.allocated / (n.mem.allocatable || 1),
    meta: { priority: 4, width: 150 },
    cell: ({ row: { original: n } }) => <Meter used={n.mem.allocated} limit={n.mem.allocatable} label={`${fmtBytes(n.mem.allocated)}/${fmtBytes(n.mem.allocatable)}`} width={48} />,
  },
  {
    id: "gpu",
    header: "GPU",
    accessorFn: (n) => n.gpu.total,
    meta: { priority: 3, width: 150 },
    cell: ({ row: { original: n } }) =>
      n.gpu.total ? (
        <span className="inline-flex items-center gap-1">
          {Array.from({ length: n.gpu.total }, (_, i) => (
            <span key={i} className={`inline-block h-2.5 w-2.5 rounded-sm border border-line ${i < n.gpu.used ? "bg-accent" : ""}`} />
          ))}
          <span className="mono">
            {n.gpu.used}/{n.gpu.total}
          </span>
          {n.gpu.model && <span className="truncate text-muted">{n.gpu.model}</span>}
        </span>
      ) : null,
  },
  { id: "rdma", header: "RDMA", enableSorting: false, meta: { priority: 7, width: 120, mono: true }, cell: ({ row }) => row.original.rdma.map((r) => `${r.resource.split("/").pop()}×${r.allocatable}`).join(" ") },
  { id: "devpods", header: "DevPod", accessorKey: "devpods", meta: { priority: 5, width: 60, mono: true, align: "right" } },
  {
    id: "labels",
    header: "标签",
    enableSorting: false,
    meta: { priority: 8, width: 220 },
    cell: ({ row }) => {
      const l = Object.entries(row.original.labels ?? {});
      const s = l.map(([k, v]) => `${k.split("/").pop()}=${v}`).join(" ");
      return (
        <span className="mono text-muted" title={l.map(([k, v]) => `${k}=${v}`).join("\n")}>
          {s}
        </span>
      );
    },
  },
];

function NodeDrawerBody({ r }: { r: Ref }) {
  const name = r.name;
  const q = useNode(r);
  const qc = useQueryClient();
  const [sel, setSel] = useState<Set<string>>(new Set());
  const [msg, setMsg] = useState<string | null>(null);
  if (q.isError) return <EmptyState tone="error" title="无法加载节点" hint={String(q.error)} />;
  if (!q.data) return <EmptyState title="加载中…" />;
  const { node, devpods } = q.data;
  const running = devpods.filter((d) => d.phase === "Running");
  const hibernate = async () => {
    const res = await runBulk([...sel], (n) => patchDevPod({ cluster: r.cluster, name: n }, false));
    const failed = res.filter((r) => !r.ok);
    setMsg(`休眠：成功 ${res.length - failed.length}，失败 ${failed.length}${failed.length ? `（${failed.map((f) => `${f.item}：${f.error}`).join("；")}）` : ""}`);
    setSel(new Set());
    void qc.invalidateQueries({ queryKey: ["admin", "node"] });
    void qc.invalidateQueries({ queryKey: ["devpods"] });
  };
  return (
    <div className="space-y-3">
      <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs">
        <span>
          CPU <span className="mono">{fmtCores(node.cpu.allocated)}/{fmtCores(node.cpu.allocatable)}</span>
        </span>
        <span>
          内存 <span className="mono">{fmtBytes(node.mem.allocated)}/{fmtBytes(node.mem.allocatable)}</span>
        </span>
        {node.gpu.total > 0 && (
          <span>
            GPU <span className="mono">{node.gpu.used}/{node.gpu.total}</span> {node.gpu.model}
          </span>
        )}
      </div>
      <Panel
        title={`DevPod（${devpods.length}）`}
        actions={
          sel.size > 0 && (
            <Button variant="danger" onClick={() => void hibernate()}>
              休眠所选（{sel.size}）
            </Button>
          )
        }
      >
        {msg && (
          <div className="p-2">
            <Notice tone="run">{msg}</Notice>
          </div>
        )}
        {devpods.length === 0 ? (
          <EmptyState title="这台节点上没有 DevPod" />
        ) : (
          <ul className="divide-y divide-line text-xs">
            {devpods.map((d) => (
              <li key={d.name} className="flex items-center gap-2 px-3 py-1">
                <input
                  type="checkbox"
                  aria-label={`选择 ${d.name}`}
                  disabled={d.phase !== "Running"}
                  checked={sel.has(d.name)}
                  onChange={(e) => {
                    const n = new Set(sel);
                    if (e.target.checked) n.add(d.name);
                    else n.delete(d.name);
                    setSel(n);
                  }}
                />
                <StatusDot phase={d.phase} showLabel={false} />
                <Link to={devpodHref({ cluster: r.cluster, name: d.name })} className="mono min-w-0 flex-1 truncate hover:text-accent">
                  {d.name}
                </Link>
                <span className="mono text-muted">{d.owner}</span>
                <span className="mono w-12 text-right">{d.cpu == null ? "—" : fmtCores(d.cpu)}</span>
                <span className="mono w-14 text-right">{d.mem == null ? "—" : fmtBytes(d.mem)}</span>
                {d.gpu > 0 && <Badge>GPU×{d.gpu}</Badge>}
              </li>
            ))}
          </ul>
        )}
        {running.length === 0 && devpods.length > 0 && <div className="px-3 pb-2 text-[11px] text-faint">没有运行中的 DevPod 可休眠</div>}
      </Panel>
      {node.gpu.total > 0 && (
        <Panel title="GPU 占用">
          <ul className="divide-y divide-line text-xs">
            {node.gpu.cards.length === 0 && <li className="px-3 py-1.5 text-muted">没有卡级数据（需要遥测）</li>}
            {node.gpu.cards.map((c) => (
              <li key={c.uuid || c.index} className="flex items-center gap-2 px-3 py-1">
                <span className="mono w-12">GPU{c.index}</span>
                {c.devpod ? (
                  <Link to={devpodHref({ cluster: r.cluster, name: c.devpod })} className="mono min-w-0 flex-1 truncate hover:text-accent">
                    {c.devpod}
                  </Link>
                ) : (
                  <span className="flex-1 text-muted">空闲</span>
                )}
                {c.owner && <span className="mono text-muted">{c.owner}</span>}
                <span className="mono w-10 text-right">{Math.round(c.util)}%</span>
              </li>
            ))}
          </ul>
        </Panel>
      )}
      {q.data.topology ? (
        <Panel title="CPU 拓扑（Kore）" actions={<TopologyLegend />}>
          <div className="p-3">
            <NodeZones cluster={r.cluster} node={q.data.topology as KoreNode} reveal />
          </div>
        </Panel>
      ) : null}
    </div>
  );
}

export function NodesPage() {
  const q = useNodes();
  const [url, setUrl] = useUrlState(schema, { key: "nodes", omit: ["node"] });
  const { matches, several, clusters } = useClusterFilter();
  const items = useMemo(() => (q.data?.items ?? []).filter(matches), [q.data, matches]);
  const cols = useMemo(() => (several ? withCluster(columns, clusters) : columns), [several, clusters]);
  const shown = useMemo(
    () =>
      items.filter(
        (n) => (!url.ready || n.ready) && (!url.notready || !n.ready) && (!url.gpu || n.gpu.total > 0) && matchesLabel(n.labels ?? {}, url.label),
      ),
    [items, url.ready, url.notready, url.gpu, url.label],
  );
  const sorting: SortingState = url.sort ? [{ id: url.sort.replace(/^-/, ""), desc: url.sort.startsWith("-") }] : [];
  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex flex-wrap items-center gap-2 border-b border-line px-3 py-2">
        <Chip active={url.ready} count={items.filter((n) => n.ready).length} onClick={() => setUrl({ ready: !url.ready, notready: false })}>
          可调度
        </Chip>
        <Chip active={url.gpu} count={items.filter((n) => n.gpu.total > 0).length} onClick={() => setUrl({ gpu: !url.gpu })}>
          有 GPU
        </Chip>
        <Chip active={url.notready} count={items.filter((n) => !n.ready).length} onClick={() => setUrl({ notready: !url.notready, ready: false })}>
          NotReady
        </Chip>
        <Input aria-label="按标签过滤" placeholder="标签 key=value" value={url.label} onChange={(e) => setUrl({ label: e.target.value })} className="mono !h-7 w-56 text-xs" />
      </div>
      {q.isError ? (
        <EmptyState tone="error" title="无法加载节点" hint={String(q.error)} />
      ) : (
        <DataTable<NodeView> fill
          storageKey="nodes"
          data={shown}
          columns={cols}
          getRowId={keyOf}
          sorting={sorting}
          onSortingChange={(s) => setUrl({ sort: s[0] ? `${s[0].desc ? "-" : ""}${s[0].id}` : "" })}
          onRowClick={(n) => setUrl({ node: keyOf(n) }, "push")}
          empty={<EmptyState title={q.isLoading ? "加载中…" : "没有符合条件的节点"} />}
        />
      )}
      {url.node && (
        <Drawer open onOpenChange={(o) => !o && setUrl({ node: "" }, "back")} title={`节点 ${refOfKey(url.node).name}`} width={620}>
          <NodeDrawerBody r={refOfKey(url.node)} />
        </Drawer>
      )}
    </div>
  );
}
