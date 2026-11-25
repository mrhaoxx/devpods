// 预约管理 (admins): every user's node reservations as a list — filter by
// time, state, user and node; change a reservation's period or cancel it,
// or pick several (by box or by a drag) and cancel them together.
// (Which nodes may be reserved is set on 节点; the rules are the
// console's settings and are shown here as they are.)
import { useMemo, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { ColumnDef, SortingState } from "@tanstack/react-table";
import { ApiFailure, deleteReservation, getReservationRules, listReservations, patchReservation, type Reservation } from "../../data/api";
import { useClusterFilter } from "../../data/clusterFilter";
import { ActionMenu } from "../../design/ActionMenu";
import { Badge } from "../../design/Badge";
import { useBulkRun } from "../../design/BulkToast";
import { Button } from "../../design/Button";
import { Chip } from "../../design/Chip";
import { ConfirmDialog } from "../../design/ConfirmDialog";
import { copyText } from "../../design/Copy";
import { DataTable, type RowAction } from "../../design/DataTable";
import { EmptyState } from "../../design/EmptyState";
import { Field, Input, Notice } from "../../design/Form";
import { RefreshControl } from "../../design/RefreshControl";
import { useAutoRefresh } from "../../lib/autoRefresh";
import { fmtDuration } from "../../lib/format";
import { fmtAmount } from "../../lib/reservations";
import { useUrlState, type Schema } from "../../lib/urlState";

const DAY = 86400e3;
const RANGES: [string, string, number][] = [
  // id, label, days back (the end is as far ahead as one may reserve)
  ["ahead", "现在起", 0],
  ["7d", "近 7 天起", 7],
  ["30d", "近 30 天起", 30],
];
const PHASES: [Reservation["phase"], string, "run" | "idle" | "neutral" | "fail"][] = [
  ["Active", "进行中", "run"],
  ["Upcoming", "即将开始", "idle"],
  ["Ended", "已结束", "neutral"],
  ["Cancelled", "已取消", "fail"],
];
const schema = {
  range: { kind: "string", default: "ahead" },
  phase: { kind: "list", default: [] as string[] },
  user: { kind: "string", default: "" },
  node: { kind: "string", default: "" },
  sort: { kind: "string", default: "start" },
} satisfies Schema;

const at = (iso: string) => new Date(iso).toLocaleString("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" });
/** local: an ISO time as a datetime-local value. */
const local = (iso: string) => {
  const d = new Date(iso);
  const p = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
};
const isLive = (r: Reservation) => r.phase === "Upcoming" || r.phase === "Active";
const said = (e: unknown) => (e instanceof ApiFailure ? e.body.message : e instanceof Error ? e.message : String(e));

export function AdminReservationsPage() {
  const qc = useQueryClient();
  const [url, setUrl] = useUrlState(schema, { key: "admin.reservations" });
  const [every, setEvery] = useAutoRefresh("admin.reservations", 30_000);
  const rules = useQuery({ queryKey: ["reservation-rules"], queryFn: getReservationRules, staleTime: 300_000, retry: false });
  const back = (RANGES.find(([id]) => id === url.range) ?? RANGES[0])[2];
  // from the start of the range to as far ahead as anyone may reserve
  const window = useMemo(() => {
    const now = rules.data ? Date.parse(rules.data.now) : Date.now();
    const ahead = (rules.data?.maxAdvanceSeconds ?? 30 * 86400) * 1000;
    return { from: new Date(now - back * DAY - DAY).toISOString(), to: new Date(now + ahead + DAY).toISOString(), now };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [back, rules.data?.now, rules.data?.maxAdvanceSeconds]);
  const q = useQuery({
    queryKey: ["reservations", "admin", window.from, window.to],
    queryFn: () => listReservations(window.from, window.to),
    refetchInterval: every || false,
    enabled: !!rules.data,
  });
  const { matches, several } = useClusterFilter();
  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ["reservations"] });
    void qc.invalidateQueries({ queryKey: ["node-choices"] });
  };
  const [cancel, setCancel] = useState<Reservation | null>(null);
  const [cancelMany, setCancelMany] = useState<{ rows: Reservation[]; clear: () => void } | null>(null);
  const bulk = useBulkRun();
  // the ⋯ menu is in the (memoised) columns: it asks for what is current
  const rowActionsNow = useRef<(r: Reservation) => RowAction[]>(() => []);
  const [edit, setEdit] = useState<{ r: Reservation; start: string; end: string } | null>(null);
  const [error, setError] = useState("");
  const del = useMutation({ mutationFn: deleteReservation, onError: (e) => setError(said(e)), onSuccess: () => setError(""), onSettled: refresh });
  const move = useMutation({
    mutationFn: (v: { r: Reservation; start: string; end: string }) => patchReservation(v.r, { start: new Date(v.start).toISOString(), end: new Date(v.end).toISOString() }),
    onError: (e) => setError(said(e)),
    onSuccess: () => setError(""),
    onSettled: refresh,
  });

  const all = useMemo(() => (q.data?.items ?? []).filter(matches), [q.data, matches]);
  const inRange = useMemo(() => {
    const from = window.now - back * DAY;
    return all.filter((r) => Date.parse(r.end) >= from);
  }, [all, window.now, back]);
  const counts = useMemo(() => Object.fromEntries(PHASES.map(([p]) => [p, inRange.filter((r) => r.phase === p).length])), [inRange]);
  const shown = useMemo(() => {
    const u = url.user.trim().toLowerCase();
    const n = url.node.trim().toLowerCase();
    return inRange.filter(
      (r) => (!url.phase.length || url.phase.includes(r.phase)) && (!u || r.user.toLowerCase().includes(u)) && (!n || r.node.toLowerCase().includes(n)),
    );
  }, [inRange, url.phase, url.user, url.node]);
  const sorting: SortingState = url.sort ? [{ id: url.sort.replace(/^-/, ""), desc: url.sort.startsWith("-") }] : [];

  const columns = useMemo<ColumnDef<Reservation, unknown>[]>(
    () => [
      ...(several ? [{ id: "cluster", header: "集群", accessorFn: (r: Reservation) => r.cluster ?? "", meta: { priority: 2, width: 90, mono: true } }] : []),
      { id: "node", header: "节点", accessorKey: "node", meta: { priority: 1, width: 150, mono: true } },
      { id: "user", header: "用户", accessorKey: "user", meta: { priority: 1, width: 110, mono: true } },
      { id: "start", header: "开始", accessorFn: (r) => Date.parse(r.start), meta: { priority: 1, width: 110 }, cell: ({ row }) => at(row.original.start) },
      { id: "end", header: "结束", accessorFn: (r) => Date.parse(r.end), meta: { priority: 2, width: 110 }, cell: ({ row }) => at(row.original.end) },
      {
        id: "length",
        header: "时长",
        accessorFn: (r) => Date.parse(r.end) - Date.parse(r.start),
        meta: { priority: 3, width: 80, mono: true },
        cell: ({ getValue }) => fmtDuration(getValue() as number),
      },
      {
        id: "amount",
        header: "资源",
        accessorFn: (r) => fmtAmount(r),
        meta: { priority: 2, width: 130 },
        cell: ({ row: { original: r } }) => (
          <span className="inline-flex items-center gap-1.5">
            <span>{fmtAmount(r)}</span>
            {r.overbooked && (
              <span className="text-[11px] font-medium text-fail" title="与其他预约重叠，超出机器容量（v2 之前的预约）">
                超额
              </span>
            )}
          </span>
        ),
      },
      {
        id: "phase",
        header: "状态",
        accessorKey: "phase",
        meta: { priority: 1, width: 110 },
        cell: ({ row: { original: r } }) => {
          const [, label, tone] = PHASES.find(([p]) => p === r.phase) ?? [r.phase, r.phase, "neutral"];
          return (
            <span className="inline-flex items-center gap-1.5">
              <Badge tone={tone}>{label}</Badge>
              {r.cancelledBy && <span className="text-[11px] text-muted">由 {r.cancelledBy}</span>}
            </span>
          );
        },
      },
      {
        id: "actions",
        header: "",
        enableSorting: false,
        meta: { priority: 1, width: 48, align: "right", pin: "right" },
        cell: ({ row: { original: r } }) => <ActionMenu label={`${r.user} ${r.node} 操作`} actions={() => rowActionsNow.current(r)} />,
      },
    ],
    [several],
  );

  // what can be done to a reservation, a selection: said once, offered by
  // the ⋯ menu, the right-click menu and the bulk bar alike
  const editOf = (r: Reservation) => setEdit({ r, start: local(r.start), end: local(r.end) });
  const rowActions = (r: Reservation): RowAction[] => [
    ...(isLive(r)
      ? [
          { label: "改时间", onSelect: () => editOf(r) },
          { label: "取消", danger: true, onSelect: () => setCancel(r) },
        ]
      : []),
    { label: "复制节点名", onSelect: () => void copyText(r.node) },
    { label: "复制用户名", onSelect: () => void copyText(r.user) },
  ];
  rowActionsNow.current = rowActions;
  const bulkActions = (rows: Reservation[], clear: () => void): RowAction[] => {
    const can = rows.filter(isLive);
    return [
      {
        label: `取消所选（${can.length}）`,
        danger: true,
        disabled: can.length === 0,
        title: can.length === 0 ? "所选的都已结束或已取消" : undefined,
        onSelect: () => setCancelMany({ rows: can, clear }),
      },
      { label: "复制节点名", onSelect: () => void copyText([...new Set(rows.map((r) => r.node))].join("\n")) },
    ];
  };
  const cancelAll = async (v: { rows: Reservation[]; clear: () => void }) => {
    setCancelMany(null);
    await bulk.run("取消预约", v.rows, (r) => deleteReservation(r).catch((e) => Promise.reject(new Error(said(e)))), (r) => `${r.user} ${r.node}`);
    v.clear();
    refresh();
  };

  if (rules.isError) return <EmptyState tone="error" title="节点预约不可用" hint={said(rules.error)} />;
  const ru = rules.data;
  const span = (s: number) => fmtDuration(s * 1000);
  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex flex-wrap items-center gap-2 border-b border-line px-3 py-2">
        <div role="radiogroup" aria-label="时间范围" className="inline-flex rounded-md border border-line bg-sunk p-0.5 text-xs">
          {RANGES.map(([id, label]) => (
            <button
              key={id}
              type="button"
              role="radio"
              aria-checked={url.range === id}
              onClick={() => setUrl({ range: id })}
              className={`rounded px-2.5 py-0.5 ${url.range === id ? "bg-surface font-medium shadow-sm" : "text-muted hover:text-ink"}`}
            >
              {label}
            </button>
          ))}
        </div>
        {PHASES.map(([p, label]) => (
          <Chip key={p} active={url.phase.includes(p)} count={counts[p]} onClick={() => setUrl({ phase: url.phase.includes(p) ? url.phase.filter((x) => x !== p) : [...url.phase, p] })}>
            {label}
          </Chip>
        ))}
        <Input aria-label="按用户过滤" placeholder="用户" value={url.user} onChange={(e) => setUrl({ user: e.target.value })} className="mono !h-7 w-28 text-xs" />
        <Input aria-label="按节点过滤" placeholder="节点" value={url.node} onChange={(e) => setUrl({ node: e.target.value })} className="mono !h-7 w-32 text-xs" />
        <span className="flex-1" />
        <RefreshControl every={every} onEvery={setEvery} onRefresh={() => void q.refetch()} busy={q.isFetching} />
      </div>
      {ru && (
        <p className="border-b border-line px-3 py-1.5 text-[11px] text-muted">
          规则：每格 {span(ru.granularitySeconds)} · 一次最长 {ru.maxDurationSeconds > 0 ? span(ru.maxDurationSeconds) : "不限"} · 最多提前 {span(ru.maxAdvanceSeconds)} · 开始前 {span(ru.clearNoticeSeconds)}{" "}
          提醒节点上的其他 DevPod · 时区 {ru.timezone}。哪些节点可以预约在 <Link to="/admin/nodes" className="text-accent hover:underline">节点</Link> 里设置。
        </p>
      )}
      {error && (
        <div className="border-b border-line px-3 py-1.5">
          <Notice tone="warm">{error}</Notice>
        </div>
      )}
      <DataTable<Reservation>
        fill
        storageKey="admin-reservations"
        data={shown}
        columns={columns}
        getRowId={(r) => `${r.cluster ?? ""}/${r.id}`}
        sorting={sorting}
        onSortingChange={(s) => setUrl({ sort: s[0] ? `${s[0].desc ? "-" : ""}${s[0].id}` : "" })}
        empty={<EmptyState title={q.isLoading || !ru ? "加载中…" : "没有符合条件的预约"} />}
        selectable
        onRowClick={(r) => isLive(r) && editOf(r)}
        bulkBar={(rows, clear) =>
          bulkActions(rows, clear).map((a) => (
            <Button key={a.label} variant={a.danger ? "danger" : undefined} disabled={a.disabled} title={a.title} onClick={a.onSelect}>
              {a.label}
            </Button>
          ))
        }
        rowMenu={(rows, isBulk, clear) => (isBulk ? bulkActions(rows, clear) : rowActions(rows[0]))}
      />
      <ConfirmDialog
        open={!!cancelMany}
        onOpenChange={(o) => !o && setCancelMany(null)}
        title={`取消 ${cancelMany?.rows.length ?? 0} 份预约？`}
        confirmLabel={`取消 ${cancelMany?.rows.length ?? 0} 份预约`}
        danger
        onConfirm={() => cancelMany && void cancelAll(cancelMany)}
      >
        {cancelMany && (() => {
          const on = cancelMany.rows.filter((r) => r.phase === "Active").length;
          const users = new Set(cancelMany.rows.map((r) => r.user)).size;
          return `${users} 个用户的预约。${on > 0 ? `其中 ${on} 份正在进行，取消后那些节点立刻对其他人开放。` : ""}预约人会看到是你取消的。`;
        })()}
      </ConfirmDialog>
      {bulk.toast}
      <ConfirmDialog
        open={!!cancel}
        onOpenChange={(o) => !o && setCancel(null)}
        title={`取消 ${cancel?.user} 在 ${cancel?.node} 上的预约？`}
        confirmLabel="取消预约"
        danger
        onConfirm={() => {
          const r = cancel!;
          setCancel(null);
          del.mutate(r);
        }}
      >
        {cancel && `${at(cancel.start)} – ${at(cancel.end)}。`}
        {cancel?.phase === "Active" && "预约正在进行，取消后节点立刻对其他人开放。"}
        预约人会看到是你取消的。
      </ConfirmDialog>
      <ConfirmDialog
        open={!!edit}
        onOpenChange={(o) => !o && setEdit(null)}
        title={`改 ${edit?.r.user} 在 ${edit?.r.node} 上的预约时间`}
        confirmLabel="保存"
        onConfirm={() => {
          const v = edit!;
          if (!(Date.parse(v.end) > Date.parse(v.start))) {
            setError("结束时间要晚于开始时间");
            return;
          }
          setEdit(null);
          move.mutate(v);
        }}
      >
        {edit && (
          <div className="grid gap-2 sm:grid-cols-2">
            <Field label="开始">
              <Input type="datetime-local" aria-label="开始" value={edit.start} disabled={edit.r.phase === "Active"} onChange={(e) => setEdit({ ...edit, start: e.target.value })} />
            </Field>
            <Field label="结束">
              <Input type="datetime-local" aria-label="结束" value={edit.end} onChange={(e) => setEdit({ ...edit, end: e.target.value })} />
            </Field>
            <p className="text-[11px] text-muted sm:col-span-2">按每格 {ru ? span(ru.granularitySeconds) : ""} 对齐；进行中的预约只能改结束时间。</p>
          </div>
        )}
      </ConfirmDialog>
    </div>
  );
}
