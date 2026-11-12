// 配置 (config center spec §6.1): saved DevPod YAML with variables — my
// own, those shared with me, and the public ones, in one table to
// search, scope and sort (there can be dozens). Any of them can be
// launched or copied; only my own can be changed.
import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "react-router-dom";
import * as Menu from "@radix-ui/react-dropdown-menu";
import type { ColumnDef, SortingState } from "@tanstack/react-table";
import { ApiFailure } from "../../data/api";
import { useMe } from "../../data/queries";
import { Badge } from "../../design/Badge";
import { Button } from "../../design/Button";
import { ConfirmDialog } from "../../design/ConfirmDialog";
import { DataTable } from "../../design/DataTable";
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
const useConfigs = (scope: ConfigScope) =>
  useQuery({
    queryKey: configsKey(scope),
    queryFn: () => listConfigs(scope),
    retry: false,
  });

type Scope = "all" | "official" | "mine" | "shared" | "public";
const SCOPES: [Scope, string, string][] = [
  ["all", "全部", "没有符合的配置。"],
  ["official", "官方", "还没有官方配置。"],
  ["mine", "我的", "你还没有保存过配置。"],
  ["shared", "分享给我的", "没有人单独分享配置给你。"],
  ["public", "公开", "没有公开的配置。"],
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

const item =
  "cursor-pointer rounded px-2 py-1 outline-none data-[highlighted]:bg-sunk";

function RowMenu({
  c,
  admin,
  onOpen,
  onCopy,
  onDelete,
  onUnpublish,
  onOfficial,
}: {
  c: Row;
  admin: boolean;
  onOpen: () => void;
  onCopy: () => void;
  onDelete: () => void;
  onUnpublish: () => void;
  onOfficial: (official: boolean) => void;
}) {
  return (
    <Menu.Root>
      <Menu.Trigger
        aria-label={`${c.title} 更多操作`}
        className="rounded px-1.5 text-muted hover:bg-sunk hover:text-ink"
      >
        ⋯
      </Menu.Trigger>
      <Menu.Portal>
        <Menu.Content
          align="end"
          className="z-50 min-w-32 rounded-md border border-line bg-surface p-1 text-xs shadow-lg"
        >
          <Menu.Item className={item} onSelect={onOpen}>
            {c.mine ? "编辑" : "查看"}
          </Menu.Item>
          <Menu.Item className={item} onSelect={onCopy}>
            另存为副本
          </Menu.Item>
          {admin && c.public && (
            <Menu.Item
              className={item}
              onSelect={() => onOfficial(!c.official)}
            >
              {c.official ? "取消官方" : "设为官方"}
            </Menu.Item>
          )}
          {admin && !c.mine && c.public && (
            <Menu.Item className={item} onSelect={onUnpublish}>
              取消公开
            </Menu.Item>
          )}
          {(c.mine || admin) && (
            <Menu.Item
              className="cursor-pointer rounded px-2 py-1 text-fail outline-none data-[highlighted]:bg-fail-soft"
              onSelect={onDelete}
            >
              删除
            </Menu.Item>
          )}
        </Menu.Content>
      </Menu.Portal>
    </Menu.Root>
  );
}

export function ConfigsPage() {
  const me = useMe().data;
  const admin = !!me?.admin;
  const nav = useNavigate();
  const qc = useQueryClient();
  const [url, setUrl] = useUrlState(schema);
  const lists = {
    official: useConfigs("official"),
    mine: useConfigs("mine"),
    shared: useConfigs("shared"),
    public: useConfigs("public"),
  };
  const [forget, setForget] = useState<ConfigSummary | null>(null);
  const [error, setError] = useState<string | null>(null);
  const refresh = () => void qc.invalidateQueries({ queryKey: ["configs"] });
  const say = (e: unknown) =>
    setError(e instanceof ApiFailure ? e.body.message : String(e));
  const del = useMutation({
    mutationFn: (name: string) => deleteConfig(name),
    onError: say,
    onSettled: refresh,
  });
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

  const data = {
    official: lists.official.data?.items,
    mine: lists.mine.data?.items,
    shared: lists.shared.data?.items,
    public: lists.public.data?.items,
  };
  const used = useMemo(
    () => new Map(recentConfigs(me?.user ?? "").map((r) => [r.name, r.at])),
    [me?.user],
  );
  const byScope = useMemo(() => {
    const row = (c: ConfigSummary): Row => ({ ...c, usedAt: used.get(c.name) });
    const out = {} as Record<Scope, Row[]>;
    const seen = new Map<string, Row>();
    for (const k of ["official", "mine", "shared", "public"] as const) {
      out[k] = (data[k] ?? []).map(row);
      for (const c of out[k]) if (!seen.has(c.name)) seen.set(c.name, c);
    }
    out.all = [...seen.values()];
    return out;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [data.official, data.mine, data.shared, data.public, used]);

  const scope = (
    SCOPES.some(([k]) => k === url.scope) ? url.scope : "all"
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
              {c.mine && c.sharedWith.length > 0 && (
                <Badge title={c.sharedWith.join(", ")}>
                  分享给 {c.sharedWith.length} 人
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
        meta: { priority: 1, width: 130, align: "right" },
        cell: ({ row: { original: c } }) => (
          <span className="inline-flex items-center gap-2 whitespace-nowrap">
            <Link
              to={`/?new=1&config=${encodeURIComponent(c.name)}`}
              className="text-accent hover:underline"
            >
              用此配置创建
            </Link>
            <RowMenu
              c={c}
              admin={admin}
              onOpen={() => nav(`/configs/${c.name}`)}
              onCopy={() => void copy(c)}
              onDelete={() => setForget(c)}
              onUnpublish={() => unpublish.mutate(c.name)}
              onOfficial={(official) => mark.mutate({ name: c.name, official })}
            />
          </span>
        ),
      },
    ],
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [admin],
  );

  const all = Object.values(lists);
  const loading = all.some((q) => q.isLoading);
  const failed = all.every((q) => q.isError);
  const none = !loading && !failed && byScope.all.length === 0;
  const emptyText = url.q
    ? "没有符合的配置。"
    : SCOPES.find(([k]) => k === scope)![2];

  return (
    <div className="space-y-3 p-3">
      <header className="flex flex-wrap items-center gap-2">
        <div
          role="tablist"
          aria-label="范围"
          className="inline-flex rounded-md border border-line bg-sunk p-0.5 text-xs"
        >
          {SCOPES.map(([k, label]) => (
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
      {error && <Notice>{error}</Notice>}
      {failed ? (
        <EmptyState tone="error" title="加载失败" />
      ) : none ? (
        <EmptyState
          title="还没有保存过配置"
          hint="点「新建配置」，或在 DevPod 详情页点「存为配置」。把一份带变量的 DevPod YAML 存成配置，之后选配置、填变量就能创建。"
        />
      ) : (
        <DataTable<Row>
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
        />
      )}
      <ConfirmDialog
        open={!!forget}
        onOpenChange={(o) => !o && setForget(null)}
        title="删除这个配置？"
        confirmLabel="删除配置"
        danger
        onConfirm={() => {
          if (forget) del.mutate(forget.name);
          setForget(null);
        }}
      >
        <span className="mono text-ink">{forget?.name}</span> 会被删除
        {forget && !forget.mine ? `（${forget.owner} 的配置）` : ""}
        ；用它创建过的 DevPod 不受影响。
      </ConfirmDialog>
    </div>
  );
}
