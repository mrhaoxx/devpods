// 命令 · 文件 检索 (UI spec §5.8): full-width tables, substring / path /
// sha256 search, time range in the URL; the server limits non-admins to
// their own events.
import { useClusterFilter } from "../../data/clusterFilter";
import { useState } from "react";
import { Link, NavLink } from "react-router-dom";
import type { ColumnDef, SortingState } from "@tanstack/react-table";
import { fileContentURL, searchCommands, searchFiles, type CommandRow, type FileEventRow } from "../../data/api";
import { useMe } from "../../data/queries";
import { DataTable } from "../../design/DataTable";
import { Badge } from "../../design/Badge";
import { EmptyState } from "../../design/EmptyState";
import { useUrlState, type Schema } from "../../lib/urlState";
import { fmtBytes } from "../../lib/format";
import { AuditError, FilterBox, LoadMore, RangePicker, confirmDownload, fmtTS, fromTo, usePaged } from "./common";

function Tabs() {
  const cls = ({ isActive }: { isActive: boolean }) => `-mb-px border-b-2 px-3 py-1.5 ${isActive ? "border-accent font-semibold text-accent" : "border-transparent text-muted hover:text-ink"}`;
  return (
    <nav className="flex gap-1 border-b border-line px-3 text-xs">
      <NavLink to="/audit/commands" className={cls}>
        命令
      </NavLink>
      <NavLink to="/audit/files" className={cls}>
        文件
      </NavLink>
    </nav>
  );
}

const cmdSchema = { range: { kind: "string", default: "7d" }, q: { kind: "string", default: "" }, user: { kind: "string", default: "" }, devpod: { kind: "string", default: "" } } satisfies Schema;

export function CommandsPage() {
  const admin = !!useMe().data?.admin;
  const [url, setUrl] = useUrlState(cmdSchema);
  const [sorting, setSorting] = useState<SortingState>([]);
  // a DevPod name is looked up in the filtered cluster (a name can be in two)
  const { filter } = useClusterFilter();
  const q = usePaged<CommandRow>("commands", { ...fromTo(url.range), q: url.q, user: admin ? url.user : "", devpod: url.devpod, cluster: url.devpod ? filter : "" }, searchCommands);
  const columns: ColumnDef<CommandRow, unknown>[] = [
    { id: "ts", header: "时间", accessorKey: "ts", meta: { priority: 2, width: 150 }, cell: ({ getValue }) => fmtTS(String(getValue())) },
    { id: "user", header: "用户", accessorKey: "user", meta: { priority: 3, width: 90, mono: true } },
    { id: "cmd", header: "命令", accessorKey: "cmdline", meta: { priority: 1, width: 400, mono: true }, cell: ({ getValue }) => <span title={String(getValue())}>{String(getValue())}</span> },
    { id: "exit", header: "退出码", accessorKey: "exit_code", meta: { priority: 4, width: 50, mono: true }, cell: ({ getValue }) => (getValue() == null ? "—" : <span className={getValue() ? "text-fail" : ""}>{String(getValue())}</span>) },
    { id: "source", header: "来源", accessorKey: "source", meta: { priority: 6, width: 70 } },
    { id: "s", header: "", enableSorting: false, meta: { priority: 1, width: 50 }, cell: ({ row }) => row.original.session_id && <Link to={`/sessions/${row.original.session_id}`} className="text-accent hover:underline">会话</Link> },
  ];
  return (
    <>
      <Tabs />
      <div className="flex flex-wrap items-center gap-2 border-b border-line px-3 py-2">
        <RangePicker value={url.range} onChange={(range) => setUrl({ range })} />
        <FilterBox label="命令包含" value={url.q} onChange={(v) => setUrl({ q: v })} width="w-56" />
        {admin && <FilterBox label="用户" value={url.user} onChange={(user) => setUrl({ user })} width="w-32" />}
        <FilterBox label="DevPod" value={url.devpod} onChange={(devpod) => setUrl({ devpod })} />
      </div>
      {q.isError ? (
        <AuditError error={q.error} />
      ) : (
        <>
          <DataTable<CommandRow> storageKey="commands" data={q.items} columns={columns} getRowId={(c) => c.event_id} sorting={sorting} onSortingChange={setSorting} empty={<EmptyState title={q.isLoading ? "加载中…" : "这段时间没有命令"} />} />
          <LoadMore hasMore={q.hasNextPage} busy={q.isFetchingNextPage} onClick={() => void q.fetchNextPage()} />
        </>
      )}
    </>
  );
}

const fileSchema = { range: { kind: "string", default: "7d" }, path: { kind: "string", default: "" }, sha256: { kind: "string", default: "" }, user: { kind: "string", default: "" } } satisfies Schema;

export function FilesPage() {
  const admin = !!useMe().data?.admin;
  const [url, setUrl] = useUrlState(fileSchema);
  const [sorting, setSorting] = useState<SortingState>([]);
  const q = usePaged<FileEventRow>("files", { ...fromTo(url.range), path: url.path, sha256: url.sha256, user: admin ? url.user : "" }, searchFiles);
  const columns: ColumnDef<FileEventRow, unknown>[] = [
    { id: "ts", header: "时间", accessorKey: "ts", meta: { priority: 2, width: 150 }, cell: ({ getValue }) => fmtTS(String(getValue())) },
    { id: "user", header: "用户", accessorKey: "user", meta: { priority: 3, width: 90, mono: true } },
    { id: "op", header: "操作", accessorFn: (f) => `${f.protocol} ${f.op}`, meta: { priority: 4, width: 110, mono: true } },
    { id: "path", header: "路径", accessorKey: "path", meta: { priority: 1, width: 320, mono: true }, cell: ({ row: { original: f } }) => <span title={f.path2 ? `${f.path} → ${f.path2}` : f.path}>{f.path}{f.path2 && ` → ${f.path2}`}</span> },
    { id: "size", header: "大小", accessorKey: "size", meta: { priority: 5, width: 70, mono: true, align: "right" }, cell: ({ getValue }) => fmtBytes(getValue() as number) },
    { id: "sha", header: "sha256", accessorKey: "sha256", meta: { priority: 7, width: 110, mono: true }, cell: ({ getValue }) => <span title={String(getValue())}>{String(getValue()).slice(0, 12)}</span> },
    {
      id: "status",
      header: "",
      enableSorting: false,
      meta: { priority: 1, width: 130 },
      cell: ({ row: { original: f } }) => (
        <span className="inline-flex items-center gap-2">
          {f.blocked && <Badge tone="warm">已拦截</Badge>}
          {!f.blocked && f.s3_key && !f.complete && <Badge>不完整</Badge>}
          {f.s3_key && (
            <a href={fileContentURL(f.event_id)} onClick={confirmDownload} className="text-accent hover:underline">
              下载
            </a>
          )}
          <Link to={`/sessions/${f.session_id}`} className="text-accent hover:underline">
            会话
          </Link>
        </span>
      ),
    },
  ];
  return (
    <>
      <Tabs />
      <div className="flex flex-wrap items-center gap-2 border-b border-line px-3 py-2">
        <RangePicker value={url.range} onChange={(range) => setUrl({ range })} />
        <FilterBox label="路径包含" value={url.path} onChange={(path) => setUrl({ path })} width="w-56" />
        <FilterBox label="sha256" value={url.sha256} onChange={(sha256) => setUrl({ sha256 })} width="w-56" />
        {admin && <FilterBox label="用户" value={url.user} onChange={(user) => setUrl({ user })} width="w-32" />}
      </div>
      {q.isError ? (
        <AuditError error={q.error} />
      ) : (
        <>
          <DataTable<FileEventRow> storageKey="files" data={q.items} columns={columns} getRowId={(f) => f.event_id} sorting={sorting} onSortingChange={setSorting} empty={<EmptyState title={q.isLoading ? "加载中…" : "这段时间没有文件传输"} />} />
          <LoadMore hasMore={q.hasNextPage} busy={q.isFetchingNextPage} onClick={() => void q.fetchNextPage()} />
        </>
      )}
    </>
  );
}
