// The DevPod table's columns (UI spec §5.1). Metric cells subscribe to
// their own row of the live stream, so a per-second update re-renders
// only them.
import { createContext, useContext } from "react";
import { Link } from "react-router-dom";
import type { ColumnDef } from "@tanstack/react-table";
import type { DevPod, LiveItem, Me, PodInfo, RecentFrame } from "../../data/api";
import { devpodsStream } from "../../data/live/streams";
import { useLiveRow } from "../../data/live/hooks";
import { MetricCell } from "../../design/MetricCell";
import { StatusDot, phaseLabel } from "../../design/StatusDot";
import { Badge } from "../../design/Badge";
import { fmtAge, fmtBytes, fmtCores, fmtRate, parseQuantity } from "../../lib/format";
import { phaseSummary, reservationNotes } from "./status";
import { sparkSeries } from "../../lib/spark";
import { provenance } from "./provenance";
import type { UsageKey } from "./usage";

export type Row = {
  dp: DevPod;
  pod?: PodInfo;
  name: string;
  owner: string;
  phase: string;
  node: string;
  image: string;
  gpu: number;
  cpuLimit: number;
  memLimit: number;
  /** a group of DevPods launched together, shown as this one row (groups.ts) */
  group?: { id: string; members: Row[] };
};

export function toRow(dp: DevPod, pod?: PodInfo): Row {
  const c = dp.spec.pod?.spec?.containers?.[0];
  const lim = c?.resources?.limits ?? {};
  return {
    dp,
    pod,
    name: dp.metadata.name,
    owner: dp.spec.owner,
    phase: dp.status?.phase ?? (dp.spec.running ? "Pending" : "Stopped"),
    node: pod?.node ?? "",
    image: c?.image ?? "",
    gpu: parseQuantity(lim["nvidia.com/gpu"]),
    cpuLimit: parseQuantity(lim.cpu),
    memLimit: parseQuantity(lim.memory),
  };
}

/** Shared by the cells: scope of the live stream and a coarse clock. */
export const TableCtx = createContext<{ scope: "mine" | "all"; now: Date; recent?: RecentFrame }>({ scope: "mine", now: new Date() });

function useLive(name: string) {
  const { scope } = useContext(TableCtx);
  return useLiveRow<LiveItem>(devpodsStream(scope), name).row;
}

const running = (r: Row) => r.phase === "Running";

function CpuCell({ r }: { r: Row }) {
  const { now, recent } = useContext(TableCtx);
  const live = useLive(r.name);
  if (!running(r)) {
    const s = phaseSummary(r.dp, r.pod, now) ?? "";
    return (
      <span className={r.phase === "Failed" ? "text-fail" : "text-muted"} title={s}>
        {s}
      </span>
    );
  }
  const v = live?.value.cpu ?? null;
  const limit = live?.value.cpuLimit || r.cpuLimit;
  return (
    <MetricCell
      value={v}
      text={v == null ? undefined : `${fmtCores(v)}/${fmtCores(limit)}`}
      spark={sparkSeries(
        recent?.items[r.name] && { ts: recent.ts, v: recent.items[r.name].cpu },
        (live?.history ?? []).map((h) => ({ ts: h.ts, v: h.value.cpu })),
        Date.now(),
      )}
      sparkMax={limit || undefined}
    />
  );
}

function MemCell({ r }: { r: Row }) {
  const live = useLive(r.name);
  if (!running(r)) return null;
  const v = live?.value.mem ?? null;
  const limit = live?.value.memLimit || r.memLimit;
  return <MetricCell value={v} meter={v == null ? undefined : { used: v, limit }} text={v == null ? undefined : `${fmtBytes(v)}/${fmtBytes(limit)}`} />;
}

function GpuCell({ r }: { r: Row }) {
  const live = useLive(r.name);
  if (!running(r)) return null;
  const cards = live?.value.gpu ?? [];
  if (cards.length === 0) return r.gpu > 0 ? <MetricCell value={null} /> : null;
  const avg = cards.reduce((a, c) => a + c.util, 0) / cards.length;
  return (
    <span className="inline-flex items-center gap-1">
      {cards.map((c) => (
        <span
          key={c.uuid || c.index}
          title={`GPU${c.index} ${Math.round(c.util)}% · ${Math.round(c.memUsed)}/${Math.round(c.memTotal)} MiB`}
          className="inline-block h-2.5 w-2.5 rounded-sm border border-line"
          style={{ background: `color-mix(in srgb, var(--color-run) ${Math.round(c.util)}%, transparent)` }}
        />
      ))}
      <span className="mono">{Math.round(avg)}%</span>
    </span>
  );
}

function SessionsCell({ r }: { r: Row }) {
  const live = useLive(r.name);
  const users = [...new Set((live?.value.sessions ?? []).map((s) => s.user).filter(Boolean))];
  if (!running(r) || users.length === 0) return null;
  return (
    <span title={users.join(", ")}>
      <span className="mono">{live!.value.sessions.length}</span> <span className="text-muted">{users.join(", ")}</span>
    </span>
  );
}

function NetCell({ r }: { r: Row }) {
  const live = useLive(r.name);
  if (!running(r)) return null;
  const rx = live?.value.netRx ?? null;
  return (
    <span className="inline-flex items-center gap-1">
      <MetricCell value={rx} text={rx == null ? undefined : `↓${fmtRate(rx)} ↑${fmtRate(live!.value.netTx ?? 0)}`} />
      {live?.value.netKind === "tcp-only" && <Badge title="hostNetwork：只统计 TCP">TCP only</Badge>}
    </span>
  );
}

function RdmaCell({ r }: { r: Row }) {
  const live = useLive(r.name);
  if (!running(r)) return null;
  const kind = live?.value.rdmaKind;
  if (!kind && live) return null; // no RDMA device
  const rx = live?.value.rdmaRx ?? null;
  return (
    <span className="inline-flex items-center gap-1">
      <MetricCell value={rx} text={rx == null ? undefined : `↓${fmtRate(rx)} ↑${fmtRate(live!.value.rdmaTx ?? 0)}`} />
      {kind && <Badge>{kind === "vf" ? "VF" : "shared"}</Badge>}
    </span>
  );
}

function UptimeCell({ r }: { r: Row }) {
  const { now } = useContext(TableCtx);
  if (!running(r) || !r.pod?.startedAt) return null;
  return <span className="mono">{fmtAge(r.pod.startedAt, now)}</span>;
}

function GroupName({ r, open, onToggle }: { r: Row; open: boolean; onToggle: (id: string) => void }) {
  const ms = r.group!.members;
  const up = ms.filter((m) => m.phase === "Running").length;
  return (
    <span className="inline-flex min-w-0 items-center gap-1.5">
      <button type="button" aria-label={`${open ? "收起" : "展开"} ${r.name}`} aria-expanded={open} onClick={() => onToggle(r.name)} className="w-3 text-muted hover:text-ink">
        {open ? "▾" : "▸"}
      </button>
      <span title={phaseLabel(r.phase)}>
        <StatusDot phase={r.phase} showLabel={false} />
      </span>
      <span className="mono truncate">{`${r.name} ×${ms.length}`}</span>
      <span className="text-[11px] text-muted">{`${up}/${ms.length} 运行中`}</span>
    </span>
  );
}

/** a cell of a DevPod's own; a group row leaves it empty */
const own = (cell: (r: Row) => React.ReactNode) => ({ row }: { row: { original: Row } }) => (row.original.group ? null : cell(row.original));

export function devpodColumns(opts: {
  admin: boolean;
  me?: Me;
  actions: (r: Row) => React.ReactNode;
  groups?: { open: ReadonlySet<string>; toggle: (id: string) => void };
  /** usage: what a row uses now (undefined: not known), to sort by */
  usage?: (r: Row, key: UsageKey) => number | undefined;
}): ColumnDef<Row, unknown>[] {
  // a usage column: most first on the first click, rows with nothing known last
  const by = (key: UsageKey) => ({ accessorFn: (r: Row) => opts.usage?.(r, key), sortDescFirst: true, sortUndefined: "last" as const });
  const cols: ColumnDef<Row, unknown>[] = [
    {
      id: "name",
      header: "名称",
      accessorFn: (r) => r.name,
      meta: { priority: 1, width: 190 },
      cell: ({ row: { original: r } }) =>
        r.group ? (
          <GroupName r={r} open={!!opts.groups?.open.has(r.name)} onToggle={(id) => opts.groups?.toggle(id)} />
        ) : (
        <span className={`inline-flex min-w-0 items-center gap-1.5 ${provenance(r.dp).group && opts.groups?.open.has(provenance(r.dp).group!) ? "pl-4" : ""}`}>
          <span title={phaseLabel(r.phase)}>
            <StatusDot phase={r.phase} showLabel={false} />
          </span>
          <Link to={`/devpods/${r.name}`} className="mono truncate hover:text-accent">
            {r.name}
          </Link>
          {r.phase !== "Running" && <span className="text-[11px] text-muted">{phaseLabel(r.phase)}</span>}
          {reservationNotes(r.dp).map((n) => (
            <Badge key={n.type} tone={n.tone} title={n.message}>
              {n.label}
            </Badge>
          ))}
        </span>
      ),
    },
  ];
  if (opts.admin) cols.push({ id: "owner", header: "Owner", accessorFn: (r) => r.owner, meta: { priority: 2, width: 90, mono: true } });
  cols.push(
    { id: "cpu", header: "CPU", ...by("cpu"), meta: { priority: 2, width: 140 }, cell: own((r) => <CpuCell r={r} />) },
    { id: "mem", header: "内存", ...by("mem"), meta: { priority: 3, width: 140 }, cell: own((r) => <MemCell r={r} />) },
    {
      id: "gpu",
      header: "GPU",
      ...by("gpu"),
      meta: { priority: 4, width: 110 },
      cell: ({ row }) => (row.original.group ? row.original.gpu > 0 ? <span className="mono">{row.original.gpu}</span> : null : <GpuCell r={row.original} />),
    },
    { id: "sessions", header: "会话", enableSorting: false, meta: { priority: 5, width: 120 }, cell: own((r) => <SessionsCell r={r} />) },
    {
      id: "uptime",
      header: "运行时长",
      accessorFn: (r) => (r.phase === "Running" && r.pod?.startedAt ? Date.parse(r.pod.startedAt) : Infinity),
      sortDescFirst: false,
      meta: { priority: 6, width: 80 },
      cell: own((r) => <UptimeCell r={r} />),
    },
    { id: "net", header: "网络 ↓/↑", ...by("net"), meta: { priority: 7, width: 150 }, cell: own((r) => <NetCell r={r} />) },
    { id: "rdma", header: "RDMA ↓/↑", ...by("rdma"), meta: { priority: 8, width: 150 }, cell: own((r) => <RdmaCell r={r} />) },
    { id: "node", header: "节点", accessorFn: (r) => r.node, meta: { priority: 9, width: 90, mono: true } },
    { id: "image", header: "镜像", accessorFn: (r) => r.image, meta: { priority: 10, width: 170, mono: true }, cell: ({ getValue }) => <span title={String(getValue())}>{String(getValue())}</span> },
    { id: "actions", header: "", enableSorting: false, meta: { priority: 1, width: 36, align: "right" }, cell: ({ row }) => opts.actions(row.original) },
  );
  return cols;
}
