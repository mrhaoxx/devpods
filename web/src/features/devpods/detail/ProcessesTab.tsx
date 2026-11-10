// Processes (UI spec §5.2): the top 10 on 概览, every process on 进程;
// other users' command lines arrive redacted from the server.
import { useState } from "react";
import type { ColumnDef, SortingState } from "@tanstack/react-table";
import type { ProcLive } from "../../../data/api";
import { DataTable } from "../../../design/DataTable";
import { Badge } from "../../../design/Badge";
import { EmptyState } from "../../../design/EmptyState";
import { fmtBytes, fmtRate } from "../../../lib/format";

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
    cell: ({ getValue }) => <span title={String(getValue())}>{String(getValue())}</span>,
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

export function ProcessTable({ procs, limit, live }: { procs?: ProcLive[]; limit?: number; live: boolean }) {
  const [sorting, setSorting] = useState<SortingState>([{ id: "cpu", desc: true }]);
  if (!live) return <EmptyState title="—" hint="监控在 Pod 下次重建后生效" />;
  const data = limit ? [...(procs ?? [])].sort((a, b) => b.cpu - a.cpu).slice(0, limit) : (procs ?? []);
  return (
    <DataTable<ProcLive>
      data={data}
      columns={columns}
      getRowId={(p) => `${p.pid}:${p.start}`}
      sorting={sorting}
      onSortingChange={setSorting}
      empty={<EmptyState title={procs ? "没有进程" : "等待数据…"} />}
    />
  );
}
