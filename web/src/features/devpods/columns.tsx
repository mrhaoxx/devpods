// The DevPod table's columns (UI spec §5.1). Metric cells subscribe to
// their own row of the live stream, so a per-second update re-renders
// only them.
import type { Cluster } from "../clusters/api";
import { devpodHref, keyOf } from "../../data/cluster";
import { createContext, useContext } from "react";
import { Spinner } from "../../design/Spinner";
import { Link } from "react-router-dom";
import type { ColumnDef } from "@tanstack/react-table";
import type { DevPod, LiveItem, Me, PodInfo, RecentFrame } from "../../data/api";
import { devpodsStream } from "../../data/live/streams";
import { useLiveRows } from "../../data/live/hooks";
import { MetricCell } from "../../design/MetricCell";
import { StatusDot, phaseLabel } from "../../design/StatusDot";
import { Badge } from "../../design/Badge";
import { fmtAge, fmtBytes, fmtCores, fmtRate, parseQuantity, gpuCount } from "../../lib/format";
import { phaseSummary, pinnedNode, reservationNotes, type ReservationsNow } from "./status";
import { sparkSeries } from "../../lib/spark";
import { provenance } from "./provenance";
import type { UsageKey } from "./usage";

export type Row = {
  /** the cluster it is in (absent in a console of one) */
  cluster?: string;
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
  group?: { id: string; key: string; members: Row[] };
  /** a member shown under its group's row */
  inGroup?: boolean;
};

export function toRow(dp: DevPod, pod?: PodInfo): Row {
  const c = dp.spec.pod?.spec?.containers?.[0];
  const lim = c?.resources?.limits ?? {};
  return {
    ...(dp.cluster ? { cluster: dp.cluster } : {}),
    dp,
    pod,
    name: dp.metadata.name,
    owner: dp.spec.owner,
    phase: dp.status?.phase ?? (dp.spec.running ? "Pending" : "Stopped"),
    // where its Pod is — or, with none (hibernated, not placed yet), where it is pinned
    node: pod?.node || pinnedNode(dp),
    image: c?.image ?? "",
    gpu: gpuCount(lim),
    cpuLimit: parseQuantity(lim.cpu),
    memLimit: parseQuantity(lim.memory),
  };
}

/** Shared by the cells: scope of the live stream and a coarse clock. */
/** Pending: what is being done to a DevPod (label, e.g. 启动), or that it failed and why. */
export type Pending = { label: string; error?: string };

/** ReservationBadges: what node reservations mean for the row's DevPod. */
function ReservationBadges({ r }: { r: Row }) {
  const rs = useContext(TableCtx).reservations;
  return (
    <>
      {reservationNotes(r.dp, { node: r.pod?.node, cluster: r.cluster }, rs).map((n) => (
        <Badge key={n.type} tone={n.tone} title={n.message}>
          {n.label}
        </Badge>
      ))}
    </>
  );
}

export const TableCtx = createContext<{ scope: "mine" | "all"; now: Date; recent?: RecentFrame; pending?: ReadonlyMap<string, Pending>; reservations?: ReservationsNow }>({
  scope: "mine",
  now: new Date(),
});

/** PhaseNote: the row's phase — or what is being done to it, until it is done. */
function PhaseNote({ r }: { r: Row }) {
  const p = useContext(TableCtx).pending?.get(keyOf(r));
  if (p?.error)
    return (
      <span className="shrink-0 text-[11px] text-fail" title={p.error}>
        {p.label}失败
      </span>
    );
  if (p)
    return (
      <span className="inline-flex shrink-0 items-center gap-1 text-[11px] text-accent">
        <Spinner />
        {p.label}中…
      </span>
    );
  return r.phase !== "Running" ? <span className="text-[11px] text-muted">{phaseLabel(r.phase)}</span> : null;
}

const running = (r: Row) => r.phase === "Running";

/** useUp: the running DevPods a row's metrics are of — its own, or a
 *  group's members (their totals) — with their live values, keyed
 *  <cluster>/<name> (keyOf) as the stream keys them. */
function useUp(r: Row) {
  const { scope } = useContext(TableCtx);
  const rs = r.group?.members ?? [r];
  const lives = useLiveRows<LiveItem>(devpodsStream(scope), rs.map(keyOf));
  return rs.map((m, i) => ({ m, live: lives[i] })).filter((x) => running(x.m));
}
/** total: the sum of what is known (null: nothing is) */
const total = (xs: (number | null | undefined)[]) => {
  const known = xs.filter((x): x is number => x != null);
  return known.length ? known.reduce((a, b) => a + b, 0) : null;
};

function CpuCell({ r }: { r: Row }) {
  const { now, recent } = useContext(TableCtx);
  const up = useUp(r);
  if (!r.group && !running(r)) {
    const s = phaseSummary(r.dp, r.pod, now) ?? "";
    return (
      <span className={r.phase === "Failed" ? "text-fail" : "text-muted"} title={s}>
        {s}
      </span>
    );
  }
  if (up.length === 0) return null;
  const v = total(up.map((x) => x.live?.value.cpu));
  const limit = up.reduce((a, x) => a + (x.live?.value.cpuLimit || x.m.cpuLimit), 0);
  const t = Date.now();
  // the members' curves added bucket by bucket (each is bucketed over the same window)
  const curves = up.map((x) =>
    sparkSeries(
      recent?.items[keyOf(x.m)] && { ts: recent.ts, v: recent.items[keyOf(x.m)].cpu },
      (x.live?.history ?? []).map((h) => ({ ts: h.ts, v: h.value.cpu })),
      t,
    ),
  );
  return (
    <MetricCell
      value={v}
      text={v == null ? undefined : `${fmtCores(v)}/${fmtCores(limit)}`}
      spark={curves[0].map((_, i) => total(curves.map((c) => c[i])))}
      sparkMax={limit || undefined}
    />
  );
}

function MemCell({ r }: { r: Row }) {
  const up = useUp(r);
  if (up.length === 0) return null;
  const v = total(up.map((x) => x.live?.value.mem));
  const limit = up.reduce((a, x) => a + (x.live?.value.memLimit || x.m.memLimit), 0);
  return <MetricCell value={v} meter={v == null ? undefined : { used: v, limit }} text={v == null ? undefined : `${fmtBytes(v)}/${fmtBytes(limit)}`} />;
}

const MiB = 1024 * 1024;

/** GpuMemCell: GPU memory in use over every card, of their total. */
function GpuMemCell({ r }: { r: Row }) {
  const up = useUp(r);
  if (up.length === 0) return null;
  const cards = up.flatMap((x) => x.live?.value.gpu ?? []);
  if (cards.length === 0) return up.some((x) => x.m.gpu > 0) ? <MetricCell value={null} /> : null;
  const used = cards.reduce((a, c) => a + c.memUsed, 0) * MiB;
  const all = cards.reduce((a, c) => a + c.memTotal, 0) * MiB;
  return <MetricCell value={used} meter={{ used, limit: all }} text={`${fmtBytes(used)}/${fmtBytes(all)}`} />;
}

function GpuCell({ r }: { r: Row }) {
  const up = useUp(r);
  if (up.length === 0) return null;
  const cards = up.flatMap((x) => x.live?.value.gpu ?? []);
  if (cards.length === 0) return up.some((x) => x.m.gpu > 0) ? <MetricCell value={null} /> : null;
  const avg = cards.reduce((a, c) => a + c.util, 0) / cards.length;
  return (
    <span className="inline-flex items-center gap-1">
      {cards.map((c, i) => (
        <span
          key={`${i}:${c.uuid || c.index}`}
          title={`GPU${c.index} ${Math.round(c.util)}% · ${Math.round(c.memUsed)}/${Math.round(c.memTotal)} MiB${c.power != null ? ` · ${Math.round(c.power)} W` : ""}`}
          className="inline-block h-2.5 w-2.5 rounded-sm border border-line"
          style={{ background: `color-mix(in srgb, var(--color-run) ${Math.round(c.util)}%, transparent)` }}
        />
      ))}
      <span className="mono">{Math.round(avg)}%</span>
    </span>
  );
}

function SessionsCell({ r }: { r: Row }) {
  const up = useUp(r);
  const sessions = up.flatMap((x) => x.live?.value.sessions ?? []);
  const users = [...new Set(sessions.map((s) => s.user).filter(Boolean))];
  if (users.length === 0) return null;
  return (
    <span title={users.join(", ")}>
      <span className="mono">{sessions.length}</span> <span className="text-muted">{users.join(", ")}</span>
    </span>
  );
}

function NetCell({ r }: { r: Row }) {
  const up = useUp(r);
  if (up.length === 0) return null;
  const rx = total(up.map((x) => x.live?.value.netRx));
  return (
    <span className="inline-flex items-center gap-1">
      <MetricCell value={rx} text={rx == null ? undefined : `↓${fmtRate(rx)} ↑${fmtRate(total(up.map((x) => x.live?.value.netTx)) ?? 0)}`} />
      {up.some((x) => x.live?.value.netKind === "tcp-only") && <Badge title="hostNetwork：只统计 TCP">TCP only</Badge>}
    </span>
  );
}

function RdmaCell({ r }: { r: Row }) {
  const up = useUp(r);
  if (up.length === 0) return null;
  const kind = up.map((x) => x.live?.value.rdmaKind).find(Boolean);
  if (!kind && up.every((x) => x.live)) return null; // no RDMA device
  const rx = total(up.map((x) => x.live?.value.rdmaRx));
  return (
    <span className="inline-flex items-center gap-1">
      <MetricCell value={rx} text={rx == null ? undefined : `↓${fmtRate(rx)} ↑${fmtRate(total(up.map((x) => x.live?.value.rdmaTx)) ?? 0)}`} />
      {kind && <Badge>{kind === "vf" ? "VF" : "shared"}</Badge>}
    </span>
  );
}

function UptimeCell({ r }: { r: Row }) {
  const { now } = useContext(TableCtx);
  if (running(r)) return r.pod?.startedAt ? <span className="mono">{fmtAge(r.pod.startedAt, now)}</span> : null;
  // asleep: for how long (what the column sorts the asleep by)
  const h = r.dp.status?.hibernatedAt;
  if (!r.dp.spec.running && h) {
    return (
      <span className="text-muted" title={`${new Date(h).toLocaleString()} 休眠`}>
        休眠 {fmtAge(h, now)}
      </span>
    );
  }
  return null;
}

// 运行时长's order, as one number (so the header's two directions are the
// two orders): the running, the longest up first; then those asked to run
// that are not running yet (pending, failing), which join the running as
// they come up; then the asleep, the latest asleep first, those with no
// time known last. A group goes where its best-placed member does.
const BAND = 1e15;
export function uptimeKey(r: Row): number {
  if (r.group) return Math.min(...r.group.members.map(uptimeKey));
  if (running(r)) {
    const t = r.pod?.startedAt ? Date.parse(r.pod.startedAt) : NaN;
    return Number.isNaN(t) ? BAND - 1 : t;
  }
  if (r.dp.spec.running) return BAND;
  const h = r.dp.status?.hibernatedAt ? Date.parse(r.dp.status.hibernatedAt) : NaN;
  return 2 * BAND + (Number.isNaN(h) ? 4e12 : 4e12 - h);
}

function GroupName({ r, open, onToggle }: { r: Row; open: boolean; onToggle: (key: string) => void }) {
  const ms = r.group!.members;
  const up = ms.filter((m) => m.phase === "Running").length;
  const pending = useContext(TableCtx).pending;
  const busy = ms.filter((m) => {
    const p = pending?.get(keyOf(m));
    return p && !p.error;
  }).length;
  return (
    <span className="inline-flex min-w-0 items-center gap-1.5">
      <button type="button" aria-label={`${open ? "收起" : "展开"} ${r.name}`} aria-expanded={open} onClick={() => onToggle(r.group!.key)} className="w-3 text-muted hover:text-ink">
        {open ? "▾" : "▸"}
      </button>
      <span title={phaseLabel(r.phase)}>
        <StatusDot phase={r.phase} showLabel={false} />
      </span>
      <span className="mono truncate">{`${r.name} ×${ms.length}`}</span>
      <span className="text-[11px] text-muted">{`${up}/${ms.length} 运行中`}</span>
      {busy > 0 && <span className="text-[11px] text-accent">{`${busy} 个进行中…`}</span>}
    </span>
  );
}

/** a cell of a DevPod's own; a group row leaves it empty */
const own = (cell: (r: Row) => React.ReactNode) => ({ row }: { row: { original: Row } }) => (row.original.group ? null : cell(row.original));

/** sharedWithMe: in my list, a DevPod someone else owns (I am a collaborator). */
export const sharedWithMe = (r: { owner: string }, o: { admin: boolean; me?: Me }) => !o.admin && !!o.me && r.owner !== o.me.user;

export function devpodColumns(opts: {
  admin: boolean;
  me?: Me;
  actions: (r: Row) => React.ReactNode;
  /** groups: those folded (by key), and how to fold or open one */
  groups?: { folded: ReadonlySet<string>; toggle: (key: string) => void };
  /** usage: what a row uses now (undefined: not known), to sort by */
  usage?: (r: Row, key: UsageKey) => number | undefined;
  /** the clusters, in a console of several: a 集群 column */
  clusters?: Cluster[];
}): ColumnDef<Row, unknown>[] {
  // a usage column: most first on the first click, rows with nothing known last
  const by = (key: UsageKey) => ({ accessorFn: (r: Row) => opts.usage?.(r, key), sortDescFirst: true, sortUndefined: "last" as const });
  // a metric: a DevPod's own, or a folded group's totals (open, its members show theirs)
  const folded = (r: Row) => !!r.group && !!opts.groups?.folded.has(r.group.key);
  const metric = (cell: (r: Row) => React.ReactNode) => ({ row }: { row: { original: Row } }) =>
    row.original.group && !folded(row.original) ? null : cell(row.original);
  const cols: ColumnDef<Row, unknown>[] = [
    {
      id: "name",
      header: "名称",
      accessorFn: (r) => r.name,
      meta: { priority: 1, width: 190, pin: "left" },
      cell: ({ row: { original: r } }) =>
        r.group ? (
          <GroupName r={r} open={!folded(r)} onToggle={(key) => opts.groups?.toggle(key)} />
        ) : (
        <span className={`inline-flex min-w-0 items-center gap-1.5 ${r.inGroup ? "pl-4" : ""}`}>
          <span title={phaseLabel(r.phase)}>
            <StatusDot phase={r.phase} showLabel={false} />
          </span>
          {sharedWithMe(r, opts) ? (
            <>
              <span className="mono truncate">{r.name}</span>
              <Badge title="别人共享给你的 DevPod：可以 SSH 和打开终端，不能管理">共享自 {r.owner}</Badge>
            </>
          ) : (
            <Link to={devpodHref(r)} className="mono truncate hover:text-accent">
              {r.name}
            </Link>
          )}
          <PhaseNote r={r} />
          <ReservationBadges r={r} />
        </span>
      ),
    },
  ];
  if (opts.clusters) {
    const of = new Map(opts.clusters.map((c) => [c.name, c]));
    cols.push({
      id: "cluster",
      header: "集群",
      accessorFn: (r) => r.cluster ?? "",
      meta: { priority: 2, width: 100 },
      cell: ({ row: { original: r } }) => {
        const c = r.cluster ? of.get(r.cluster) : undefined;
        return (
          <span className="inline-flex min-w-0 items-center gap-1">
            <span className="truncate">{c?.displayName || r.cluster}</span>
            {c && !c.connected && <Badge tone="warm" title="这个集群离线：显示的是它最后的状态，修改会被拒绝">离线</Badge>}
          </span>
        );
      },
    });
  }
  if (opts.admin) cols.push({ id: "owner", header: "Owner", accessorFn: (r) => r.owner, meta: { priority: 2, width: 90, mono: true } });
  cols.push(
    { id: "cpu", header: "CPU", ...by("cpu"), meta: { priority: 2, width: 140 }, cell: metric((r) => <CpuCell r={r} />) },
    { id: "mem", header: "内存", ...by("mem"), meta: { priority: 3, width: 140 }, cell: metric((r) => <MemCell r={r} />) },
    {
      id: "gpu",
      header: "GPU",
      ...by("gpu"),
      meta: { priority: 4, width: 110 },
      cell: ({ row: { original: r } }) => (r.group && !folded(r) ? r.gpu > 0 ? <span className="mono">{r.gpu}</span> : null : <GpuCell r={r} />),
    },
    { id: "gpumem", header: "显存", ...by("gpumem"), meta: { priority: 4, width: 130 }, cell: metric((r) => <GpuMemCell r={r} />) },
    { id: "sessions", header: "会话", enableSorting: false, meta: { priority: 5, width: 120 }, cell: metric((r) => <SessionsCell r={r} />) },
    {
      id: "uptime",
      header: "运行时长",
      accessorFn: uptimeKey,
      sortDescFirst: false,
      meta: { priority: 6, width: 96 },
      cell: own((r) => <UptimeCell r={r} />),
    },
    { id: "net", header: "网络 ↓/↑", ...by("net"), meta: { priority: 7, width: 150 }, cell: metric((r) => <NetCell r={r} />) },
    { id: "rdma", header: "RDMA ↓/↑", ...by("rdma"), meta: { priority: 8, width: 150 }, cell: metric((r) => <RdmaCell r={r} />) },
    { id: "node", header: "节点", accessorFn: (r) => r.node, meta: { priority: 9, width: 90, mono: true } },
    { id: "image", header: "镜像", accessorFn: (r) => r.image, meta: { priority: 10, width: 170, mono: true }, cell: ({ getValue }) => <span title={String(getValue())}>{String(getValue())}</span> },
    { id: "actions", header: "", enableSorting: false, meta: { priority: 1, width: 48, align: "right", pin: "right" }, cell: ({ row }) => opts.actions(row.original) },
  );
  return cols;
}
