// 我的镜像 / 全部镜像: the images saved from DevPods, in the list table —
// search, narrow by state, sort; copy the address, start a DevPod from
// one, watch one still being saved, try a failed one again, forget one or
// several (by box, a drag, or Ctrl+A).
import { useClusterFilter } from "../../data/clusterFilter";
import { devpodHref } from "../../data/cluster";
import { useMemo, useRef, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "react-router-dom";
import type { ColumnDef, SortingState } from "@tanstack/react-table";
import { ApiFailure, deleteSnapshot, retrySnapshot, type SnapshotView } from "../../data/api";
import { useMe, useSnapshots } from "../../data/queries";
import { ActionMenu } from "../../design/ActionMenu";
import { Badge } from "../../design/Badge";
import { useBulkRun } from "../../design/BulkToast";
import { Button } from "../../design/Button";
import { Chip } from "../../design/Chip";
import { ConfirmDialog } from "../../design/ConfirmDialog";
import { copyText } from "../../design/Copy";
import { DataTable, type RowAction } from "../../design/DataTable";
import { EmptyState } from "../../design/EmptyState";
import { Input } from "../../design/Form";
import { fmtAge, fmtBytes } from "../../lib/format";
import { useUrlState, type Schema } from "../../lib/urlState";
import { SnapshotDialog } from "./SnapshotDialog";

const phaseBadge: Record<SnapshotView["phase"], { tone: "neutral" | "run" | "fail" | "warm"; label: string }> = {
  Pending: { tone: "warm", label: "等待中" },
  Running: { tone: "warm", label: "保存中" },
  Succeeded: { tone: "run", label: "已保存" },
  Failed: { tone: "fail", label: "失败" },
};
// the state chips: one still being saved is 保存中 whether it has begun or not
const STATES: [string, string, (s: SnapshotView) => boolean][] = [
  ["ok", "已保存", (s) => s.phase === "Succeeded"],
  ["busy", "保存中", (s) => s.phase === "Pending" || s.phase === "Running"],
  ["failed", "失败", (s) => s.phase === "Failed"],
];
const schema = {
  q: { kind: "string", default: "" },
  state: { kind: "list", default: [] as string[] },
  sort: { kind: "string", default: "-time" },
} satisfies Schema;

const busy = (s: SnapshotView) => s.phase === "Pending" || s.phase === "Running";
const keyOfSnap = (s: SnapshotView) => `${s.cluster ?? ""}/${s.id}`;
const newHref = (s: SnapshotView) => `/?new=1&image=${encodeURIComponent(s.image)}${s.cluster ? `&in=${encodeURIComponent(s.cluster)}` : ""}`;
const said = (e: unknown) => (e instanceof ApiFailure ? e.body.message : e instanceof Error ? e.message : String(e));

/** everyone: the admins' page — every user's snapshots, with their owner. */
export function ImagesPage({ everyone }: { everyone?: boolean }) {
  const me = useMe().data;
  const qc = useQueryClient();
  const nav = useNavigate();
  const all = !!everyone && !!me?.admin;
  const q = useSnapshots({ all });
  const [url, setUrl] = useUrlState(schema, { key: all ? "admin.images" : "images" });
  const [follow, setFollow] = useState<SnapshotView | null>(null);
  const [forget, setForget] = useState<{ rows: SnapshotView[]; clear?: () => void } | null>(null);
  const bulk = useBulkRun();
  const retry = useMutation({ mutationFn: (s: SnapshotView) => retrySnapshot(s), onSettled: () => void qc.invalidateQueries({ queryKey: ["snapshots"] }) });
  const { matches, several } = useClusterFilter();
  const items = useMemo(() => (q.data?.items ?? []).filter(matches), [q.data, matches]);
  const counts = useMemo(() => Object.fromEntries(STATES.map(([id, , is]) => [id, items.filter(is).length])), [items]);
  const shown = useMemo(() => {
    const t = url.q.trim().toLowerCase();
    const states = STATES.filter(([id]) => url.state.includes(id));
    return items.filter(
      (s) => (!states.length || states.some(([, , is]) => is(s))) && (!t || [s.image, s.devpod, s.owner ?? "", s.cluster ?? ""].some((x) => x.toLowerCase().includes(t))),
    );
  }, [items, url.q, url.state]);
  const sorting: SortingState = url.sort ? [{ id: url.sort.replace(/^-/, ""), desc: url.sort.startsWith("-") }] : [];

  // what can be done to an image, a selection: said once, offered by the ⋯
  // menu, the right-click menu and the bulk bar alike
  const rowActions = (s: SnapshotView): RowAction[] => [
    ...(busy(s) ? [{ label: "查看进度", onSelect: () => setFollow(s) }] : []),
    ...(s.phase === "Succeeded"
      ? [
          { label: "新建 DevPod", onSelect: () => nav(newHref(s)) },
          { label: "复制地址", onSelect: () => void copyText(s.image) },
        ]
      : []),
    ...(s.phase === "Failed" ? [{ label: "重试", disabled: retry.isPending, onSelect: () => retry.mutate(s) }] : []),
    { label: "打开来源 DevPod", onSelect: () => nav(devpodHref({ cluster: s.cluster, name: s.devpod })) },
    ...(!busy(s) ? [{ label: "删除记录", danger: true, onSelect: () => setForget({ rows: [s] }) }] : []),
  ];
  const rowActionsNow = useRef(rowActions);
  rowActionsNow.current = rowActions;
  const bulkActions = (rows: SnapshotView[], clear: () => void): RowAction[] => {
    const done = rows.filter((s) => !busy(s));
    const saved = rows.filter((s) => s.phase === "Succeeded");
    return [
      ...(saved.length ? [{ label: "复制地址", onSelect: () => void copyText(saved.map((s) => s.image).join("\n")) }] : []),
      {
        label: `删除记录（${done.length}）`,
        danger: true,
        disabled: done.length === 0,
        title: done.length === 0 ? "所选的都还在保存" : undefined,
        onSelect: () => setForget({ rows: done, clear }),
      },
    ];
  };
  const forgetAll = async (v: { rows: SnapshotView[]; clear?: () => void }) => {
    setForget(null);
    await bulk.run("删除记录", v.rows, (s) => deleteSnapshot(s).catch((e) => Promise.reject(new Error(said(e)))), (s) => s.image);
    v.clear?.();
    void qc.invalidateQueries({ queryKey: ["snapshots"] });
  };

  const columns = useMemo<ColumnDef<SnapshotView, unknown>[]>(
    () => [
      {
        id: "image",
        header: "镜像",
        accessorFn: (s) => s.image,
        meta: { priority: 1, width: 380 },
        cell: ({ row: { original: s } }) => (
          <div className="min-w-0">
            <div className="mono truncate" title={s.image}>
              {s.image}
            </div>
            {s.customRegistry && <div className="text-[11px] text-faint">自定义仓库</div>}
            {s.phase === "Failed" && s.message && (
              <div className="truncate text-[11px] text-fail" title={s.message}>
                {s.message}
              </div>
            )}
          </div>
        ),
      },
      ...(several ? [{ id: "cluster", header: "集群", accessorFn: (s: SnapshotView) => s.cluster ?? "", meta: { priority: 2, width: 90, mono: true } }] : []),
      ...(all ? [{ id: "owner", header: "用户", accessorFn: (s: SnapshotView) => s.owner ?? "", meta: { priority: 1, width: 110, mono: true } }] : []),
      {
        id: "devpod",
        header: "来源 DevPod",
        accessorFn: (s) => s.devpod,
        meta: { priority: 2, width: 160 },
        cell: ({ row: { original: s } }) => (
          <Link to={devpodHref({ cluster: s.cluster, name: s.devpod })} className="mono text-ink hover:text-accent">
            {s.devpod}
          </Link>
        ),
      },
      { id: "size", header: "大小", accessorFn: (s) => s.sizeBytes ?? -1, meta: { priority: 3, width: 80, mono: true, align: "right" }, cell: ({ row: { original: s } }) => (s.sizeBytes ? fmtBytes(s.sizeBytes) : "—") },
      {
        id: "time",
        header: "时间",
        accessorFn: (s) => Date.parse(s.createdAt),
        sortDescFirst: true,
        meta: { priority: 2, width: 90 },
        cell: ({ row: { original: s } }) => <span title={new Date(s.createdAt).toLocaleString()}>{fmtAge(s.createdAt)} 前</span>,
      },
      {
        id: "phase",
        header: "状态",
        accessorFn: (s) => s.phase,
        meta: { priority: 1, width: 80 },
        cell: ({ row: { original: s } }) => <Badge tone={phaseBadge[s.phase].tone}>{phaseBadge[s.phase].label}</Badge>,
      },
      {
        // the one thing to do now, in sight; the rest under ⋯
        id: "go",
        header: "",
        enableSorting: false,
        meta: { priority: 1, width: 96, align: "right" },
        cell: ({ row: { original: s } }) =>
          busy(s) ? (
            <button type="button" onClick={() => setFollow(s)} className="text-accent hover:underline">
              查看进度
            </button>
          ) : s.phase === "Succeeded" ? (
            <Link to={newHref(s)} className="text-accent hover:underline">
              新建 DevPod
            </Link>
          ) : (
            <button type="button" disabled={retry.isPending} onClick={() => retry.mutate(s)} className="text-accent hover:underline disabled:opacity-50" title="用同样的设置再保存一次">
              重试
            </button>
          ),
      },
      { id: "actions", header: "", enableSorting: false, meta: { priority: 1, width: 48, align: "right", pin: "right" }, cell: ({ row }) => <ActionMenu label={`${row.original.image} 操作`} actions={() => rowActionsNow.current(row.original)} /> },
    ],
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [several, all, retry.isPending],
  );

  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex flex-wrap items-center gap-2 border-b border-line px-3 py-2">
        <h1 className="text-sm font-semibold">
          {all ? "全部镜像" : "我的镜像"} <span className="mono text-faint">{items.length}</span>
        </h1>
        <Input aria-label="搜索镜像" placeholder={all ? "地址、DevPod、用户…" : "地址、DevPod…"} value={url.q} onChange={(e) => setUrl({ q: e.target.value })} className="!h-7 w-56 text-xs" />
        {STATES.map(([id, label]) => (
          <Chip key={id} active={url.state.includes(id)} count={counts[id]} onClick={() => setUrl({ state: url.state.includes(id) ? url.state.filter((x) => x !== id) : [...url.state, id] })}>
            {label}
          </Chip>
        ))}
      </div>
      <p className="border-b border-line px-3 py-1.5 text-[11px] text-muted">
        在 DevPod 详情页点「保存为镜像」，把装好环境的容器存成镜像；之后新建 DevPod 时可以直接选它。镜像不包含持久卷（home 目录）。
      </p>
      {retry.isError && (
        <p role="alert" className="border-b border-line bg-fail-soft px-3 py-1.5 text-xs text-fail">
          重试失败：{said(retry.error)}
        </p>
      )}
      {q.isError ? (
        <EmptyState tone="error" title="加载失败" />
      ) : (
        <DataTable<SnapshotView>
          fill
          label={all ? "全部镜像" : "我的镜像"}
          storageKey={all ? "admin-images" : "images"}
          data={shown}
          columns={columns}
          getRowId={keyOfSnap}
          sorting={sorting}
          onSortingChange={(s) => setUrl({ sort: s[0] ? `${s[0].desc ? "-" : ""}${s[0].id}` : "" })}
          empty={<EmptyState title={q.isLoading ? "加载中…" : items.length ? "没有符合条件的镜像" : "还没有保存过镜像"} />}
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
      {follow && <SnapshotDialog devpod={{ cluster: follow.cluster, name: follow.devpod }} owner={follow.owner} following={follow} onClose={() => setFollow(null)} />}
      <ConfirmDialog
        open={!!forget}
        onOpenChange={(o) => !o && setForget(null)}
        title={forget && forget.rows.length > 1 ? `删除 ${forget.rows.length} 条镜像记录？` : "删除这条镜像记录？"}
        confirmLabel={forget && forget.rows.length > 1 ? `删除 ${forget.rows.length} 条记录` : "删除记录"}
        danger
        onConfirm={() => forget && void forgetAll(forget)}
      >
        {forget && forget.rows.length > 1 ? (
          "只删除控制台里的记录，仓库里的镜像不会被删除。"
        ) : (
          <>
            只删除控制台里的记录，仓库里的镜像 <span className="mono text-ink">{forget?.rows[0]?.image}</span> 不会被删除。
          </>
        )}
      </ConfirmDialog>
      {bulk.toast}
    </div>
  );
}
