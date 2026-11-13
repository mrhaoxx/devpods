// Processes (UI spec §5.2): the top 10 on 概览, every process on 进程;
// other users' command lines arrive redacted from the server. The full
// list scrolls in a box of its own and keeps its order for a few seconds
// (the values still change every second), so reading it is not chasing
// rows; 树状 shows each process under its parent.
import { createContext, useContext, useEffect, useMemo, useRef, useState } from "react";
import type { ColumnDef, SortingState } from "@tanstack/react-table";
import type { ProcLive } from "../../../data/api";
import { DataTable } from "../../../design/DataTable";
import { Badge } from "../../../design/Badge";
import { EmptyState } from "../../../design/EmptyState";
import { Button } from "../../../design/Button";
import { fmtBytes, fmtRate } from "../../../lib/format";
import { procTree } from "./procTree";

const idOf = (p: ProcLive) => `${p.pid}:${p.start}`;
/** Depths: in the tree, how deep each process is (absent: a flat list). */
const Depths = createContext<ReadonlyMap<string, number> | undefined>(undefined);
function Cmd({ p, text }: { p: ProcLive; text: string }) {
  const depth = useContext(Depths)?.get(idOf(p)) ?? 0;
  return (
    <span title={text} style={depth ? { paddingLeft: depth * 14 } : undefined}>
      {depth > 0 && <span aria-hidden="true" className="text-faint">└ </span>}
      {text}
    </span>
  );
}

const howLabel: Record<string, string> = {
  tree: "exact",
  ssh_connection: "SSH_CONNECTION",
  sid_tty: "tmux",
  inferred: "推断",
  workload: "workload",
  system: "system",
  untracked: "untracked",
  unattributed: "未归属",
};

const columns: ColumnDef<ProcLive, unknown>[] = [
  { id: "pid", header: "PID", accessorKey: "pid", meta: { priority: 1, width: 60, mono: true } },
  {
    id: "cmd",
    header: "命令",
    accessorFn: (p) => p.cmdline || p.exe,
    meta: { priority: 1, width: 300, mono: true },
    cell: ({ getValue, row }) => <Cmd p={row.original} text={String(getValue())} />,
  },
  {
    id: "user",
    header: "会话归属",
    accessorFn: (p) => p.user ?? "",
    meta: { priority: 3, width: 150 },
    cell: ({ row: { original: p } }) =>
      p.user || p.how ? (
        <span className="inline-flex items-center gap-1">
          {p.user && <span className="mono">{p.user}</span>}
          {p.how && <Badge title="归属依据">{howLabel[p.how] ?? p.how}</Badge>}
        </span>
      ) : null,
  },
  { id: "cpu", header: "CPU", accessorKey: "cpu", sortDescFirst: true, meta: { priority: 2, width: 60, mono: true, align: "right" }, cell: ({ getValue }) => (getValue() as number).toFixed(2) },
  { id: "rss", header: "RSS", accessorKey: "rss", sortDescFirst: true, meta: { priority: 2, width: 70, mono: true, align: "right" }, cell: ({ getValue }) => fmtBytes(getValue() as number) },
  {
    id: "io",
    header: "IO 读/写",
    accessorFn: (p) => p.ioR + p.ioW,
    sortDescFirst: true,
    meta: { priority: 5, width: 130, mono: true },
    cell: ({ row: { original: p } }) => `${fmtRate(p.ioR)} / ${fmtRate(p.ioW)}`,
  },
  {
    id: "tcp",
    header: "TCP ↓/↑",
    accessorFn: (p) => p.tcpRx + p.tcpTx,
    sortDescFirst: true,
    meta: { priority: 6, width: 130, mono: true },
    cell: ({ row: { original: p } }) => `${fmtRate(p.tcpRx)} / ${fmtRate(p.tcpTx)}`,
  },
  { id: "gpu", header: "GPU", accessorFn: (p) => (p.gpu ?? []).join(","), meta: { priority: 4, width: 60, mono: true }, cell: ({ getValue }) => (getValue() ? `GPU ${getValue()}` : "") },
];

const HOLD_MS = 5000; // how long an order stands while values change
const TREE_KEY = "devpod.processes.tree";
const recallTree = () => {
  try {
    return localStorage.getItem(TREE_KEY) === "1";
  } catch {
    return false;
  }
};
const sortValue = (p: ProcLive, id: string): string | number => {
  const col = columns.find((c) => c.id === id) as { accessorKey?: keyof ProcLive; accessorFn?: (p: ProcLive, i: number) => unknown } | undefined;
  const v = col?.accessorFn ? col.accessorFn(p, 0) : col?.accessorKey ? p[col.accessorKey] : "";
  return typeof v === "number" ? v : String(v ?? "");
};

export function ProcessTable({ procs, limit, live }: { procs?: ProcLive[]; limit?: number; live: boolean }) {
  const [sorting, setSorting] = useState<SortingState>([{ id: "cpu", desc: true }]);
  const [tree, setTree] = useState(recallTree);
  // a moment without data (a late sample) keeps the last list: the table
  // does not empty and jump back to its top
  // (ten seconds at most: a DevPod that stopped has no processes)
  const last = useRef<{ at: number; procs: ProcLive[] } | undefined>(undefined);
  if (procs && procs.length > 0) last.current = { at: Date.now(), procs };
  const list = procs && procs.length === 0 && last.current && Date.now() - last.current.at < 10_000 ? last.current.procs : procs;
  // the order: worked out when the sort changes or every few seconds, then
  // held — new processes go to the end until the next time
  const order = useRef<{ at: number; key: string; ids: string[] }>({ at: 0, key: "", ids: [] });
  const [tick, setTick] = useState(0);
  useEffect(() => {
    const t = setInterval(() => setTick((n) => n + 1), HOLD_MS);
    return () => clearInterval(t);
  }, []);
  const sortKey = `${tree ? "tree" : ""}:${sorting.map((x) => `${x.desc ? "-" : ""}${x.id}`).join(",")}`;
  const { data, depths } = useMemo(() => {
    const all = list ?? [];
    if (limit) return { data: [...all].sort((a, b) => b.cpu - a.cpu).slice(0, limit), depths: undefined };
    if (tree) {
      const rows = procTree([...all]);
      return { data: rows.map((r) => r.p), depths: new Map(rows.map((r) => [idOf(r.p), r.depth])) };
    }
    const now = Date.now();
    const o = order.current;
    if (o.key !== sortKey || now - o.at >= HOLD_MS - 100 || o.ids.length === 0) {
      const by = sorting[0];
      const sorted = by
        ? [...all].sort((a, b) => {
            const x = sortValue(a, by.id);
            const y = sortValue(b, by.id);
            return (x < y ? -1 : x > y ? 1 : 0) * (by.desc ? -1 : 1);
          })
        : all;
      order.current = { at: now, key: sortKey, ids: sorted.map(idOf) };
    }
    const at = new Map(order.current.ids.map((id, i) => [id, i]));
    return { data: [...all].sort((a, b) => (at.get(idOf(a)) ?? Infinity) - (at.get(idOf(b)) ?? Infinity)), depths: undefined };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [list, limit, tree, sortKey, tick]);
  if (!live) return <EmptyState title="—" hint="监控在 Pod 下次重建后生效" />;
  const table = (
    <DataTable<ProcLive>
      storageKey="processes"
      data={data}
      columns={columns}
      getRowId={idOf}
      sorted={!limit}
      fill={!limit}
      sorting={tree ? [] : sorting}
      onSortingChange={(x) => {
        setSorting(x);
        if (tree) toggleTree(false); // asking for an order leaves the tree
      }}
      empty={<EmptyState title={procs ? "没有进程" : "等待数据…"} />}
    />
  );
  function toggleTree(on: boolean) {
    setTree(on);
    try {
      localStorage.setItem(TREE_KEY, on ? "1" : "0");
    } catch {
      // not kept: no matter
    }
  }
  if (limit) return table;
  return (
    <Depths.Provider value={depths}>
      <div className="flex items-center gap-2 border-b border-line px-3 py-1.5 text-xs">
        <Button aria-pressed={tree} onClick={() => toggleTree(!tree)} title="按父子关系显示进程；点表头排序回到列表">
          {tree ? "✓ 树状" : "树状"}
        </Button>
        <span className="text-muted">
          {data.length} 个进程{!tree && "，顺序每 5 秒更新"}
        </span>
      </div>
      <div className="flex h-[min(70vh,720px)] min-h-64 flex-col">{table}</div>
    </Depths.Provider>
  );
}
