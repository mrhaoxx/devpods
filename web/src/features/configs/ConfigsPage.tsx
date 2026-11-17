// 配置 (config center spec §6.1): saved DevPod YAML with variables — my
// own, those shared with me, and the public ones, in one table to
// search, scope and sort (there can be dozens). Any of them can be
// launched or copied; only my own can be changed. Several are picked by
// box, a drag or Ctrl+A, and deleted together.
import { useMemo, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "react-router-dom";
import type { ColumnDef, SortingState } from "@tanstack/react-table";
import { ApiFailure } from "../../data/api";
import { useMe } from "../../data/queries";
import { ActionMenu } from "../../design/ActionMenu";
import { Badge } from "../../design/Badge";
import { useBulkRun } from "../../design/BulkToast";
import { Button } from "../../design/Button";
import { ConfirmDialog } from "../../design/ConfirmDialog";
import { DataTable, type RowAction } from "../../design/DataTable";
import { EmptyState } from "../../design/EmptyState";
import { Input, Notice } from "../../design/Form";
import { fmtAge } from "../../lib/format";
import { useUrlState, type Schema } from "../../lib/urlState";
import { recentConfigs } from "../devpods/recentConfigs";
import {
  deleteConfig,
  getConfig,
  listConfigs,
  patchConfig,
  type ConfigScope,
  type ConfigSummary,
} from "./api";

export const configsKey = (scope: ConfigScope) => ["configs", scope] as const;
const useConfigs = (scope: ConfigScope, enabled = true) =>
  useQuery({
    queryKey: configsKey(scope),
    queryFn: () => listConfigs(scope),
    retry: false,
    enabled,
  });

type Scope = "all" | "official" | "mine" | "shared" | "public" | "everyone";
const SCOPES: [Scope, string, string][] = [
  ["all", "全部", "没有符合的配置。"],
  ["official", "官方", "还没有官方配置。"],
  ["mine", "我的", "你还没有保存过配置。"],
  ["shared", "分享给我的", "没有人单独分享配置给你。"],
  ["public", "公开", "没有公开的配置。"],
  // admins: every user's, private ones too
  ["everyone", "所有用户的", "还没有人保存过配置。"],
];
const schema = {
  scope: { kind: "string", default: "all" },
  q: { kind: "string", default: "" },
  sort: { kind: "string", default: "" },
} satisfies Schema;

/** A row: a config and when I last used it (in this browser). */
type Row = ConfigSummary & { usedAt?: number };

const matchesQuery = (c: ConfigSummary, q: string) => {
  const s = q.trim().toLowerCase();
  if (!s) return true;
  return [
    c.title,
    c.name,
    c.owner,
    c.description,
    ...c.variables.map((v) => v.name),
  ].some((x) => x.toLowerCase().includes(s));
};


/** everyone: the admins' page — every user's configs, private ones too. */
export function ConfigsPage({ everyone }: { everyone?: boolean }) {
  const me = useMe().data;
  const admin = !!me?.admin;
  const nav = useNavigate();
  const qc = useQueryClient();
  const [url, setUrl] = useUrlState(schema, { key: "configs" });
  const lists = {
    official: useConfigs("official"),
    mine: useConfigs("mine"),
    shared: useConfigs("shared"),
    public: useConfigs("public"),
    everyone: useConfigs("everyone", !!everyone && admin),
  };
  const [forget, setForget] = useState<{ rows: Row[]; clear?: () => void } | null>(null);
  const bulk = useBulkRun();
  const [error, setError] = useState<string | null>(null);
  const refresh = () => void qc.invalidateQueries({ queryKey: ["configs"] });
  const say = (e: unknown) =>
    setError(e instanceof ApiFailure ? e.body.message : String(e));
  const unpublish = useMutation({
    mutationFn: (name: string) => patchConfig(name, { public: false }),
    onError: say,
    onSettled: refresh,
  });
  const mark = useMutation({
    mutationFn: (p: { name: string; official: boolean }) =>
      patchConfig(p.name, { official: p.official }),
    onError: say,
    onSettled: refresh,
  });
  // a copy starts a new config of mine from the other's text
  const copy = (c: ConfigSummary) =>
    getConfig(c.name)
      .then((full) =>
        nav("/configs/new", {
          state: {
            content: full.content,
            title: full.title,
            description: full.description,
          },
        }),
      )
      .catch(say);

  // what can be done to a config, a selection: said once, offered by the ⋯
  // menu, the right-click menu and the bulk bar alike
  const deletable = (c: Row) => c.mine || admin;
  const rowActions = (c: Row): RowAction[] => [
    { label: c.mine ? "编辑" : "查看", onSelect: () => nav(`/configs/${c.name}`) },
    { label: "用此配置创建", onSelect: () => nav(`/?new=1&config=${encodeURIComponent(c.name)}`) },
    { label: "另存为副本", onSelect: () => void copy(c) },
    ...(admin && c.public ? [{ label: c.official ? "取消官方" : "设为官方", onSelect: () => mark.mutate({ name: c.name, official: !c.official }) }] : []),
    ...(admin && !c.mine && c.public ? [{ label: "取消公开", onSelect: () => unpublish.mutate(c.name) }] : []),
    ...(deletable(c) ? [{ label: "删除", danger: true, onSelect: () => setForget({ rows: [c] }) }] : []),
  ];
  const rowActionsNow = useRef(rowActions);
  rowActionsNow.current = rowActions;
  const bulkActions = (rows: Row[], clear: () => void): RowAction[] => {
    const mayGo = rows.filter(deletable);
    return [
      {
        label: `删除（${mayGo.length}）`,
        danger: true,
        disabled: mayGo.length === 0,
        title: mayGo.length === 0 ? "所选的都不是你的配置" : undefined,
        onSelect: () => setForget({ rows: mayGo, clear }),
      },
    ];
  };
  const forgetAll = async (v: { rows: Row[]; clear?: () => void }) => {
    setForget(null);
    setError(null);
    await bulk.run("删除配置", v.rows, (c) => deleteConfig(c.name).catch((e) => Promise.reject(new Error(e instanceof ApiFailure ? e.body.message : String(e)))), (c) => c.title);
    v.clear?.();
    refresh();
  };

  const data = {
    official: lists.official.data?.items,
    mine: lists.mine.data?.items,
    shared: lists.shared.data?.items,
    public: lists.public.data?.items,
    everyone: lists.everyone.data?.items,
  };
  const used = useMemo(
    () => new Map(recentConfigs(me?.user ?? "").map((r) => [r.name, r.at])),
    [me?.user],
  );
  const byScope = useMemo(() => {
    const row = (c: ConfigSummary): Row => ({ ...c, usedAt: used.get(c.name) });
    const out = {} as Record<Scope, Row[]>;
    const seen = new Map<string, Row>();
    out.everyone = (data.everyone ?? []).map(row);
    for (const k of ["official", "mine", "shared", "public"] as const) {
      out[k] = (data[k] ?? []).map(row);
      for (const c of out[k]) if (!seen.has(c.name)) seen.set(c.name, c);
    }
    out.all = [...seen.values()];
    return out;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [data.official, data.mine, data.shared, data.public, data.everyone, used]);

  // the admins' page shows every user's; this one, the viewer's own scopes
  const scopes = SCOPES.filter(([k]) => k !== "everyone");
  const scope = (
    everyone && admin ? "everyone" : scopes.some(([k]) => k === url.scope) ? url.scope : "all"
  ) as Scope;
  // without a sort asked for: official first, then the latest updated
  const shown = useMemo(
    () =>
      byScope[scope]
        .filter((c) => matchesQuery(c, url.q))
        .sort(
          (a, b) =>
            Number(b.official) - Number(a.official) ||
            Date.parse(b.updatedAt) - Date.parse(a.updatedAt),
        ),
    [byScope, scope, url.q],
  );
  const sorting: SortingState = url.sort
    ? [{ id: url.sort.replace(/^-/, ""), desc: url.sort.startsWith("-") }]
    : [];

  const columns = useMemo<ColumnDef<Row, unknown>[]>(
    () => [
      {
        id: "title",
        header: "标题",
        accessorFn: (c) => c.title,
        meta: { priority: 1, width: 320 },
        cell: ({ row: { original: c } }) => (
          <div className="min-w-0">
            <div className="flex min-w-0 items-center gap-1.5">
              <Link
                data-testid="config-title"
                to={`/configs/${c.name}`}
                className="truncate font-medium text-ink hover:text-accent"
              >
                {c.title}
              </Link>
              {c.official && <Badge tone="accent">官方</Badge>}
              {c.public && !c.official && <Badge tone="accent">公开</Badge>}
              {c.mine && (c.sharedWith.length > 0 || (c.sharedWithGroups?.length ?? 0) > 0) && (
                <Badge title={[c.sharedWith.join(", "), c.sharedWithGroups?.length ? `组 ${c.sharedWithGroups.join(", ")}` : ""].filter(Boolean).join("；")}>
                  分享给 {[c.sharedWith.length > 0 ? `${c.sharedWith.length} 人` : "", c.sharedWithGroups?.length ? `${c.sharedWithGroups.length} 个组` : ""].filter(Boolean).join("、")}
                </Badge>
              )}
            </div>
            {c.description && (
              <div
                className="truncate text-[11px] text-muted"
                title={c.description}
              >
                {c.description}
              </div>
            )}
          </div>
        ),
      },
      {
        id: "owner",
        header: "所有者",
        accessorFn: (c) => c.owner,
        meta: { priority: 2, width: 90, mono: true },
      },
      {
        id: "vars",
        header: "变量",
        enableSorting: false,
        meta: { priority: 4, width: 200 },
        cell: ({ row: { original: c } }) => (
          <span className="flex flex-wrap gap-1">
            {c.variables.length === 0 && <span className="text-faint">—</span>}
            {c.variables.map((v) => (
              <span
                key={v.name}
                className="mono rounded bg-sunk px-1 text-[11px] text-muted"
                title={v.default ? `默认 ${v.default}` : undefined}
              >
                {v.name}
              </span>
            ))}
          </span>
        ),
      },
      {
        id: "used",
        header: "最近使用",
        accessorFn: (c) => c.usedAt,
        sortDescFirst: true,
        sortUndefined: "last",
        meta: { priority: 3, width: 90 },
        cell: ({ row: { original: c } }) =>
          c.usedAt ? (
            <span title={new Date(c.usedAt).toLocaleString()}>
              {fmtAge(new Date(c.usedAt).toISOString())} 前
            </span>
          ) : (
            <span className="text-faint">—</span>
          ),
      },
      {
        id: "updated",
        header: "更新",
        accessorFn: (c) => Date.parse(c.updatedAt),
        sortDescFirst: true,
        meta: { priority: 3, width: 90 },
        cell: ({ row: { original: c } }) => (
          <span title={new Date(c.updatedAt).toLocaleString()}>
            {fmtAge(c.updatedAt)} 前
          </span>
        ),
      },
      {
        id: "actions",
        header: "",
        enableSorting: false,
        meta: { priority: 1, width: 130, align: "right", pin: "right" },
        cell: ({ row: { original: c } }) => (
          <span className="inline-flex items-center gap-2 whitespace-nowrap">
            <Link to={`/?new=1&config=${encodeURIComponent(c.name)}`} className="text-accent hover:underline">
              用此配置创建
            </Link>
            <ActionMenu label={`${c.title} 更多操作`} actions={() => rowActionsNow.current(c)} />
          </span>
        ),
      },
    ],
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [admin],
  );

  const all = Object.values(lists).filter((q) => q.fetchStatus !== "idle" || q.data || q.isError);
  const loading = all.some((q) => q.isLoading);
  const failed = all.every((q) => q.isError);
  const none = !loading && !failed && byScope.all.length === 0;
  const emptyText = url.q
    ? "没有符合的配置。"
    : SCOPES.find(([k]) => k === scope)![2];

  return (
    <div className="flex h-full min-h-0 flex-col">
      <header className="flex flex-wrap items-center gap-2 border-b border-line px-3 py-2">
        {everyone && admin ? (
          <h1 className="text-sm font-semibold">所有用户的配置 <span className="mono text-faint">{byScope.everyone.length}</span></h1>
        ) : (
        <div
          role="tablist"
          aria-label="范围"
          className="inline-flex rounded-md border border-line bg-sunk p-0.5 text-xs"
        >
          {scopes.map(([k, label]) => (
            <button
              key={k}
              type="button"
              role="tab"
              aria-selected={scope === k}
              onClick={() => setUrl({ scope: k })}
              className={`rounded px-2.5 py-0.5 ${scope === k ? "bg-surface font-medium shadow-sm" : "text-muted hover:text-ink"}`}
            >
              {label}{" "}
              <span className="mono text-faint">{byScope[k].length}</span>
            </button>
          ))}
        </div>
        )}
        <Input
          aria-label="搜索配置"
          placeholder="标题、名字、所有者、说明或变量…"
          value={url.q}
          onChange={(e) => setUrl({ q: e.target.value })}
          className="!h-7 w-64 text-xs"
        />
        <span className="flex-1" />
        <Button variant="primary" onClick={() => nav("/configs/new")}>
          + 新建配置
        </Button>
      </header>
      {error && (
        <div className="border-b border-line px-3 py-1.5">
          <Notice>{error}</Notice>
        </div>
      )}
      {failed ? (
        <EmptyState tone="error" title="加载失败" />
      ) : none ? (
        <EmptyState
          title="还没有保存过配置"
          hint="点「新建配置」，或在 DevPod 详情页点「存为配置」。把一份带变量的 DevPod YAML 存成配置，之后选配置、填变量就能创建。"
        />
      ) : (
        <DataTable<Row>
          fill
          label="配置"
          storageKey="configs"
          data={shown}
          columns={columns}
          getRowId={(c) => c.name}
          sorting={sorting}
          onSortingChange={(s) =>
            setUrl({ sort: s[0] ? `${s[0].desc ? "-" : ""}${s[0].id}` : "" })
          }
          onRowClick={(c) => nav(`/configs/${c.name}`)}
          empty={<EmptyState title={loading ? "加载中…" : emptyText} />}
          selectable
          bulkBar={(rows, clear) =>
            bulkActions(rows, clear).map((a) => (
              <Button key={a.label} variant={a.danger ? "danger" : undefined} disabled={a.disabled} title={a.title} onClick={a.onSelect}>
                {a.label}
              </Button>
            ))
          }
          rowMenu={(rows, isBulk, clear) => (isBulk ? bulkActions(rows, clear) : rowActions(rows[0]))}
        />
      )}
      <ConfirmDialog
        open={!!forget}
        onOpenChange={(o) => !o && setForget(null)}
        title={forget && forget.rows.length > 1 ? `删除 ${forget.rows.length} 个配置？` : "删除这个配置？"}
        confirmLabel={forget && forget.rows.length > 1 ? `删除 ${forget.rows.length} 个配置` : "删除配置"}
        danger
        onConfirm={() => forget && void forgetAll(forget)}
      >
        {forget && forget.rows.length > 1 ? (
          <>
            {forget.rows.length} 个配置会被删除
            {forget.rows.some((c) => !c.mine) ? `（其中 ${forget.rows.filter((c) => !c.mine).length} 个是别人的）` : ""}；用它们创建过的 DevPod 不受影响。
          </>
        ) : (
          <>
            <span className="mono text-ink">{forget?.rows[0]?.name}</span> 会被删除
            {forget && !forget.rows[0]?.mine ? `（${forget.rows[0]?.owner} 的配置）` : ""}
            ；用它创建过的 DevPod 不受影响。
          </>
        )}
      </ConfirmDialog>
      {bulk.toast}
    </div>
  );
}
