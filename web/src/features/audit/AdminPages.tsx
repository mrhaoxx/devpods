// 告警, 访问记录 and 策略 (UI spec §5.8), admins only.
import { useMemo, useState } from "react";
import { Link, NavLink } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import type { ColumnDef, SortingState } from "@tanstack/react-table";
import { getAuditPolicyDefault, listAccessLog, listAlerts, listAdminUsers, listDevPods, type AccessLogRow, type AlertRow, type AuditPolicy } from "../../data/api";
import { qk } from "../../data/queries";
import { DataTable } from "../../design/DataTable";
import { EmptyState } from "../../design/EmptyState";
import { Panel } from "../../design/Panel";
import { Notice } from "../../design/Form";
import { Badge } from "../../design/Badge";
import { useUrlState, type Schema } from "../../lib/urlState";
import { AuditError, FilterBox, LoadMore, RangePicker, fmtTS, fromTo, usePaged } from "./common";

const alertSchema = { range: { kind: "string", default: "7d" }, devpod: { kind: "string", default: "" } } satisfies Schema;

export function AlertsPage() {
  const [url, setUrl] = useUrlState(alertSchema);
  const [sorting, setSorting] = useState<SortingState>([]);
  const q = usePaged<AlertRow>("alerts", fromTo(url.range), listAlerts);
  const pods = useQuery({ queryKey: qk.devpods("all"), queryFn: () => listDevPods("all") });
  const byUID = useMemo(() => new Map((pods.data?.items ?? []).map((d) => [d.metadata.uid ?? "", d.metadata.name])), [pods.data]);
  const rows = url.devpod ? q.items.filter((a) => (byUID.get(a.devpod_uid) ?? "").includes(url.devpod)) : q.items;
  const columns: ColumnDef<AlertRow, unknown>[] = [
    { id: "ts", header: "时间", accessorKey: "ts", meta: { priority: 2, width: 150 }, cell: ({ getValue }) => fmtTS(String(getValue())) },
    { id: "kind", header: "类型", accessorKey: "kind", meta: { priority: 1, width: 120 }, cell: ({ getValue }) => <Badge tone="fail">{String(getValue())}</Badge> },
    { id: "devpod", header: "DevPod", accessorFn: (a) => byUID.get(a.devpod_uid) ?? a.devpod_uid, meta: { priority: 3, width: 150, mono: true } },
    { id: "source", header: "来源", accessorKey: "source", meta: { priority: 5, width: 80 } },
    { id: "detail", header: "详情", accessorKey: "detail", meta: { priority: 1, width: 360 }, cell: ({ getValue }) => <span title={String(getValue())}>{String(getValue())}</span> },
    { id: "s", header: "", enableSorting: false, meta: { priority: 1, width: 50 }, cell: ({ row }) => row.original.session_id && <Link to={`/sessions/${row.original.session_id}`} className="text-accent hover:underline">回放</Link> },
  ];
  return (
    <>
      <div className="flex flex-wrap items-center gap-2 border-b border-line px-3 py-2">
        <RangePicker value={url.range} onChange={(range) => setUrl({ range })} />
        <FilterBox label="DevPod" value={url.devpod} onChange={(devpod) => setUrl({ devpod })} />
      </div>
      {q.isError ? (
        <AuditError error={q.error} />
      ) : (
        <>
          <DataTable<AlertRow> data={rows} columns={columns} getRowId={(a) => `${a.ts}:${a.kind}:${a.session_id}:${a.detail}`} sorting={sorting} onSortingChange={setSorting} empty={<EmptyState title={q.isLoading ? "加载中…" : "这段时间没有告警"} />} />
          <LoadMore hasMore={q.hasNextPage} busy={q.isFetchingNextPage} onClick={() => void q.fetchNextPage()} />
        </>
      )}
    </>
  );
}

function AccessTabs() {
  const cls = ({ isActive }: { isActive: boolean }) => `-mb-px border-b-2 px-3 py-1.5 ${isActive ? "border-accent font-semibold text-accent" : "border-transparent text-muted hover:text-ink"}`;
  return (
    <nav className="flex gap-1 border-b border-line px-3 text-xs">
      <NavLink to="/audit/access-log" className={cls}>
        访问记录
      </NavLink>
      <NavLink to="/audit/policy" className={cls}>
        策略
      </NavLink>
    </nav>
  );
}

const accessSchema = { range: { kind: "string", default: "7d" }, viewer: { kind: "string", default: "" } } satisfies Schema;

export function AccessLogPage() {
  const [url, setUrl] = useUrlState(accessSchema);
  const [sorting, setSorting] = useState<SortingState>([]);
  const q = usePaged<AccessLogRow>("access", fromTo(url.range), listAccessLog);
  const rows = url.viewer ? q.items.filter((a) => a.viewer.includes(url.viewer)) : q.items;
  const columns: ColumnDef<AccessLogRow, unknown>[] = [
    { id: "ts", header: "时间", accessorKey: "ts", meta: { priority: 2, width: 150 }, cell: ({ getValue }) => fmtTS(String(getValue())) },
    { id: "viewer", header: "查看者", accessorKey: "viewer", meta: { priority: 1, width: 100, mono: true } },
    { id: "action", header: "动作", accessorKey: "action", meta: { priority: 1, width: 110, mono: true } },
    {
      id: "target",
      header: "对象",
      accessorFn: (a) => a.target_file || a.target_session,
      meta: { priority: 3, width: 300, mono: true },
      cell: ({ row: { original: a } }) =>
        a.target_session ? (
          <Link to={`/sessions/${a.target_session}`} className="hover:text-accent">
            {a.target_session}
            {a.target_file && ` · ${a.target_file}`}
          </Link>
        ) : (
          a.target_file
        ),
    },
    { id: "ip", header: "IP", accessorKey: "client_ip", meta: { priority: 5, width: 130, mono: true } },
  ];
  return (
    <>
      <AccessTabs />
      <div className="flex flex-wrap items-center gap-2 border-b border-line px-3 py-2">
        <RangePicker value={url.range} onChange={(range) => setUrl({ range })} />
        <FilterBox label="查看者" value={url.viewer} onChange={(viewer) => setUrl({ viewer })} width="w-32" />
      </div>
      {q.isError ? (
        <AuditError error={q.error} />
      ) : (
        <>
          <DataTable<AccessLogRow> data={rows} columns={columns} getRowId={(a) => `${a.ts}:${a.viewer}:${a.action}:${a.target_session}:${a.target_file}`} sorting={sorting} onSortingChange={setSorting} empty={<EmptyState title={q.isLoading ? "加载中…" : "这段时间没有访问记录"} />} />
          <LoadMore hasMore={q.hasNextPage} busy={q.isFetchingNextPage} onClick={() => void q.fetchNextPage()} />
        </>
      )}
    </>
  );
}

const action = (a?: string) => (a === "Deny" ? "拒绝" : "允许");

/** vscodeBypass mirrors policy.WarnVSCodeBypass: downloads denied while
 *  local forwarding (VS Code's channel) is allowed. */
export function vscodeBypass(p: AuditPolicy): boolean {
  return p.download === "Deny" && p.forwarding?.local !== "Deny";
}

export function PolicyPage() {
  const def = useQuery({ queryKey: ["audit", "policy-default"], queryFn: getAuditPolicyDefault, retry: false });
  const users = useQuery({ queryKey: qk.users, queryFn: listAdminUsers });
  const custom = (users.data?.items ?? []).filter((u) => u.customPolicy);
  const p = def.data;
  return (
    <>
      <AccessTabs />
      <div className="grid gap-3 p-3 lg:grid-cols-2">
        <Panel title="集群默认策略（只读）">
          {def.isError ? (
            <AuditError error={def.error} />
          ) : !p ? (
            <EmptyState title="加载中…" />
          ) : (
            <div className="space-y-2 p-3">
              {vscodeBypass(p) && <Notice tone="warm">下载被拒绝，但本地端口转发仍允许：VS Code 可以借此绕过下载限制。</Notice>}
              <dl className="grid grid-cols-[9rem_1fr] gap-y-1 text-xs">
                {(
                  [
                    ["下载文件", action(p.download)],
                    ["上传文件", action(p.upload)],
                    ["本地端口转发", action(p.forwarding?.local)],
                    ["远程端口转发", action(p.forwarding?.remote)],
                    ["记录转发内容", p.forwarding?.recordContent ? "是" : "否"],
                    ["Agent 转发", action(p.agentForwarding)],
                    ["X11 转发", action(p.x11Forwarding)],
                    ["审计失败时", p.failMode === "Closed" ? "拒绝连接" : "放行"],
                    ["单文件上限", p.maxFileSize ?? "不限"],
                  ] as [string, string][]
                ).map(([k, v]) => (
                  <div key={k} className="contents">
                    <dt className="text-muted">{k}</dt>
                    <dd className="mono">{v}</dd>
                  </div>
                ))}
              </dl>
              <p className="text-[11px] text-faint">集群默认策略在 Helm values（audit.policy）里配置。</p>
            </div>
          )}
        </Panel>
        <Panel title={`自定义策略的用户（${custom.length}）`}>
          {custom.length === 0 ? (
            <EmptyState title="没有用户覆盖默认策略" />
          ) : (
            <ul className="divide-y divide-line text-xs">
              {custom.map((u) => (
                <li key={u.name} className="px-3 py-1">
                  <Link to={`/admin/users?user=${encodeURIComponent(u.name)}`} className="mono hover:text-accent">
                    {u.name}
                  </Link>
                </li>
              ))}
            </ul>
          )}
        </Panel>
      </div>
    </>
  );
}
