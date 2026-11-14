// 命令 · 文件 · HTTP 检索 (UI spec §5.8): full-width tables, substring / path /
// sha256 search, HTTP requests to DevPod ports by DevPod / port / status, time range in the URL; the server limits non-admins to
// their own events.
import { useClusterFilter } from "../../data/clusterFilter";
import { useState } from "react";
import { Link, NavLink } from "react-router-dom";
import type { ColumnDef, SortingState } from "@tanstack/react-table";
import { fileContentURL, searchCommands, searchFiles, searchHTTP, type CommandRow, type FileEventRow, type HTTPRequestRow } from "../../data/api";
import { useMe } from "../../data/queries";
import { DataTable } from "../../design/DataTable";
import { Badge } from "../../design/Badge";
import { EmptyState } from "../../design/EmptyState";
import { useUrlState, type Schema } from "../../lib/urlState";
import { fmtBytes } from "../../lib/format";
import { AuditError, FilterBox, LoadMore, RangePicker, confirmDownload, fmtTS, fromTo, usePaged, withCluster, PagedRefresh, endOf } from "./common";

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
      <NavLink to="/audit/http" className={cls}>
        HTTP
      </NavLink>
    </nav>
  );
}

const cmdSchema = { range: { kind: "string", default: "7d" }, q: { kind: "string", default: "" }, user: { kind: "string", default: "" }, devpod: { kind: "string", default: "" } } satisfies Schema;

export function CommandsPage() {
  const admin = !!useMe().data?.admin;
  const [url, setUrl] = useUrlState(cmdSchema, { key: "audit.commands" });
  const [sorting, setSorting] = useState<SortingState>([]);
  // the filtered cluster's commands (a DevPod name is looked up there too)
  const { filter, several } = useClusterFilter();
  const q = usePaged<CommandRow>("commands", { ...fromTo(url.range), q: url.q, user: admin ? url.user : "", devpod: url.devpod, cluster: filter }, searchCommands);
  const columns: ColumnDef<CommandRow, unknown>[] = [
    { id: "ts", header: "时间", accessorKey: "ts", meta: { priority: 2, width: 150 }, cell: ({ getValue }) => fmtTS(String(getValue())) },
    { id: "user", header: "用户", accessorKey: "user", meta: { priority: 3, width: 90, mono: true } },
    { id: "cmd", header: "命令", accessorKey: "cmdline", meta: { priority: 1, width: 400, mono: true }, cell: ({ getValue }) => <span title={String(getValue())}>{String(getValue())}</span> },
    { id: "exit", header: "退出码", accessorKey: "exit_code", meta: { priority: 4, width: 50, mono: true }, cell: ({ getValue }) => (getValue() == null ? "—" : <span className={getValue() ? "text-fail" : ""}>{String(getValue())}</span>) },
    { id: "source", header: "来源", accessorKey: "source", meta: { priority: 6, width: 70 } },
    { id: "s", header: "", enableSorting: false, meta: { priority: 1, width: 50 }, cell: ({ row }) => row.original.session_id && <Link to={`/sessions/${row.original.session_id}`} className="text-accent hover:underline">会话</Link> },
  ];
  return (
    <div className="flex h-full min-h-0 flex-col">
      <Tabs />
      <div className="flex flex-wrap items-center gap-2 border-b border-line px-3 py-2">
        <RangePicker value={url.range} onChange={(range) => setUrl({ range })} />
        <FilterBox label="命令包含" value={url.q} onChange={(v) => setUrl({ q: v })} width="w-56" />
        {admin && <FilterBox label="用户" value={url.user} onChange={(user) => setUrl({ user })} width="w-32" />}
        <FilterBox label="DevPod" value={url.devpod} onChange={(devpod) => setUrl({ devpod })} />
        <span className="flex-1" />
        <PagedRefresh q={q} />
      </div>
      {q.isError ? (
        <AuditError error={q.error} />
      ) : (
        <>
          <DataTable<CommandRow> fill onEnd={endOf(q)} storageKey="commands" data={q.items} columns={withCluster(columns, several)} getRowId={(c) => c.event_id} sorting={sorting} onSortingChange={setSorting} empty={<EmptyState title={q.isLoading ? "加载中…" : "这段时间没有命令"} />} />
          <LoadMore hasMore={q.hasNextPage} busy={q.isFetchingNextPage} onClick={() => void q.fetchNextPage()} />
        </>
      )}
    </div>
  );
}

const fileSchema = { range: { kind: "string", default: "7d" }, path: { kind: "string", default: "" }, sha256: { kind: "string", default: "" }, user: { kind: "string", default: "" } } satisfies Schema;

export function FilesPage() {
  const admin = !!useMe().data?.admin;
  const [url, setUrl] = useUrlState(fileSchema, { key: "audit.files" });
  const [sorting, setSorting] = useState<SortingState>([]);
  const { filter, several } = useClusterFilter();
  const q = usePaged<FileEventRow>("files", { ...fromTo(url.range), path: url.path, sha256: url.sha256, user: admin ? url.user : "", cluster: filter }, searchFiles);
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
    <div className="flex h-full min-h-0 flex-col">
      <Tabs />
      <div className="flex flex-wrap items-center gap-2 border-b border-line px-3 py-2">
        <RangePicker value={url.range} onChange={(range) => setUrl({ range })} />
        <FilterBox label="路径包含" value={url.path} onChange={(path) => setUrl({ path })} width="w-56" />
        <FilterBox label="sha256" value={url.sha256} onChange={(sha256) => setUrl({ sha256 })} width="w-56" />
        {admin && <FilterBox label="用户" value={url.user} onChange={(user) => setUrl({ user })} width="w-32" />}
        <span className="flex-1" />
        <PagedRefresh q={q} />
      </div>
      {q.isError ? (
        <AuditError error={q.error} />
      ) : (
        <>
          <DataTable<FileEventRow> fill onEnd={endOf(q)} storageKey="files" data={q.items} columns={withCluster(columns, several)} getRowId={(f) => f.event_id} sorting={sorting} onSortingChange={setSorting} empty={<EmptyState title={q.isLoading ? "加载中…" : "这段时间没有文件传输"} />} />
          <LoadMore hasMore={q.hasNextPage} busy={q.isFetchingNextPage} onClick={() => void q.fetchNextPage()} />
        </>
      )}
    </div>
  );
}

const httpSchema = {
  range: { kind: "string", default: "7d" },
  user: { kind: "string", default: "" },
  devpod: { kind: "string", default: "" },
  port: { kind: "string", default: "" },
  status: { kind: "string", default: "" },
  method: { kind: "string", default: "" },
} satisfies Schema;

// HTTP requests to DevPod ports through a gateway (one row per request,
// one per WebSocket connection).
export function HTTPPage() {
  const admin = !!useMe().data?.admin;
  const [url, setUrl] = useUrlState(httpSchema, { key: "audit.http" });
  const [sorting, setSorting] = useState<SortingState>([]);
  const { filter, several } = useClusterFilter();
  const q = usePaged<HTTPRequestRow>(
    "http",
    { ...fromTo(url.range), user: admin ? url.user : "", devpod: url.devpod, port: url.port, status: url.status, method: url.method, cluster: filter },
    searchHTTP,
  );
  const columns: ColumnDef<HTTPRequestRow, unknown>[] = [
    { id: "ts", header: "时间", accessorKey: "ts", meta: { priority: 2, width: 150 }, cell: ({ getValue }) => fmtTS(String(getValue())) },
    { id: "user", header: "用户", accessorKey: "user", meta: { priority: 3, width: 90, mono: true }, cell: ({ getValue }) => (getValue() ? String(getValue()) : <span className="text-muted">匿名</span>) },
    { id: "devpod", header: "DevPod", accessorFn: (h) => `${h.devpod}:${h.port}`, meta: { priority: 3, width: 150, mono: true } },
    { id: "method", header: "方法", accessorKey: "method", meta: { priority: 4, width: 60, mono: true }, cell: ({ row: { original: h } }) => (h.websocket ? `${h.method} ws` : h.method) },
    {
      id: "path",
      header: "路径",
      accessorFn: (h) => (h.query ? `${h.path}?${h.query}` : h.path),
      meta: { priority: 1, width: 320, mono: true },
      cell: ({ getValue }) => <span title={String(getValue())}>{String(getValue())}</span>,
    },
    { id: "status", header: "状态", accessorKey: "status", meta: { priority: 2, width: 50, mono: true }, cell: ({ getValue }) => <span className={(getValue() as number) >= 400 ? "text-warm" : undefined}>{String(getValue())}</span> },
    { id: "bytes", header: "入 / 出", accessorFn: (h) => h.bytes_in + h.bytes_out, meta: { priority: 5, width: 110, mono: true, align: "right" }, cell: ({ row: { original: h } }) => `${fmtBytes(h.bytes_in)} / ${fmtBytes(h.bytes_out)}` },
    { id: "dur", header: "耗时", accessorKey: "duration_ms", meta: { priority: 6, width: 70, mono: true, align: "right" }, cell: ({ getValue }) => `${getValue()} ms` },
    {
      id: "client",
      header: "来源",
      accessorKey: "client_ip",
      meta: { priority: 7, width: 110, mono: true },
      cell: ({ row: { original: h } }) => <span title={h.forwarded_for ? `X-Forwarded-For（未经核实）: ${h.forwarded_for}` : undefined}>{h.forwarded_for || h.client_ip}</span>,
    },
  ];
  return (
    <div className="flex h-full min-h-0 flex-col">
      <Tabs />
      <div className="flex flex-wrap items-center gap-2 border-b border-line px-3 py-2">
        <RangePicker value={url.range} onChange={(range) => setUrl({ range })} />
        {admin && <FilterBox label="用户" value={url.user} onChange={(user) => setUrl({ user })} width="w-32" />}
        <FilterBox label="DevPod" value={url.devpod} onChange={(devpod) => setUrl({ devpod })} />
        <FilterBox label="端口" value={url.port} onChange={(port) => setUrl({ port })} width="w-20" />
        <FilterBox label="状态" value={url.status} onChange={(status) => setUrl({ status })} width="w-20" />
        <FilterBox label="方法" value={url.method} onChange={(method) => setUrl({ method: method.toUpperCase() })} width="w-20" />
        <span className="flex-1" />
        <PagedRefresh q={q} />
      </div>
      {q.isError ? (
        <AuditError error={q.error} />
      ) : (
        <>
          <DataTable<HTTPRequestRow> fill onEnd={endOf(q)}
            storageKey="http"
            data={q.items}
            columns={withCluster(columns, several)}
            getRowId={(h) => h.request_id}
            sorting={sorting}
            onSortingChange={setSorting}
            empty={<EmptyState title={q.isLoading ? "加载中…" : "这段时间没有 HTTP 请求"} />}
          />
          <LoadMore hasMore={q.hasNextPage} busy={q.isFetchingNextPage} onClick={() => void q.fetchNextPage()} />
        </>
      )}
    </div>
  );
}
