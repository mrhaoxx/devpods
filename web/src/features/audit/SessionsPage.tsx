// 会话列表 (UI spec §5.8): admins filter by user / DevPod / time; others
// see their own sessions (the server enforces it).
import { useClusterFilter } from "../../data/clusterFilter";
import { devpodHref } from "../../data/cluster";
import { useState } from "react";
import { Link } from "react-router-dom";
import type { ColumnDef, SortingState } from "@tanstack/react-table";
import { listAuditSessions, type SessionSummary } from "../../data/api";
import { useMe } from "../../data/queries";
import { DataTable } from "../../design/DataTable";
import { Badge } from "../../design/Badge";
import { Chip } from "../../design/Chip";
import { EmptyState } from "../../design/EmptyState";
import { useUrlState, type Schema } from "../../lib/urlState";
import { fmtDuration } from "../../lib/format";
import { useNow } from "../../lib/useNow";
import { AuditError, FilterBox, LoadMore, RangePicker, fmtTS, fromTo, usePaged, KeyName } from "./common";

const schema = {
  range: { kind: "string", default: "7d" },
  user: { kind: "string", default: "" },
  devpod: { kind: "string", default: "" },
  files: { kind: "bool", default: false },
  alerts: { kind: "bool", default: false },
} satisfies Schema;

export function SessionsPage({ mine }: { mine?: boolean }) {
  const me = useMe().data;
  const admin = !!me?.admin && !mine;
  const [url, setUrl] = useUrlState(schema, { key: "audit.sessions" });
  const { filter } = useClusterFilter();
  const now = useNow(10_000);
  const [sorting, setSorting] = useState<SortingState>([]);
  const filters = {
    ...fromTo(url.range),
    user: mine ? (me?.user ?? "") : admin ? url.user : "",
    devpod: url.devpod,
    // the filtered cluster's sessions (a DevPod name is looked up there too)
    cluster: filter,
    hasFiles: url.files ? "true" : "",
    hasAlerts: url.alerts ? "true" : "",
  };
  const q = usePaged<SessionSummary>("sessions", filters, listAuditSessions);
  const columns: ColumnDef<SessionSummary, unknown>[] = [
    { id: "user", header: "用户", accessorKey: "user", meta: { priority: 1, width: 90, mono: true } },
    {
      id: "devpod",
      header: "DevPod",
      accessorKey: "devpod",
      meta: { priority: 2, width: 150, mono: true },
      cell: ({ row: { original: s } }) => (
        <Link to={devpodHref({ cluster: s.cluster, name: s.devpod })} className="hover:text-accent">
          {s.devpod}
          {s.cluster && <span className="ml-1.5 text-faint">· {s.cluster}</span>}
        </Link>
      ),
    },
    {
      id: "from",
      header: "来源 · 认证",
      accessorKey: "client_ip",
      meta: { priority: 5, width: 170, mono: true },
      cell: ({ row: { original: s } }) => `${s.client_ip} · ${s.proxy_alias ? `${s.proxy_alias}/` : ""}${s.auth_kind}`,
    },
    {
      id: "key",
      header: "公钥",
      accessorFn: (s) => s.pubkey_name || s.pubkey_fp,
      meta: { priority: 5, width: 150 },
      cell: ({ row: { original: s } }) => <KeyName fp={s.pubkey_fp} name={s.pubkey_name} proxy={s.proxy_alias} />,
    },
    { id: "start", header: "开始", accessorKey: "opened_at", meta: { priority: 3, width: 150 }, cell: ({ getValue }) => fmtTS(String(getValue())) },
    {
      id: "dur",
      header: "时长",
      accessorFn: (s) => (s.closed_at ? Date.parse(s.closed_at) : now.getTime()) - Date.parse(s.opened_at),
      meta: { priority: 4, width: 80, mono: true },
      cell: ({ row: { original: s }, getValue }) => (
        <span>
          {fmtDuration(getValue() as number)}
          {!s.closed_at && (
            <>
              {" "}
              <Badge tone="run">在线</Badge>
            </>
          )}
        </span>
      ),
    },
    { id: "commands", header: "命令", accessorKey: "commands", meta: { priority: 6, width: 50, mono: true, align: "right" } },
    { id: "files", header: "文件", accessorKey: "files", meta: { priority: 6, width: 50, mono: true, align: "right" } },
    { id: "hits", header: "拦截", accessorKey: "policy_hits", meta: { priority: 5, width: 50, mono: true, align: "right" }, cell: ({ getValue }) => ((getValue() as number) > 0 ? <span className="text-warm">{String(getValue())}</span> : "0") },
    { id: "alerts", header: "告警", accessorKey: "alerts", meta: { priority: 4, width: 50, mono: true, align: "right" }, cell: ({ getValue }) => ((getValue() as number) > 0 ? <span className="text-fail">{String(getValue())}</span> : "0") },
    { id: "gaps", header: "gap", accessorKey: "gaps", meta: { priority: 7, width: 40, mono: true, align: "right" }, cell: ({ getValue }) => ((getValue() as number) > 0 ? <span className="text-fail">{String(getValue())}</span> : "") },
    {
      id: "replay",
      header: "",
      enableSorting: false,
      meta: { priority: 1, width: 50 },
      cell: ({ row }) => (
        <Link to={`/sessions/${row.original.session_id}`} className="text-accent hover:underline">
          {row.original.closed_at ? "回放" : "直播"}
        </Link>
      ),
    },
  ];
  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex flex-wrap items-center gap-2 border-b border-line px-3 py-2">
        <RangePicker value={url.range} onChange={(range) => setUrl({ range })} />
        {admin && <FilterBox label="用户" value={url.user} onChange={(user) => setUrl({ user })} width="w-32" />}
        <FilterBox label="DevPod" value={url.devpod} onChange={(devpod) => setUrl({ devpod })} />
        <Chip active={url.files} onClick={() => setUrl({ files: !url.files })}>
          有文件传输
        </Chip>
        <Chip active={url.alerts} onClick={() => setUrl({ alerts: !url.alerts })}>
          有拦截 / 告警
        </Chip>
      </div>
      {q.isError ? (
        <AuditError error={q.error} />
      ) : (
        <>
          <DataTable<SessionSummary> fill
          storageKey="sessions"
            data={q.items}
            columns={columns}
            getRowId={(s) => s.session_id}
            sorting={sorting}
            onSortingChange={setSorting}
            empty={<EmptyState title={q.isLoading ? "加载中…" : "这段时间没有会话"} />}
          />
          <LoadMore hasMore={q.hasNextPage} busy={q.isFetchingNextPage} onClick={() => void q.fetchNextPage()} />
        </>
      )}
    </div>
  );
}
