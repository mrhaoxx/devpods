// 用户与配额 (UI spec §5.5): one table, quick filters, batch actions, CSV
// import and a drawer per user (?user=).
import { useMemo, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import type { ColumnDef, SortingState } from "@tanstack/react-table";
import { createUser, ApiFailure, type AdminUserRow, type BatchOp, type UserQuota } from "../../data/api";
import { qk, useUsers } from "../../data/queries";
import { DataTable } from "../../design/DataTable";
import { Chip } from "../../design/Chip";
import { Button } from "../../design/Button";
import { Badge } from "../../design/Badge";
import { Meter } from "../../design/Meter";
import { EmptyState } from "../../design/EmptyState";
import { ConfirmDialog } from "../../design/ConfirmDialog";
import { Field, Input, Notice } from "../../design/Form";
import { useUrlState, type Schema } from "../../lib/urlState";
import { fmtAge, fmtBytes, fmtCores } from "../../lib/format";
import { useNow } from "../../lib/useNow";
import { bars, isHigh, type Bar } from "./quota";
import { BatchDialog } from "./BatchDialog";
import { ImportDialog } from "./ImportDialog";
import { UserDrawer } from "./UserDrawer";

const schema = {
  user: { kind: "string", default: "" },
  running: { kind: "bool", default: false },
  high: { kind: "bool", default: false },
  custom: { kind: "bool", default: false },
  policy: { kind: "bool", default: false },
  admin: { kind: "bool", default: false },
  never: { kind: "bool", default: false },
  sort: { kind: "string", default: "name" },
} satisfies Schema;

const sourceLabel = { oidc: "GitLab", password: "密码", ldap: "LDAP" } as const;

function BarCell({ b, fmt }: { b: Bar | null; fmt: (n: number) => string }) {
  if (!b) return <span className="text-faint">不限</span>;
  return <Meter used={b.used} limit={b.limit} width={44} label={`${fmt(b.used)}/${fmt(b.limit)}${b.custom ? "*" : ""}`} />;
}

function NewUserDialog({ onClose }: { onClose: (changed: boolean) => void }) {
  const [f, setF] = useState({ username: "", displayName: "", password: "" });
  const [err, setErr] = useState<string | null>(null);
  return (
    <ConfirmDialog
      open
      onOpenChange={(o) => !o && onClose(false)}
      title="新建用户"
      confirmLabel="创建"
      onConfirm={() =>
        void createUser(f.username, f.displayName, f.password)
          .then(() => onClose(true))
          .catch((e) => setErr(e instanceof ApiFailure ? e.body.message : String(e)))
      }
    >
      <div className="space-y-2 text-ink">
        <Field label="用户名">
          <Input aria-label="用户名" value={f.username} onChange={(e) => setF({ ...f, username: e.target.value })} className="mono" />
        </Field>
        <Field label="显示名">
          <Input aria-label="显示名" value={f.displayName} onChange={(e) => setF({ ...f, displayName: e.target.value })} />
        </Field>
        <Field label="初始密码">
          <Input aria-label="初始密码" type="password" value={f.password} onChange={(e) => setF({ ...f, password: e.target.value })} />
        </Field>
        {err && <Notice>{err}</Notice>}
      </div>
    </ConfirmDialog>
  );
}

export function UsersPage() {
  const q = useUsers();
  const qc = useQueryClient();
  const [url, setUrl] = useUrlState(schema);
  const now = useNow(60_000);
  const [batch, setBatch] = useState<{ op: BatchOp; names: string[]; clear: () => void } | null>(null);
  const [dialog, setDialog] = useState<"import" | "new" | null>(null);
  const items = q.data?.items ?? [];
  const def: UserQuota = q.data?.defaultQuota ?? {};

  const columns = useMemo<ColumnDef<AdminUserRow, unknown>[]>(
    () => [
      {
        id: "name",
        header: "用户名",
        accessorKey: "name",
        meta: { priority: 1, width: 170 },
        cell: ({ row: { original: u } }) => (
          <span className="inline-flex items-center gap-1.5">
            <span data-testid="user-name" className="mono">
              {u.name}
            </span>
            {u.admin && <Badge tone="accent">管理员</Badge>}
            {u.displayName && <span className="truncate text-muted">{u.displayName}</span>}
          </span>
        ),
      },
      { id: "source", header: "来源", accessorKey: "source", meta: { priority: 6, width: 60 }, cell: ({ row }) => sourceLabel[row.original.source] },
      {
        id: "devpods",
        header: "DevPod",
        accessorKey: "devpods",
        meta: { priority: 2, width: 70, mono: true },
        cell: ({ row: { original: u } }) => {
          const b = bars(u, def).pods;
          return `${u.running}/${u.devpods}${b ? ` · ${b.limit}${b.custom ? "*" : ""}` : ""}`;
        },
      },
      { id: "cpu", header: "CPU", enableSorting: false, meta: { priority: 3, width: 130 }, cell: ({ row }) => <BarCell b={bars(row.original, def).cpu} fmt={fmtCores} /> },
      { id: "mem", header: "内存", enableSorting: false, meta: { priority: 4, width: 150 }, cell: ({ row }) => <BarCell b={bars(row.original, def).mem} fmt={fmtBytes} /> },
      { id: "gpu", header: "GPU", enableSorting: false, meta: { priority: 5, width: 100 }, cell: ({ row }) => <BarCell b={bars(row.original, def).gpu} fmt={(n) => String(n)} /> },
      {
        id: "last",
        header: "最近会话",
        accessorFn: (u) => (u.lastSession ? Date.parse(u.lastSession) : 0),
        meta: { priority: 7, width: 80, mono: true },
        cell: ({ row: { original: u } }) => (u.lastSession ? fmtAge(u.lastSession, now) : <span className="text-faint">从未</span>),
      },
      { id: "policy", header: "策略", accessorFn: (u) => u.customPolicy, meta: { priority: 8, width: 60 }, cell: ({ row }) => (row.original.customPolicy ? <Badge tone="warm">自定义</Badge> : null) },
    ],
    [def, now],
  );

  const shown = items.filter(
    (u) =>
      (!url.running || u.running > 0) &&
      (!url.high || isHigh(u, def)) &&
      (!url.custom || !!u.quota) &&
      (!url.policy || u.customPolicy) &&
      (!url.admin || u.admin) &&
      (!url.never || !u.lastSession),
  );
  const sorting: SortingState = url.sort ? [{ id: url.sort.replace(/^-/, ""), desc: url.sort.startsWith("-") }] : [];
  const chips: [keyof typeof url, string, number][] = [
    ["running", "有运行中", items.filter((u) => u.running > 0).length],
    ["high", "配额 >80%", items.filter((u) => isHigh(u, def)).length],
    ["custom", "自定义配额", items.filter((u) => u.quota).length],
    ["policy", "自定义审计策略", items.filter((u) => u.customPolicy).length],
    ["admin", "管理员", items.filter((u) => u.admin).length],
    ["never", "从未登录", items.filter((u) => !u.lastSession).length],
  ];
  const refresh = (changed: boolean) => changed && void qc.invalidateQueries({ queryKey: qk.users });
  const defText = [
    def.compute?.cpu && `CPU ${def.compute.cpu}`,
    def.compute?.memory && `内存 ${def.compute.memory}`,
    def.compute?.["nvidia.com/gpu"] && `GPU ${def.compute["nvidia.com/gpu"]}`,
    def.maxDevPods != null && `DevPod ${def.maxDevPods}`,
    def.storage && `存储 ${def.storage}`,
  ]
    .filter(Boolean)
    .join(" · ");

  return (
    <>
      <div className="flex flex-wrap items-center gap-2 border-b border-line px-3 py-2">
        {chips.map(([k, label, n]) => (
          <Chip key={k} active={!!url[k]} count={n} onClick={() => setUrl({ [k]: !url[k] })}>
            {label}
          </Chip>
        ))}
        <span className="flex-1" />
        <Button onClick={() => setDialog("import")}>导入 CSV</Button>
        <Button variant="primary" onClick={() => setDialog("new")}>
          + 新建用户
        </Button>
      </div>
      {q.isError ? (
        <EmptyState tone="error" title="无法加载用户" hint={String(q.error)} />
      ) : (
        <DataTable<AdminUserRow>
          data={shown}
          columns={columns}
          getRowId={(u) => u.name}
          sorting={sorting}
          onSortingChange={(s) => setUrl({ sort: s[0] ? `${s[0].desc ? "-" : ""}${s[0].id}` : "" })}
          selectable
          onRowClick={(u) => setUrl({ user: u.name })}
          empty={<EmptyState title={q.isLoading ? "加载中…" : "没有符合条件的用户"} />}
          bulkBar={(sel, clear) =>
            (["setQuota", "setPolicy", "resetPassword", "hibernateAll"] as BatchOp[]).map((op) => (
              <Button key={op} variant={op === "resetPassword" || op === "hibernateAll" ? "danger" : "default"} onClick={() => setBatch({ op, names: sel.map((u) => u.name), clear })}>
                {{ setQuota: "设配额", setPolicy: "设审计策略", resetPassword: "重置密码", hibernateAll: "休眠全部 DevPod" }[op]}
              </Button>
            ))
          }
        />
      )}
      {defText && <div className="border-t border-line px-3 py-1.5 text-[11px] text-faint">* 为自定义配额；全局默认：{defText}</div>}
      {batch && (
        <BatchDialog
          op={batch.op}
          names={batch.names}
          onClose={(changed) => {
            refresh(changed);
            if (changed) batch.clear();
            setBatch(null);
          }}
        />
      )}
      {dialog === "import" && (
        <ImportDialog
          onClose={(c) => {
            refresh(c);
            setDialog(null);
          }}
        />
      )}
      {dialog === "new" && (
        <NewUserDialog
          onClose={(c) => {
            refresh(c);
            setDialog(null);
          }}
        />
      )}
      {url.user && q.data && <UserDrawer u={items.find((u) => u.name === url.user)} def={def} onClose={() => setUrl({ user: "" })} />}
    </>
  );
}
