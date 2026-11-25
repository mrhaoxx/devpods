// 节点预约 (UI spec §5.7; access control design §12): a timeline of the
// reservable machines the access rules ask me a reservation for — drag to
// reserve, click a block of your own to change or release it — plus the
// list of my reservations and a plain form (the keyboard path). A
// reservation is a permit, not a hold: they may overlap, drawn in lanes.
import { keyOf, refOfKey } from "../../data/cluster";
import { useClusterFilter } from "../../data/clusterFilter";
import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
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
import { phaseLabel, ReservationDetails } from "./ReservationDetails";
import { EmptyState } from "../../design/EmptyState";
import { Field, Input, Notice, Select } from "../../design/Form";
import { Panel } from "../../design/Panel";
import { useNow } from "../../lib/useNow";
import { LoadMore } from "../audit/common";
import {
  type Amount,
  amountOf,
  bookedAt,
  capOf,
  cpuShare,
  freeIn,
  presets,
  dayStart,
  fmtAmount,
  fmtClock,
  fmtDay,
  fmtSpan,
  fmtTime,
  fromInput,
  fromNow,
  periodError,
  selection,
  slotStart,
  stackBand,
  toInput,
} from "../../lib/reservations";
import * as Dialog from "@radix-ui/react-dialog";

const DAY = 86_400_000;
const RANGES = [3, 7, 14] as const;

/** apiMessage turns a reservation API failure into a sentence. */
function apiMessage(e: unknown): string {
  if (e instanceof ApiFailure) {
    if (e.code === "NOT_RESERVABLE") return "这台机器没有开放预约。";
    if (e.code === "ACL_DENIED") return "你没有使用这台机器的权限。";
    if (e.code === "NO_ROOM") {
      const d = e.body.detail as { free?: Amount; earliest?: string } | undefined;
      if (d?.free) {
        const what = d.free.gpu > 0 ? `${d.free.gpu} 卡 · ${num(d.free.cpu)} 核` : d.free.cpu > 0 ? `${num(d.free.cpu)} 核` : "";
        return `${what ? `这段时间最多还能约 ${what}` : "这段时间已约满"}${d.earliest ? `；最早 ${fmtTime(d.earliest)} 起有空` : ""}`;
      }
    }
    return e.body.message;
  }
  return String(e);
}

/** num: a count of cores as said — at most one decimal. */
const num = (n: number) => String(Math.round(n * 10) / 10);

/** Ask: what of a machine a reservation asks for — cards and cores; neither: all of it. */
type Ask = { gpu?: number; cpu?: number };
const wholeAsk = (a: Ask) => a.gpu == null && a.cpu == null;

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
    { id: "amount", header: "资源", accessorFn: (b) => fmtAmount(b), meta: { priority: 2, width: 120 } },
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
    {
      id: "purpose",
      header: "用途",
      accessorFn: (b) => b.purpose ?? "",
      meta: { priority: 2, width: 200 },
      cell: ({ getValue }) => <span title={getValue() as string}>{getValue() as string}</span>,
    },
    { id: "actions", header: "", enableSorting: false, meta: { priority: 1, width: 48, align: "right", pin: "right" }, cell: ({ row }) => actions(row.original) },
  ];
}

/** The form: new reservation, move (not started) or move the end of one that has started. */
function ReserveDialog({
  draft,
  nodes,
  blocks,
  rules,
  now,
  forOthers,
  onClose,
}: {
  draft: Draft;
  nodes: NodeChoice[];
  /** the reservations on the timeline: what the form says is free from */
  blocks: Reservation[];
  rules: ReservationRules;
  now: number;
  forOthers: boolean;
  onClose: () => void;
}) {
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
  const [purpose, setPurpose] = useState(edit?.purpose ?? "");
  const [forUser, setForUser] = useState("");
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
  // how much of the machine: the whole of it when that time is free, else
  // the most offered that fits (an edit: what it holds)
  const blocksOf = (k: string) => blocks.filter((b) => nodeKey(b) === k);
  const nodeOf = (k: string) => nodes.find((n) => keyOf(n) === k);
  const first = nodeOf(edit ? node : (picked[0] ?? draft.node)) ?? nodeOf(draft.node);
  const cap: Amount = first ? capOf(first) : { gpu: 0, cpu: 0 };
  const offered = presets(cap);
  const [ask, setAsk] = useState<Ask>(() => {
    if (edit) return { gpu: edit.gpu, cpu: edit.cpu };
    const k = draft.nodes?.[0] ?? draft.node;
    const n = nodeOf(k);
    if (!n) return {};
    const c = capOf(n);
    const free = freeIn(blocksOf(k), Math.max(draft.start, now), draft.end, c);
    const p = presets(c).find((p) => (wholeAsk(p) ? free.gpu === c.gpu && free.cpu === c.cpu : (p.gpu ?? 0) <= free.gpu && (p.cpu ?? 0) <= free.cpu));
    return p ? { gpu: p.gpu, cpu: p.cpu } : {};
  });
  // an edit whose cores are not its cards' share: they were set by hand
  const [ownCpu, setOwnCpu] = useState(() => !!edit && cap.gpu > 0 && edit.gpu != null && edit.cpu != null && edit.cpu !== cpuShare(edit.gpu, cap));
  // what is typed as another amount (empty while a preset is chosen)
  const [custom, setCustom] = useState(() =>
    edit && !wholeAsk(ask) && !presets(cap).some((p) => (cap.gpu > 0 ? p.gpu === edit.gpu : p.cpu === edit.cpu)) ? String((cap.gpu > 0 ? edit.gpu : edit.cpu) ?? "") : "",
  );
  const pick = (a: Ask) => (setAsk(a), touch());
  // the times to check, as the console does: from now; a change only for
  // the time it adds — all it has left when it asks for more (one made
  // before shares may overlap another and still be shortened or renamed)
  const spansOf = (c: Amount): [number, number][] => {
    const from = Math.max(s, now);
    if (!edit) return [[from, e]];
    const was = wholeAsk(edit) ? c : { gpu: edit.gpu ?? 0, cpu: edit.cpu ?? 0 };
    const want = wholeAsk(ask) ? c : { gpu: ask.gpu ?? 0, cpu: ask.cpu ?? 0 };
    if (want.gpu > was.gpu || want.cpu > was.cpu || (wholeAsk(ask) && !wholeAsk(edit))) return [[from, e]];
    const [os, oe] = [Date.parse(edit.start), Date.parse(edit.end)];
    const out: [number, number][] = [];
    if (from < os) out.push([from, Math.min(os, e)]);
    if (e > oe) out.push([Math.max(oe, from), e]);
    return out.filter(([a, b]) => a < b);
  };
  // what is free on each machine in that time, beside the others (not the one being changed)
  const rooms = Number.isNaN(e) || problem
    ? []
    : (edit ? [node] : picked).flatMap((k) => {
        const n = nodeOf(k);
        if (!n) return [];
        const c = capOf(n);
        const spans = spansOf(c);
        if (spans.length === 0) return []; // nothing more asked
        const frees = spans.map(([a, b]) => freeIn(blocksOf(k), a, b, c, edit?.id));
        const free = { gpu: Math.min(...frees.map((f) => f.gpu)), cpu: Math.min(...frees.map((f) => f.cpu)) };
        const ok = frees.every((f) => (wholeAsk(ask) ? f.gpu === c.gpu && f.cpu === c.cpu : (ask.gpu ?? 0) <= f.gpu && (ask.cpu ?? 0) <= f.cpu));
        const what = c.gpu > 0 ? `${free.gpu} 卡 · ${num(free.cpu)} 核` : `${num(free.cpu)} 核`;
        return [{ k, ok, what, full: free.gpu <= 0 && free.cpu <= 0 }];
      });
  const short = rooms.some((r) => !r.ok);
  const noticeMs = rules.clearNoticeSeconds * 1000;
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
        // only what was changed
        const body: { start?: string; end?: string; purpose?: string; gpu?: number; cpu?: number; whole?: boolean } = {};
        if (!active && s !== Date.parse(edit.start)) body.start = period.start;
        if (e !== Date.parse(edit.end)) body.end = period.end;
        if (purpose.trim() !== (edit.purpose ?? "")) body.purpose = purpose.trim();
        if ((ask.gpu ?? null) !== (edit.gpu ?? null) || (ask.cpu ?? null) !== (edit.cpu ?? null)) {
          if (wholeAsk(ask)) body.whole = true;
          else Object.assign(body, { gpu: ask.gpu ?? 0, ...(ask.cpu != null ? { cpu: ask.cpu } : {}) });
        }
        if (Object.keys(body).length > 0) await patchReservation(edit, body);
        return { made: 1, failed: [] };
      }
      const keys = picked;
      const extra = {
        ...(purpose.trim() ? { purpose: purpose.trim() } : {}),
        ...(forOthers && forUser.trim() ? { user: forUser.trim() } : {}),
        ...(ask.gpu != null ? { gpu: ask.gpu } : {}),
        ...(ask.cpu != null ? { cpu: ask.cpu } : {}),
      };
      const results = await Promise.allSettled(keys.map((k) => createReservation(refOfKey(k).cluster, { ...period, ...extra, node: refOfKey(k).name })));
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
        <Dialog.Content aria-describedby={undefined} className="fixed left-1/2 top-4 z-50 max-h-[calc(100dvh-2rem)] w-[min(92vw,440px)] -translate-x-1/2 space-y-3 overflow-y-auto rounded-lg border border-line bg-surface p-4 shadow-xl sm:top-[10vh] sm:max-h-[80vh]">
          <Dialog.Title className="text-sm font-semibold">{edit ? "修改预约" : "预约节点"}</Dialog.Title>
          <form
            className="space-y-3"
            onSubmit={(ev) => {
              ev.preventDefault();
              if (problem || short) return;
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
            <div className="space-y-1">
              <div className="text-xs font-medium">要多少</div>
              <div role="radiogroup" aria-label="要多少" className="flex flex-wrap items-center gap-1.5 text-xs">
                {offered.map((p) => {
                  const on = (p.gpu ?? null) === (ask.gpu ?? null) && (p.gpu != null || (p.cpu ?? null) === (ask.cpu ?? null));
                  return (
                    <button
                      key={p.label}
                      type="button"
                      role="radio"
                      aria-checked={on}
                      onClick={() => (setOwnCpu(false), setCustom(""), pick({ gpu: p.gpu, cpu: p.cpu }))}
                      className={`rounded border px-2 py-0.5 ${on ? "border-accent bg-accent-soft text-accent" : "border-line hover:bg-sunk"}`}
                    >
                      {p.label}
                    </button>
                  );
                })}
                <Input
                  aria-label={cap.gpu > 0 ? "自定义卡数" : "自定义核数"}
                  type="number"
                  min={1}
                  max={cap.gpu > 0 ? cap.gpu : cap.cpu || undefined}
                  placeholder={cap.gpu > 0 ? "其他卡数" : "其他核数"}
                  className="!w-24"
                  value={custom}
                  onChange={(ev) => {
                    setCustom(ev.target.value);
                    const n = Number(ev.target.value);
                    if (!Number.isInteger(n) || n < 1) return;
                    pick(cap.gpu > 0 ? { gpu: n, cpu: ownCpu && ask.cpu != null ? ask.cpu : cpuShare(n, cap) } : { cpu: n });
                  }}
                />
                {cap.gpu > 0 && !wholeAsk(ask) && (
                  <>
                    {ownCpu ? (
                      <Input
                        aria-label="CPU 核数"
                        type="number"
                        min={1}
                        className="!w-20"
                        value={String(ask.cpu ?? "")}
                        onChange={(ev) => {
                          const n = Number(ev.target.value);
                          if (n > 0) pick({ gpu: ask.gpu, cpu: n });
                        }}
                      />
                    ) : (
                      <span className="text-muted">→ {ask.cpu} 核</span>
                    )}
                    <button type="button" className="text-accent hover:underline" onClick={() => setOwnCpu((o) => !o)}>
                      {ownCpu ? "按卡数配 CPU" : "调整 CPU"}
                    </button>
                  </>
                )}
              </div>
              {rooms.map((r) => (
                <p key={r.k} className={`text-[11px] ${r.ok ? "text-run" : "text-fail"}`}>
                  {r.ok ? `✓ 这段时间 ${nameOf(r.k)} 还能约 ${r.what}` : r.full ? `✗ ${nameOf(r.k)} 这段时间已约满` : `✗ ${nameOf(r.k)} 这段时间最多还能约 ${r.what}`}
                </p>
              ))}
            </div>
            <Field label="用途（选填）">
              <Input aria-label="用途" value={purpose} maxLength={200} placeholder="例如：跑 sglang 基准" onChange={(ev) => (setPurpose(ev.target.value), touch())} />
            </Field>
            {forOthers && !edit && (
              <Field label="为谁预约" hint="留空为自己；对方要能使用这台机器">
                <Input aria-label="为谁预约" className="mono" value={forUser} placeholder="用户名" onChange={(ev) => (setForUser(ev.target.value), touch())} />
              </Field>
            )}
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
            {!edit && !problem && noticeMs > 0 && s < now + noticeMs && (
              <p className="text-[11px] text-warm">如果机器上有人在用没被预约的部分，他们会先收到通知，最晚 {fmtClock(now + noticeMs)} 前腾出。</p>
            )}
            {!edit && (
              <p className="text-[11px] text-muted">
                约到的卡和核在预约期间只归你，机器上没被约走的部分别人照样可以用。开始时如果有人占着你约的部分，他们会先收到通知、到时被休眠。到期后，你在这台机器上只靠这份预约运行的 DevPod 会被自动休眠。
              </p>
            )}
            <div className="flex justify-end gap-2">
              <Dialog.Close asChild>
                <Button type="button">取消</Button>
              </Dialog.Close>
              <Button type="submit" variant="primary" disabled={!!problem || short || save.isPending || (!edit && picked.length === 0)}>
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
  index,
  sel,
  bad,
  label,
  trackW,
  onPick,
}: {
  node: NodeChoice;
  blocks: Reservation[];
  from: number;
  to: number;
  now: number;
  gran: number;
  /** this row's place: what a drag across rows reads */
  index: number;
  /** the time a drag picks, when the drag takes this row in */
  sel: [number, number] | null;
  bad: boolean;
  /** the drag's time is said on the top row it takes in only */
  label: boolean;
  /** the track's width in px (the whole window at the zoom); 0: not measured, fill the row */
  trackW: number;
  onPick: (b: Reservation) => void;
}) {
  void gran;
  const pct = (t: number) => `${Math.min(100, Math.max(0, ((t - from) / (to - from)) * 100))}%`;
  const cap = capOf(node);
  // each machine is a GPU band (when it has cards) and a CPU band: a
  // reservation is in each at its share of it, stacked from the bottom
  const bands = useMemo(() => {
    const out: { key: "gpu" | "cpu"; label: string; h: number; cap: number; pick: (b: Reservation) => number }[] = [];
    if (cap.gpu > 0) out.push({ key: "gpu", label: "GPU", h: Math.min(64, Math.max(24, 6 * cap.gpu)), cap: cap.gpu, pick: (b) => amountOf(b, cap).gpu });
    out.push({ key: "cpu", label: "CPU", h: out.length ? 24 : 36, cap: cap.cpu, pick: (b) => amountOf(b, cap).cpu });
    return out.map((band) => ({ ...band, stack: band.cap > 0 ? stackBand(blocks, band.cap, band.pick) : null }));
  }, [blocks, cap.gpu, cap.cpu]); // eslint-disable-line react-hooks/exhaustive-deps
  // the band a reservation is said (and picked) in: the first it shows in
  const primary = (b: Reservation) => bands.find((band) => band.cap <= 0 || band.pick(b) > 0)?.key ?? "cpu";
  const [hot, setHot] = useState<string | null>(null);
  const [hoverT, setHoverT] = useState<number | null>(null);
  const booked = bookedAt(blocks, hoverT ?? now, cap);
  return (
    <div data-row={index} className="flex items-stretch border-b border-line last:border-0">
      <div className="sticky left-0 z-10 w-28 shrink-0 border-r border-line bg-surface px-2 py-1.5 sm:w-52" title={node.name}>
        <div className="mono break-all text-xs font-medium">
          {node.cluster && <span className="mr-1 font-normal text-muted">{node.cluster}</span>}
          {node.name}
        </div>
        <div className="truncate text-[11px] text-muted">{node.gpuTotal > 0 ? `${node.gpuModel ?? "GPU"} ×${node.gpuTotal}` : `${Math.round(node.cpu.allocatable)} 核`}</div>
        {/* what is reserved at now, or at the time under the pointer */}
        <div className="mono text-[10px] leading-tight text-faint">
          {bands.map((band) => (
            <div key={band.key}>{`${band.label} ${num(band.key === "gpu" ? booked.gpu : booked.cpu)}/${num(band.cap)}`}</div>
          ))}
        </div>
      </div>
      <div
        data-track
        role="group"
        aria-label={`${node.name} 的预约`}
        // touch: the browser pans (sideways through the days, down the page); a tap reserves
        className={`relative cursor-crosshair touch-pan-x touch-pan-y select-none ${trackW > 0 ? "shrink-0" : "min-w-0 flex-1"}`}
        style={{ height: 8 + bands.reduce((h, band) => h + band.h, 0) + (bands.length - 1) * 2, ...(trackW > 0 ? { width: trackW } : {}) }}
        onMouseMove={(e) => {
          const r = e.currentTarget.getBoundingClientRect();
          if (r.width > 0) setHoverT(from + ((e.clientX - r.left) / r.width) * (to - from));
        }}
        onMouseLeave={() => setHoverT(null)}
      >
        {/* past time is not reservable */}
        <div className="pointer-events-none absolute inset-y-0 left-0 bg-sunk/70" style={{ width: pct(now) }} />
        {Array.from({ length: Math.round((to - from) / DAY) - 1 }, (_, i) => (
          <div key={i} className="pointer-events-none absolute inset-y-0 w-px bg-line" style={{ left: pct(from + (i + 1) * DAY) }} />
        ))}
        {bands.map((band, bi) => {
          const top = 4 + bands.slice(0, bi).reduce((h, x) => h + x.h + 2, 0);
          return (
            <div key={band.key} data-band={band.key} className="pointer-events-none absolute inset-x-0 border-y border-line/40" style={{ top, height: band.h }}>
              {blocks.map((b) => {
                const units = band.pick(b);
                const at = band.stack?.get(b.id);
                // a machine whose capacity is not known: every one fills the band
                const [y0, y1, over] = band.cap > 0 && at ? [at.y0 / band.cap, at.y1 / band.cap, at.over] : [0, 1, false];
                if (band.cap > 0 && !(units > 0)) return null;
                const h = Math.max(2, (y1 - y0) * band.h);
                const bad = over || b.overbooked;
                const cls = `${b.mine ? "border-accent bg-accent-soft text-accent" : "border-line-strong bg-sunk text-muted"} ${bad ? "border-fail" : ""} ${hot === b.id ? "ring-2 ring-accent" : ""}`;
                const style = {
                  left: pct(Date.parse(b.start)),
                  width: `calc(${pct(Date.parse(b.end))} - ${pct(Date.parse(b.start))})`,
                  minWidth: 16,
                  bottom: y0 * band.h,
                  height: h,
                };
                const title = `${b.user}：${fmtTime(b.start)} – ${fmtTime(b.end)}（${phaseLabel[b.phase]}）· ${fmtAmount(b)}${bad ? " · 超出机器容量" : ""}${b.purpose ? `\n${b.purpose}` : ""}`;
                const hover = { onMouseEnter: () => setHot(b.id), onMouseLeave: () => setHot(null) };
                const common = { "data-res": b.id, "data-hot": hot === b.id ? "true" : undefined, title, style, ...hover };
                if (primary(b) !== band.key)
                  return <div key={b.id} aria-hidden {...common} onClick={() => onPick(b)} className={`pointer-events-auto absolute cursor-pointer overflow-hidden rounded border ${cls}`} />;
                const text = `${b.mine ? "我" : b.user} · ${fmtClock(b.start)}–${fmtClock(b.end)}${b.purpose ? ` · ${b.purpose}` : ""}`;
                return (
                  <button
                    key={b.id}
                    type="button"
                    aria-label={text}
                    {...common}
                    onClick={() => onPick(b)}
                    className={`pointer-events-auto absolute overflow-hidden rounded border px-1 text-left text-[11px] leading-[14px] whitespace-nowrap hover:brightness-95 ${cls}`}
                  >
                    {h >= 14 ? text : null}
                  </button>
                );
              })}
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
  // the timeline holds the whole window one may reserve in (00:00 today to
  // the furthest bookable moment); the zoom says how many days fit a screen
  const ahead = rules.data && rules.data.maxAdvanceSeconds > 0 ? rules.data.maxAdvanceSeconds * 1000 : 14 * DAY;
  const spanDays = Math.max(days, Math.ceil((now + ahead - from) / DAY));
  const to = from + spanDays * DAY;
  // the width the track has on screen: the scroller's, less the name column
  const scroller = useRef<HTMLDivElement | null>(null);
  const nameCol = useRef<HTMLDivElement | null>(null);
  const [vis, setVis] = useState(0);
  const measure = useCallback(() => {
    const el = scroller.current;
    if (!el) return;
    const w = el.clientWidth - (nameCol.current?.offsetWidth ?? 0);
    setVis((v) => (Math.abs(v - w) > 0.5 ? w : v));
  }, []);
  const scrollerRef = useCallback(
    (el: HTMLDivElement | null) => {
      scroller.current = el;
      if (el) measure();
    },
    [measure],
  );
  useEffect(() => {
    window.addEventListener("resize", measure);
    const el = scroller.current;
    const ro = el && typeof ResizeObserver !== "undefined" ? new ResizeObserver(() => measure()) : null;
    if (el) ro?.observe(el);
    return () => {
      window.removeEventListener("resize", measure);
      ro?.disconnect();
    };
    // the scroller comes with the machines
  }, [measure, nodesQ.data]);
  const trackW = vis > 0 ? Math.round((vis * spanDays) / days) : 0;
  // opened, or zoomed: now a little in from the left
  const placed = useRef("");
  useLayoutEffect(() => {
    const el = scroller.current;
    const key = `${days}:${from}:${trackW > 0}`;
    if (!el || trackW <= 0 || placed.current === key) return;
    placed.current = key;
    el.scrollLeft = Math.max(0, ((now - from) / (to - from)) * trackW - vis * 0.1);
  });
  // a touch that may be a tap (no move): where it went down
  const tap = useRef<{ x: number; y: number; track: HTMLElement; row: number } | null>(null);
  const listFrom = from - 30 * DAY;
  const q = useQuery({
    // the whole window the timeline shows
    queryKey: ["reservations", new Date(listFrom).toISOString(), new Date(to).toISOString()],
    queryFn: () => listReservations(new Date(listFrom).toISOString(), new Date(to).toISOString()),
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
  // the reservable machines I may use, whether I need a reservation there or not
  const nodes = (nodesQ.data?.items ?? []).filter((n) => n.reservable && (n.access !== "denied" || n.manageReservations) && matches(n));
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
  // opened as it is: a change goes in only where made (a purpose alone moves no end)
  const open = (b: Reservation) => setDraft({ node: nodeKey(b), start: Date.parse(b.start), end: Date.parse(b.end), edit: b });
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
        <span className="text-[11px] text-muted">在时间轴上拖动（触屏或手写笔点一下）来预约，左右滑动看更多天。每台机器上面一条是 GPU、下面一条是 CPU：约到的部分在预约期间只归你，可以只约几张卡。北京时间。</span>
        <span className="flex-1" />
        <Button variant="primary" disabled={nodes.length === 0} onClick={() => setDraft({ node: keyOf(nodes[0]), start: first, end: first + Math.max(2 * g, HOUR) })}>
          预约节点
        </Button>
      </div>
      <Panel title="时间轴">
        {nodes.length === 0 ? (
          <EmptyState title="没有你能预约的机器" hint={me.admin ? "在「节点」页把节点设为可预约；谁需要预约由「访问控制」的规则决定。" : "还没有向你开放预约的机器。"} />
        ) : (
          <div>
          <div ref={scrollerRef} data-timeline className="relative overflow-x-auto">
            <div className="flex w-max min-w-full border-b border-line text-[11px] text-muted">
              <div ref={nameCol} className="sticky left-0 z-10 w-28 shrink-0 border-r border-line bg-surface px-2 py-1 sm:w-52">
                节点
              </div>
              <div className={`relative h-6 ${trackW > 0 ? "shrink-0" : "min-w-0 flex-1"}`} style={trackW > 0 ? { width: trackW } : undefined}>
                {Array.from({ length: spanDays }, (_, i) => (
                  <span key={i} className="absolute top-1 truncate pl-1" style={{ left: `${(i / spanDays) * 100}%`, width: `${100 / spanDays}%` }}>
                    {fmtDay(from + i * DAY)}
                  </span>
                ))}
              </div>
            </div>
            <div
              className={`w-max min-w-full ${drag ? "select-none" : ""}`}
              onPointerDown={(e) => {
                const target = e.target as HTMLElement;
                const track = target.closest<HTMLElement>("[data-track]");
                const row = target.closest<HTMLElement>("[data-row]");
                if (target.closest("button, [data-res]") || !track || !row) return;
                if (e.pointerType === "touch" || e.pointerType === "pen") {
                  // the browser pans on a swipe (finger or pen); a tap (no move) reserves there
                  tap.current = { x: e.clientX, y: e.clientY, track, row: Number(row.dataset.row) };
                  return;
                }
                e.preventDefault(); // a drag, not the start of a text selection
                e.currentTarget.setPointerCapture?.(e.pointerId);
                dragTrack.current = track;
                const t = timeAt(track, e.clientX);
                const r = Number(row.dataset.row);
                setDrag({ a: t, b: t, r0: r, r1: r });
              }}
              onPointerMove={(e) => {
                if (tap.current && Math.hypot(e.clientX - tap.current.x, e.clientY - tap.current.y) > 8) tap.current = null; // a swipe
                if (!drag || !dragTrack.current) return;
                const over = rowAt(e.clientX, e.clientY);
                setDrag({ ...drag, b: timeAt(dragTrack.current, e.clientX), r1: over ? Number(over.dataset.row) : drag.r1 });
              }}
              onPointerUp={(e) => {
                if (e.pointerType === "touch" || e.pointerType === "pen") {
                  const t = tap.current;
                  tap.current = null;
                  if (t && Math.hypot(e.clientX - t.x, e.clientY - t.y) <= 8 && nodes[t.row]) {
                    const at = Math.max(first, slotStart(timeAt(t.track, t.x), g));
                    const k = keyOf(nodes[t.row]);
                    setDraft({ node: k, nodes: [k], start: at, end: at + Math.max(2 * g, HOUR) });
                  }
                  return;
                }
                if (dragSel && !dragBad) {
                  const picked = nodes.slice(rowsOf[0], rowsOf[1] + 1).map(keyOf);
                  setDraft({ node: picked[0], nodes: picked, start: dragSel[0], end: dragSel[1] });
                }
                setDrag(null);
              }}
              onPointerCancel={() => {
                tap.current = null;
                setDrag(null);
              }}
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
                  index={i}
                  sel={dragSel && i >= rowsOf[0] && i <= rowsOf[1] ? dragSel : null}
                  bad={dragBad}
                  label={i === rowsOf[0]}
                  trackW={trackW}
                  onPick={setPicked}
                />
              ))}
            </div>
          </div>
            <div className="flex flex-wrap gap-x-4 gap-y-1 border-t border-line px-2 py-1.5 text-[11px] text-muted">
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
      {draft && <ReserveDialog key={`${draft.node}-${draft.start}-${draft.edit?.id ?? ""}`} draft={draft} nodes={nodes} blocks={live} rules={rules.data} now={now} forOthers={me.admin || !!me.permissions?.manageReservations} onClose={() => setDraft(null)} />}
      {picked && (
        <ReservationDetails
          b={picked}
          canManage={picked.canManage ?? (picked.mine || me.admin)}
          onClose={() => setPicked(null)}
          onEdit={() => {
            const b = picked;
            setPicked(null);
            open(b);
          }}
          onCancel={() => {
            const b = picked;
            setPicked(null);
            setCancel(b);
          }}
        />
      )}
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
