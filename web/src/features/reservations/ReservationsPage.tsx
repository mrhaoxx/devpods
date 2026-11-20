// 节点预约 (UI spec §5.7; access control design §12): a timeline of the
// reservable machines the access rules ask me a reservation for — drag to
// reserve, click a block of your own to change or release it — plus the
// list of my reservations and a plain form (the keyboard path). A
// reservation is a permit, not a hold: they may overlap, drawn in lanes.
import { keyOf, refOfKey } from "../../data/cluster";
import { useClusterFilter } from "../../data/clusterFilter";
import { useMemo, useRef, useState } from "react";
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ApiFailure,
  createReservation,
  deleteReservation,
  getReservationRules,
  listMyReservations,
  listReservations,
  patchReservation,
  type NodeChoice,
  type Reservation,
  type ReservationRules,
} from "../../data/api";
import { useMe, useNodeChoices } from "../../data/queries";
import type { ColumnDef, SortingState } from "@tanstack/react-table";
import { ActionMenu } from "../../design/ActionMenu";
import { Badge } from "../../design/Badge";
import { useBulkRun } from "../../design/BulkToast";
import { DataTable, type RowAction } from "../../design/DataTable";
import { copyText } from "../../design/Copy";
import { Button } from "../../design/Button";
import { ConfirmDialog } from "../../design/ConfirmDialog";
import { EmptyState } from "../../design/EmptyState";
import { Field, Input, Notice, Select } from "../../design/Form";
import { Panel } from "../../design/Panel";
import { useNow } from "../../lib/useNow";
import { LoadMore } from "../audit/common";
import { dayStart, fmtClock, fmtDay, fmtSpan, fmtTime, fromInput, fromNow, periodError, selection, slotStart, toInput } from "../../lib/reservations";
import * as Dialog from "@radix-ui/react-dialog";

const DAY = 86_400_000;
const RANGES = [3, 7, 14] as const;
const phaseLabel: Record<Reservation["phase"], string> = { Upcoming: "即将开始", Active: "进行中", Ended: "已结束", Cancelled: "已取消" };

/** apiMessage turns a reservation API failure into a sentence. */
function apiMessage(e: unknown): string {
  if (e instanceof ApiFailure) {
    if (e.code === "NOT_RESERVABLE") return "这台机器没有开放预约。";
    if (e.code === "NO_NEED") return "你可以直接使用这台机器，不需要预约。";
    if (e.code === "ACL_DENIED") return "你没有使用这台机器的权限。";
    return e.body.message;
  }
  return String(e);
}

/** node: the node's key, <cluster>/<name> in a console of several clusters */
type Draft = { node: string; start: number; end: number; edit?: Reservation; nodes?: string[] };
const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const nodeKey = (b: Reservation) => keyOf({ cluster: b.cluster, name: b.node });
const resKey = (b: Reservation) => `${b.cluster ?? ""}/${b.id}`;
const isLive = (b: Reservation) => b.phase === "Upcoming" || b.phase === "Active";
const phaseOrder: Record<Reservation["phase"], number> = { Active: 0, Upcoming: 1, Ended: 2, Cancelled: 3 };

/** the columns of my reservations: a list like the DevPod list */
function mineColumns(user: () => string, actions: (b: Reservation) => React.ReactNode): ColumnDef<Reservation, unknown>[] {
  return [
    {
      id: "node",
      header: "节点",
      accessorFn: (b) => `${b.cluster ?? ""} ${b.node}`,
      meta: { priority: 1, width: 260 },
      cell: ({ row: { original: b } }) => (
        <span className="mono" title={b.cluster ? `${b.cluster} · ${b.node}` : b.node}>
          {b.cluster && <span className="text-muted">{b.cluster} </span>}
          {b.node}
        </span>
      ),
    },
    { id: "start", header: "开始", accessorFn: (b) => Date.parse(b.start), meta: { priority: 1, width: 130, mono: true }, cell: ({ row }) => fmtTime(row.original.start) },
    { id: "end", header: "结束", accessorFn: (b) => Date.parse(b.end), meta: { priority: 2, width: 130, mono: true }, cell: ({ row }) => fmtTime(row.original.end) },
    {
      id: "length",
      header: "时长",
      accessorFn: (b) => Date.parse(b.end) - Date.parse(b.start),
      meta: { priority: 3, width: 110 },
      cell: ({ getValue }) => fmtSpan(getValue() as number),
    },
    {
      id: "phase",
      header: "状态",
      accessorFn: (b) => phaseOrder[b.phase],
      meta: { priority: 1, width: 170 },
      cell: ({ row: { original: b } }) => (
        <>
          <Badge tone={b.phase === "Active" ? "run" : b.phase === "Upcoming" ? "accent" : "neutral"}>{phaseLabel[b.phase]}</Badge>
          {b.phase === "Cancelled" && b.cancelledBy && b.cancelledBy !== user() && <span className="ml-1 text-muted">由 {b.cancelledBy} 取消</span>}
        </>
      ),
    },
    { id: "actions", header: "", enableSorting: false, meta: { priority: 1, width: 48, align: "right", pin: "right" }, cell: ({ row }) => actions(row.original) },
  ];
}

/** The form: new reservation, move (not started) or move the end of one that has started. */
function ReserveDialog({ draft, nodes, rules, now, onClose }: { draft: Draft; nodes: NodeChoice[]; rules: ReservationRules; now: number; onClose: () => void }) {
  const qc = useQueryClient();
  const edit = draft.edit;
  const active = edit?.phase === "Active";
  const [node, setNode] = useState(draft.node);
  const ref = refOfKey(node);
  const several = nodes.some((n) => n.cluster);
  // a new one may be for several machines at once: one reservation each
  const [picked, setPicked] = useState<string[]>(draft.nodes ?? [draft.node]);
  const [outcome, setOutcome] = useState<{ made: number; failed: { key: string; why: string }[] } | null>(null);
  const nameOf = (key: string) => {
    const r = refOfKey(key);
    return several && r.cluster ? `${r.cluster} · ${r.name}` : r.name;
  };
  const [error, setError] = useState<string | null>(null);
  const g = rules.granularitySeconds * 1000;
  // the system's own pickers (no step: Safari's handles it badly); what
  // is typed is rounded out to whole steps, and the period said below
  const [start, setStart] = useState(toInput(draft.start));
  const [end, setEnd] = useState(toInput(draft.end));
  const s = active ? Date.parse(edit!.start) : slotStart(fromInput(start), g);
  const rawEnd = fromInput(end);
  const e = Number.isNaN(rawEnd) ? NaN : slotStart(rawEnd + g - 1, g);
  const problem = active
    ? Number.isNaN(e)
      ? "请填写结束时间"
      : e <= now
        ? "结束时间要在现在之后（要立刻结束请提前释放）"
        : rules.maxDurationSeconds > 0 && e - Date.parse(edit!.start) > rules.maxDurationSeconds * 1000
          ? `单次最长 ${fmtSpan(rules.maxDurationSeconds * 1000)}`
          : null
    : periodError(s, e, rules, now);
  const period = { node: ref.name, start: new Date(s).toISOString(), end: new Date(e).toISOString() };
  const done = () => {
    void qc.invalidateQueries({ queryKey: ["reservations"] });
    void qc.invalidateQueries({ queryKey: ["node-choices"] });
    onClose();
  };
  function touch() {
    setError(null);
    setOutcome(null);
  }
  const save = useMutation({
    mutationFn: async () => {
      if (edit) {
        await patchReservation(edit, active ? { end: period.end } : { start: period.start, end: period.end });
        return { made: 1, failed: [] };
      }
      const keys = picked;
      const results = await Promise.allSettled(keys.map((k) => createReservation(refOfKey(k).cluster, { ...period, node: refOfKey(k).name })));
      const failed = results.flatMap((r, i) => (r.status === "rejected" ? [{ key: keys[i], why: apiMessage(r.reason) }] : []));
      return { made: keys.length - failed.length, failed };
    },
    onSuccess: (r) => {
      if (r.failed.length === 0) return done();
      // those made are made; the rest stay picked, to try again
      void qc.invalidateQueries({ queryKey: ["reservations"] });
      setOutcome(r);
      setPicked(r.failed.map((f) => f.key));
    },
    onError: (err) => setError(apiMessage(err)),
  });
  return (
    <Dialog.Root open onOpenChange={(o) => !o && onClose()}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/30" />
        <Dialog.Content aria-describedby={undefined} className="fixed left-1/2 top-1/4 z-50 w-[min(92vw,440px)] -translate-x-1/2 space-y-3 rounded-lg border border-line bg-surface p-4 shadow-xl">
          <Dialog.Title className="text-sm font-semibold">{edit ? "修改预约" : "预约节点"}</Dialog.Title>
          <form
            className="space-y-3"
            onSubmit={(ev) => {
              ev.preventDefault();
              if (problem) return;
              save.mutate();
            }}
          >
            {edit ? (
              <Field label="节点">
                <Select aria-label="节点" value={node} disabled onChange={(ev) => (setNode(ev.target.value), touch())}>
                  {nodes.map((n) => (
                    <option key={keyOf(n)} value={keyOf(n)}>
                      {several && n.cluster ? `${n.cluster} · ` : ""}
                      {n.name}
                      {n.gpuTotal > 0 ? ` · ${n.gpuModel ?? "GPU"} ×${n.gpuTotal}` : ""}
                    </option>
                  ))}
                </Select>
              </Field>
            ) : (
              <fieldset className="space-y-1">
                <legend className="mb-1 flex w-full items-center justify-between text-xs font-medium">
                  <span>节点（可多选，已选 {picked.length} 台）</span>
                  <span className="flex gap-2 font-normal">
                    <button type="button" className="text-accent hover:underline" onClick={() => (setPicked(nodes.map(keyOf)), touch())}>
                      全选
                    </button>
                    <button type="button" className="text-accent hover:underline" onClick={() => (setPicked([]), touch())}>
                      清空
                    </button>
                  </span>
                </legend>
                <div className="relative max-h-48 space-y-0.5 overflow-auto rounded-md border border-line p-1.5">
                  {nodes.map((n) => {
                    const k = keyOf(n);
                    return (
                      <label key={k} className="flex items-center gap-2 rounded px-1 py-0.5 text-xs hover:bg-sunk">
                        <input
                          type="checkbox"
                          aria-label={nameOf(k)}
                          checked={picked.includes(k)}
                          onChange={(ev) => (setPicked((cur) => (ev.target.checked ? [...cur, k] : cur.filter((x) => x !== k))), touch())}
                        />
                        <span className="mono break-all">{nameOf(k)}</span>
                        {n.gpuTotal > 0 && <span className="text-muted">{`${n.gpuModel ?? "GPU"} ×${n.gpuTotal}`}</span>}
                      </label>
                    );
                  })}
                </div>
              </fieldset>
            )}
            <div className="grid gap-3 sm:grid-cols-2">
              <Field label="开始">
                <Input aria-label="开始" type="datetime-local" value={start} disabled={active} onChange={(ev) => (setStart(ev.target.value), touch())} />
              </Field>
              <Field label="结束">
                <Input aria-label="结束" type="datetime-local" value={end} onChange={(ev) => (setEnd(ev.target.value), touch())} />
              </Field>
            </div>
            {!problem && (
              <p className="text-xs">
                实际预约：{fmtDay(s)} {fmtClock(s)} – {fmtDay(e)} {fmtClock(e)}，{fmtSpan(e - s)}
              </p>
            )}
            <p className="text-[11px] text-muted">
              北京时间，{fmtSpan(g)}一档，不在整档上的时间自动取整
              {rules.maxDurationSeconds > 0 && `，单次最长 ${fmtSpan(rules.maxDurationSeconds * 1000)}`}
              {rules.maxAdvanceSeconds > 0 && `，最多提前 ${fmtSpan(rules.maxAdvanceSeconds * 1000)}`}。
            </p>
            {problem && <Notice>{problem}</Notice>}
            {error && <Notice>{error}</Notice>}
            {outcome && (
              <Notice tone="warm">
                {outcome.made} 台已预约；以下没有成功，仍勾选着，可以改了再试：
                {outcome.failed.map((f) => (
                  <span key={f.key} className="block">
                    {nameOf(f.key)}：{f.why}
                  </span>
                ))}
              </Notice>
            )}
            {!edit && (
              <p className="text-[11px] text-muted">
                预约是使用许可，不独占：预约期间你可以在这台机器上运行 DevPod，别人（不需要预约的人、同时段的其他预约人）照样可以用。到期后，你在这台机器上只靠这份预约运行的 DevPod 会被自动休眠。
              </p>
            )}
            <div className="flex justify-end gap-2">
              <Dialog.Close asChild>
                <Button type="button">取消</Button>
              </Dialog.Close>
              <Button type="submit" variant="primary" disabled={!!problem || save.isPending || (!edit && picked.length === 0)}>
                {edit ? "保存" : picked.length > 1 ? `预约 ${picked.length} 台` : "预约"}
              </Button>
            </div>
          </form>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

/** One node's row: blocks, the now line, and drag-to-reserve on free time. */
function Row({
  node,
  blocks,
  from,
  to,
  now,
  gran,
  admin,
  index,
  sel,
  bad,
  label,
  onPick,
}: {
  node: NodeChoice;
  blocks: Reservation[];
  from: number;
  to: number;
  now: number;
  gran: number;
  admin: boolean;
  /** this row's place: what a drag across rows reads */
  index: number;
  /** the time a drag picks, when the drag takes this row in */
  sel: [number, number] | null;
  bad: boolean;
  /** the drag's time is said on the top row it takes in only */
  label: boolean;
  onPick: (b: Reservation) => void;
}) {
  void gran;
  const pct = (t: number) => `${Math.min(100, Math.max(0, ((t - from) / (to - from)) * 100))}%`;
  // reservations may overlap: each in the first lane free at its start
  const { lane, lanes } = useMemo(() => {
    const ends: number[] = [];
    const lane = new Map<string, number>();
    for (const b of [...blocks].sort((x, y) => Date.parse(x.start) - Date.parse(y.start))) {
      let i = ends.findIndex((e) => e <= Date.parse(b.start));
      if (i < 0) i = ends.push(0) - 1;
      ends[i] = Date.parse(b.end);
      lane.set(b.id, i);
    }
    return { lane, lanes: Math.max(1, ends.length) };
  }, [blocks]);
  const laneH = lanes === 1 ? 36 : 22;
  return (
    <div data-row={index} className="flex items-stretch border-b border-line last:border-0">
      <div className="w-52 shrink-0 border-r border-line px-2 py-1.5">
        <div className="mono break-all text-xs font-medium">
          {node.cluster && <span className="mr-1 font-normal text-muted">{node.cluster}</span>}
          {node.name}
        </div>
        <div className="truncate text-[11px] text-muted">{node.gpuTotal > 0 ? `${node.gpuModel ?? "GPU"} ×${node.gpuTotal}` : `${Math.round(node.cpu.allocatable)} 核`}</div>
      </div>
      <div
        data-track
        role="group"
        aria-label={`${node.name} 的预约`}
        className="relative min-w-0 flex-1 cursor-crosshair touch-none select-none"
        style={{ height: 8 + lanes * laneH + (lanes - 1) * 4 }}
      >
        {/* past time is not reservable */}
        <div className="pointer-events-none absolute inset-y-0 left-0 bg-sunk/70" style={{ width: pct(now) }} />
        {Array.from({ length: Math.round((to - from) / DAY) - 1 }, (_, i) => (
          <div key={i} className="pointer-events-none absolute inset-y-0 w-px bg-line" style={{ left: pct(from + (i + 1) * DAY) }} />
        ))}
        {blocks.map((b) => {
          const canOpen = b.mine || admin;
          const cls = b.mine ? "border-accent bg-accent-soft text-accent" : "border-line-strong bg-sunk text-muted";
          const text = `${b.mine ? "我" : b.user} · ${fmtClock(b.start)}–${fmtClock(b.end)}`;
          const style = { left: pct(Date.parse(b.start)), width: `calc(${pct(Date.parse(b.end))} - ${pct(Date.parse(b.start))})`, top: 4 + (lane.get(b.id) ?? 0) * (laneH + 4), height: laneH, lineHeight: `${laneH - 2}px` };
          const title = `${b.user}：${fmtTime(b.start)} – ${fmtTime(b.end)}（${phaseLabel[b.phase]}）`;
          return canOpen ? (
            <button key={b.id} type="button" title={title} onClick={() => onPick(b)} style={style} className={`absolute overflow-hidden rounded border px-1 text-left text-[11px] whitespace-nowrap hover:brightness-95 ${cls}`}>
              {text}
            </button>
          ) : (
            <div key={b.id} title={title} style={style} className={`absolute overflow-hidden rounded border px-1 text-[11px] whitespace-nowrap ${cls}`}>
              {text}
            </div>
          );
        })}
        {now > from && now < to && <div className="pointer-events-none absolute inset-y-0 w-px bg-fail" style={{ left: pct(now) }} aria-hidden />}
        {sel && (
          <div
            className={`pointer-events-none absolute inset-y-0.5 rounded border-2 border-dashed text-[11px] ${bad ? "border-fail bg-fail-soft text-fail" : "border-accent bg-accent-soft/60 text-accent"}`}
            style={{ left: pct(sel[0]), width: `calc(${pct(sel[1])} - ${pct(sel[0])})` }}
          >
            {label && (
              <span className="absolute -top-4 left-0 whitespace-nowrap rounded bg-surface px-1 font-medium shadow-sm">
                {fmtClock(sel[0])}–{fmtClock(sel[1])} · {fmtSpan(sel[1] - sel[0])}
                {bad && " · 不可预约"}
              </span>
            )}
          </div>
        )}
      </div>
    </div>
  );
}

export function ReservationsPage() {
  const me = useMe().data;
  const qc = useQueryClient();
  const tick = useNow(30_000).getTime();
  const [days, setDays] = useState<(typeof RANGES)[number]>(3);
  const rules = useQuery({ queryKey: ["reservation-rules"], queryFn: getReservationRules, staleTime: 300_000, retry: false });
  const nodesQ = useNodeChoices();
  // the server's clock decides what "now" is
  const skew = useMemo(() => (rules.data ? Date.parse(rules.data.now) - Date.now() : 0), [rules.data]);
  const now = tick + skew;
  const from = dayStart(now);
  const to = from + days * DAY;
  const listFrom = from - 30 * DAY;
  const q = useQuery({
    queryKey: ["reservations", new Date(listFrom).toISOString(), 14],
    queryFn: () => listReservations(new Date(listFrom).toISOString(), new Date(from + 15 * DAY).toISOString()),
    refetchInterval: 30_000,
    enabled: !!rules.data,
  });
  // my reservations, of any time: a page at a time, the latest first
  const mineQ = useInfiniteQuery({
    queryKey: ["reservations", "mine"],
    queryFn: ({ pageParam }) => listMyReservations(pageParam || undefined),
    initialPageParam: "",
    getNextPageParam: (last) => last.next || undefined,
    refetchInterval: 30_000,
    enabled: !!rules.data,
  });
  const { matches } = useClusterFilter();
  const [draft, setDraft] = useState<Draft | null>(null);
  const [picked, setPicked] = useState<Reservation | null>(null);
  const [cancel, setCancel] = useState<Reservation | null>(null);
  // a drag on the timeline, from one row's time to another's: the rows
  // between, every one, for the time between (a rectangle)
  const [drag, setDrag] = useState<{ a: number; b: number; r0: number; r1: number } | null>(null);
  const dragTrack = useRef<HTMLElement | null>(null);
  // my reservations: a list like the DevPod list; several cancelled at once
  const [sorting, setSorting] = useState<SortingState>([{ id: "start", desc: true }]);
  const [bulkCancel, setBulkCancel] = useState<{ rows: Reservation[]; clear: () => void } | null>(null);
  const bulk = useBulkRun();
  // the ⋯ menu is in the (memoised) columns: it asks for what is current
  const rowActionsNow = useRef<(b: Reservation) => RowAction[]>(() => []);
  const userNow = useRef("");
  userNow.current = me?.user ?? "";
  const mineCols = useMemo(() => mineColumns(() => userNow.current, (b) => <ActionMenu label={`${b.node} ${fmtTime(b.start)} 操作`} actions={() => rowActionsNow.current(b)} />), []);
  const del = useMutation({
    mutationFn: (r: Reservation) => deleteReservation(r),
    onSettled: () => {
      void qc.invalidateQueries({ queryKey: ["reservations"] });
      void qc.invalidateQueries({ queryKey: ["node-choices"] });
    },
  });

  if (rules.isError) return <EmptyState tone="error" title="节点预约不可用" hint={apiMessage(rules.error)} />;
  if (!rules.data || !me) return <EmptyState title="加载中…" />;
  const g = rules.data.granularitySeconds * 1000;
  const all = (q.data?.items ?? []).filter(matches);
  const live = all.filter((b) => b.phase !== "Cancelled");
  // the machines I need a reservation for (an admin sees every reservable one)
  const nodes = (nodesQ.data?.items ?? []).filter((n) => n.reservable && (n.reservationRequired || me.admin) && matches(n));
  const mine = (mineQ.data?.pages.flatMap((p) => p.items) ?? []).filter((b) => b.mine && matches(b));
  const cancelAll = async (rows: Reservation[], clear: () => void) => {
    setBulkCancel(null);
    await bulk.run("取消预约", rows, (b) => deleteReservation(b).catch((e) => Promise.reject(new Error(apiMessage(e)))), (b) => `${b.node} ${fmtTime(b.start)}`);
    clear();
    void qc.invalidateQueries({ queryKey: ["reservations"] });
    void qc.invalidateQueries({ queryKey: ["node-choices"] });
  };
  const rowActions = (b: Reservation): RowAction[] => [
    ...(isLive(b)
      ? [
          { label: "修改", onSelect: () => open(b) },
          { label: b.phase === "Active" ? "提前释放" : "取消", danger: true, onSelect: () => setCancel(b) },
        ]
      : []),
    { label: "复制节点名", onSelect: () => void copyText(b.node) },
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
        onSelect: () => setBulkCancel({ rows: can, clear }),
      },
      { label: "复制节点名", onSelect: () => void copyText([...new Set(rows.map((b) => b.node))].join("\n")) },
    ];
  };
  const open = (b: Reservation) => setDraft({ node: nodeKey(b), start: Date.parse(b.start), end: Date.parse(b.end) + (b.phase === "Active" ? g : 0), edit: b });
  // from the slot now is in, so the reservation covers now: no gap
  const first = slotStart(now, g);
  const timeAt = (track: HTMLElement, clientX: number) => {
    const r = track.getBoundingClientRect();
    return from + (r.width > 0 ? Math.min(1, Math.max(0, (clientX - r.left) / r.width)) : 0) * (to - from);
  };
  // the row under the pointer, anywhere in it (its name too)
  const rowAt = (x: number, y: number) =>
    typeof document.elementFromPoint === "function" ? ((document.elementFromPoint(x, y) as HTMLElement | null)?.closest<HTMLElement>("[data-row]") ?? null) : null;
  // a drag that starts in the past starts now (the slot now is in)
  const dragSel = drag ? fromNow(selection(drag.a, drag.b, g), first) : null;
  const dragBad = !!dragSel && dragSel[0] < first;
  const rowsOf: [number, number] = drag ? [Math.min(drag.r0, drag.r1), Math.max(drag.r0, drag.r1)] : [-1, -1];

  return (
    <div className="space-y-3 p-3">
      <div className="flex flex-wrap items-center gap-2">
        <div role="radiogroup" aria-label="时间范围" className="inline-flex rounded-md border border-line bg-sunk p-0.5 text-xs">
          {RANGES.map((d) => (
            <button key={d} type="button" role="radio" aria-checked={days === d} onClick={() => setDays(d)} className={`rounded px-2.5 py-0.5 ${days === d ? "bg-surface font-medium shadow-sm" : "text-muted hover:text-ink"}`}>
              {d} 天
            </button>
          ))}
        </div>
        <span className="text-[11px] text-muted">在时间轴上拖动来预约。预约是使用许可，不独占，同一时段可以有多份。北京时间。</span>
        <span className="flex-1" />
        <Button variant="primary" disabled={nodes.length === 0} onClick={() => setDraft({ node: keyOf(nodes[0]), start: first, end: first + Math.max(2 * g, HOUR) })}>
          预约节点
        </Button>
      </div>
      <Panel title="时间轴">
        {nodes.length === 0 ? (
          <EmptyState title="没有需要你预约的机器" hint={me.admin ? "在「节点」页把节点设为可预约；谁需要预约由「访问控制」的规则决定。" : "你能用的机器都不需要预约。"} />
        ) : (
          <div>
            <div className="flex border-b border-line text-[11px] text-muted">
              <div className="w-52 shrink-0 border-r border-line px-2 py-1">节点</div>
              <div className="relative h-6 min-w-0 flex-1">
                {Array.from({ length: days }, (_, i) => (
                  <span key={i} className="absolute top-1 truncate pl-1" style={{ left: `${(i / days) * 100}%`, width: `${100 / days}%` }}>
                    {fmtDay(from + i * DAY)}
                  </span>
                ))}
              </div>
            </div>
            <div
              className={drag ? "select-none" : undefined}
              onPointerDown={(e) => {
                const target = e.target as HTMLElement;
                const track = target.closest<HTMLElement>("[data-track]");
                const row = target.closest<HTMLElement>("[data-row]");
                if (target.closest("button") || !track || !row) return;
                e.preventDefault(); // a drag, not the start of a text selection
                e.currentTarget.setPointerCapture?.(e.pointerId);
                dragTrack.current = track;
                const t = timeAt(track, e.clientX);
                const r = Number(row.dataset.row);
                setDrag({ a: t, b: t, r0: r, r1: r });
              }}
              onPointerMove={(e) => {
                if (!drag || !dragTrack.current) return;
                const over = rowAt(e.clientX, e.clientY);
                setDrag({ ...drag, b: timeAt(dragTrack.current, e.clientX), r1: over ? Number(over.dataset.row) : drag.r1 });
              }}
              onPointerUp={() => {
                if (dragSel && !dragBad) {
                  const picked = nodes.slice(rowsOf[0], rowsOf[1] + 1).map(keyOf);
                  setDraft({ node: picked[0], nodes: picked, start: dragSel[0], end: dragSel[1] });
                }
                setDrag(null);
              }}
              onPointerCancel={() => setDrag(null)}
            >
              {nodes.map((n, i) => (
                <Row
                  key={keyOf(n)}
                  node={n}
                  blocks={live.filter((b) => nodeKey(b) === keyOf(n))}
                  from={from}
                  to={to}
                  now={now}
                  gran={g}
                  admin={me.admin}
                  index={i}
                  sel={dragSel && i >= rowsOf[0] && i <= rowsOf[1] ? dragSel : null}
                  bad={dragBad}
                  label={i === rowsOf[0]}
                  onPick={setPicked}
                />
              ))}
            </div>
            <div className="flex flex-wrap gap-x-4 gap-y-1 px-2 py-1.5 text-[11px] text-muted">
              <span className="inline-flex items-center gap-1">
                <span className="h-2.5 w-4 rounded border border-accent bg-accent-soft" /> 我的预约
              </span>
              <span className="inline-flex items-center gap-1">
                <span className="h-2.5 w-4 rounded border border-line-strong bg-sunk" /> 他人的预约（写有用户名）
              </span>
              <span className="inline-flex items-center gap-1">
                <span className="h-3 w-px bg-fail" /> 现在
              </span>
            </div>
          </div>
        )}
      </Panel>
      <Panel title="我的预约">
        {mine.length === 0 ? (
          <EmptyState title={mineQ.isLoading ? "加载中…" : mineQ.isError ? `加载失败：${apiMessage(mineQ.error)}` : "你还没有预约"} />
        ) : (
          <DataTable<Reservation>
            label="我的预约"
            storageKey="my-reservations"
            data={mine}
            columns={mineCols}
            getRowId={resKey}
            sorting={sorting}
            onSortingChange={setSorting}
            selectable
            onRowClick={(b) => isLive(b) && open(b)}
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
        <LoadMore hasMore={mineQ.hasNextPage} busy={mineQ.isFetchingNextPage} onClick={() => void mineQ.fetchNextPage()} auto />
      </Panel>
      <ConfirmDialog
        open={!!bulkCancel}
        onOpenChange={(o) => !o && setBulkCancel(null)}
        title={`取消 ${bulkCancel?.rows.length ?? 0} 份预约？`}
        confirmLabel={`取消 ${bulkCancel?.rows.length ?? 0} 份预约`}
        danger
        onConfirm={() => bulkCancel && void cancelAll(bulkCancel.rows, bulkCancel.clear)}
      >
        {bulkCancel &&
          (bulkCancel.rows.some((b) => b.phase === "Active")
            ? `其中 ${bulkCancel.rows.filter((b) => b.phase === "Active").length} 份进行中的会提前释放：你在那些机器上只靠预约运行的 DevPod 会被自动休眠。其余的直接取消。`
            : "它们都还没开始，直接取消。")}
      </ConfirmDialog>
      {bulk.toast}
      {draft && <ReserveDialog key={`${draft.node}-${draft.start}-${draft.edit?.id ?? ""}`} draft={draft} nodes={nodes} rules={rules.data} now={now} onClose={() => setDraft(null)} />}
      <ConfirmDialog
        open={!!picked}
        onOpenChange={(o) => !o && setPicked(null)}
        title={picked ? `${picked.node} · ${picked.mine ? "我的预约" : `${picked.user} 的预约`}` : ""}
        confirmLabel={picked?.mine ? "修改…" : "取消这个预约"}
        danger={!picked?.mine}
        onConfirm={() => {
          const b = picked!;
          setPicked(null);
          if (b.mine) open(b);
          else setCancel(b);
        }}
      >
        {picked && (
          <span>
            {fmtTime(picked.start)} – {fmtTime(picked.end)}，{phaseLabel[picked.phase]}。
            {picked.mine && (
              <button type="button" className="ml-2 text-fail hover:underline" onClick={() => (setCancel(picked), setPicked(null))}>
                {picked.phase === "Active" ? "提前释放" : "取消预约"}
              </button>
            )}
          </span>
        )}
      </ConfirmDialog>
      <ConfirmDialog
        open={!!cancel}
        onOpenChange={(o) => !o && setCancel(null)}
        title={cancel?.phase === "Active" ? "提前释放这台机器？" : "取消这个预约？"}
        confirmLabel={cancel?.phase === "Active" ? "提前释放" : "取消预约"}
        danger
        onConfirm={() => {
          if (cancel) del.mutate(cancel);
          setCancel(null);
        }}
      >
        {cancel && (
          <>
            {cancel.node}，{fmtTime(cancel.start)} – {fmtTime(cancel.end)}
            {!cancel.mine && `，预约人 ${cancel.user}`}。
            {cancel.phase === "Active" && "释放后，预约人在这台机器上只靠这份预约运行的 DevPod 会被自动休眠。"}
          </>
        )}
      </ConfirmDialog>
      {del.isError && <Notice>{apiMessage(del.error)}</Notice>}
    </div>
  );
}
