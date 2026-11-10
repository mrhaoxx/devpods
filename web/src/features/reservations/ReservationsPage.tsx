// 节点预约 (UI spec §5.7): a timeline of the reservable nodes — drag on
// free time to reserve, click a block of your own to extend, move or
// release it — plus the list of my reservations and a plain form (the
// keyboard path; same checks as the drag).
import { useMemo, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ApiFailure,
  createReservation,
  deleteReservation,
  getReservationRules,
  listReservations,
  patchReservation,
  previewReservation,
  type NodeChoice,
  type Reservation,
  type ReservationRules,
} from "../../data/api";
import { useMe, useNodeChoices } from "../../data/queries";
import { Badge } from "../../design/Badge";
import { Button } from "../../design/Button";
import { ConfirmDialog } from "../../design/ConfirmDialog";
import { EmptyState } from "../../design/EmptyState";
import { Field, Input, Notice, Select } from "../../design/Form";
import { Panel } from "../../design/Panel";
import { useNow } from "../../lib/useNow";
import { dayStart, fmtClock, fmtDay, fmtSpan, fmtTime, fromInput, overlapping, periodError, selection, slotStart, toInput } from "../../lib/reservations";
import * as Dialog from "@radix-ui/react-dialog";

const DAY = 86_400_000;
const RANGES = [3, 7, 14] as const;
const phaseLabel: Record<Reservation["phase"], string> = { Upcoming: "即将开始", Active: "进行中", Ended: "已结束", Cancelled: "已取消" };

/** apiMessage turns a reservation API failure into a sentence. */
function apiMessage(e: unknown): string {
  if (e instanceof ApiFailure) {
    const d = e.body.detail as { user?: string; start?: string; end?: string } | undefined;
    if (e.code === "RESERVATION_CONFLICT" && d?.start && d.end) return `这段时间和 ${d.user} 的预约（${fmtTime(d.start)} – ${fmtTime(d.end)}）冲突。`;
    if (e.code === "NOT_RESERVABLE") return "这台节点没有开放预约。";
    return e.body.message;
  }
  return String(e);
}

type Draft = { node: string; start: number; end: number; edit?: Reservation };

/** The form: new reservation, move (not started) or extend (started). */
function ReserveDialog({ draft, nodes, rules, blocks, now, onClose }: { draft: Draft; nodes: NodeChoice[]; rules: ReservationRules; blocks: Reservation[]; now: number; onClose: () => void }) {
  const qc = useQueryClient();
  const edit = draft.edit;
  const active = edit?.phase === "Active";
  const [node, setNode] = useState(draft.node);
  const [start, setStart] = useState(toInput(draft.start));
  const [end, setEnd] = useState(toInput(draft.end));
  const [preview, setPreview] = useState<number | null>(null);
  const [error, setError] = useState<string | null>(null);
  const s = fromInput(start);
  const e = fromInput(end);
  const g = rules.granularitySeconds * 1000;
  const ruleErr = active
    ? e <= Date.parse(edit!.end)
      ? "延长：新的结束时间要晚于现在的结束时间"
      : slotStart(e, g) !== e
        ? `结束时间要对齐到 ${fmtSpan(g)} 一档`
        : null
    : periodError(s, e, rules, now);
  const clash = !ruleErr ? overlapping(blocks, node, active ? Date.parse(edit!.end) : s, e, edit?.id) : undefined;
  const problem = ruleErr ?? (clash ? `和 ${clash.user} 的预约（${fmtTime(clash.start)} – ${fmtTime(clash.end)}）冲突` : null);
  const period = { node, start: new Date(s).toISOString(), end: new Date(e).toISOString() };
  const done = () => {
    void qc.invalidateQueries({ queryKey: ["reservations"] });
    void qc.invalidateQueries({ queryKey: ["node-choices"] });
    onClose();
  };
  const check = useMutation({ mutationFn: () => previewReservation(period), onSuccess: (r) => setPreview(r.willHibernate), onError: (err) => setError(apiMessage(err)) });
  const save = useMutation({
    mutationFn: () => (edit ? patchReservation(edit.id, active ? { end: period.end } : { start: period.start, end: period.end }) : createReservation(period)),
    onSuccess: done,
    onError: (err) => setError(apiMessage(err)),
  });
  const touch = () => {
    setPreview(null);
    setError(null);
  };
  const step = rules.granularitySeconds;
  return (
    <Dialog.Root open onOpenChange={(o) => !o && onClose()}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/30" />
        <Dialog.Content aria-describedby={undefined} className="fixed left-1/2 top-1/4 z-50 w-[min(92vw,440px)] -translate-x-1/2 space-y-3 rounded-lg border border-line bg-surface p-4 shadow-xl">
          <Dialog.Title className="text-sm font-semibold">{edit ? (active ? "延长预约" : "修改预约") : "预约节点"}</Dialog.Title>
          <form
            className="space-y-3"
            onSubmit={(ev) => {
              ev.preventDefault();
              if (problem) return;
              if (edit || preview !== null) save.mutate();
              else check.mutate();
            }}
          >
            <Field label="节点">
              <Select aria-label="节点" value={node} disabled={!!edit} onChange={(ev) => (setNode(ev.target.value), touch())}>
                {nodes.map((n) => (
                  <option key={n.name} value={n.name}>
                    {n.name}
                    {n.gpuTotal > 0 ? ` · ${n.gpuModel ?? "GPU"} ×${n.gpuTotal}` : ""}
                  </option>
                ))}
              </Select>
            </Field>
            <div className="grid gap-3 sm:grid-cols-2">
              <Field label="开始">
                <Input aria-label="开始" type="datetime-local" step={step} value={start} disabled={active} onChange={(ev) => (setStart(ev.target.value), touch())} />
              </Field>
              <Field label="结束">
                <Input aria-label="结束" type="datetime-local" step={step} value={end} onChange={(ev) => (setEnd(ev.target.value), touch())} />
              </Field>
            </div>
            <p className="text-[11px] text-muted">
              北京时间，{fmtSpan(g)} 一档
              {rules.maxDurationSeconds > 0 && `，单次最长 ${fmtSpan(rules.maxDurationSeconds * 1000)}`}
              {rules.maxAdvanceSeconds > 0 && `，最多提前 ${fmtSpan(rules.maxAdvanceSeconds * 1000)}`}。{!problem && e > s && `时长 ${fmtSpan(e - (active ? Date.parse(edit!.start) : s))}。`}
            </p>
            {problem && <Notice>{problem}</Notice>}
            {error && <Notice>{error}</Notice>}
            {!edit && preview !== null && (
              <Notice tone={preview > 0 ? "warm" : "idle"}>
                {preview > 0
                  ? `预约开始时，这台节点上其他用户的 ${preview} 个 DevPod 会被自动休眠（开始前 ${fmtSpan(rules.clearNoticeSeconds * 1000)} 会提醒他们）。`
                  : "这台节点上现在没有其他用户的 DevPod。"}
                预约期间只有你的 DevPod 能在这台节点上运行；到期后你在这台节点上的 DevPod 会被自动休眠。
              </Notice>
            )}
            {edit && !active && <p className="text-[11px] text-muted">修改等于取消后重新预约：新的时段按现在的时间重新排队。</p>}
            <div className="flex justify-end gap-2">
              <Dialog.Close asChild>
                <Button type="button">取消</Button>
              </Dialog.Close>
              <Button type="submit" variant="primary" disabled={!!problem || check.isPending || save.isPending}>
                {edit ? (active ? "延长" : "保存") : preview !== null ? "确认预约" : "下一步"}
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
  onDraft,
  onPick,
}: {
  node: NodeChoice;
  blocks: Reservation[];
  from: number;
  to: number;
  now: number;
  gran: number;
  admin: boolean;
  onDraft: (d: Draft) => void;
  onPick: (b: Reservation) => void;
}) {
  const track = useRef<HTMLDivElement>(null);
  const [drag, setDrag] = useState<{ a: number; b: number } | null>(null);
  const pct = (t: number) => `${Math.min(100, Math.max(0, ((t - from) / (to - from)) * 100))}%`;
  const at = (clientX: number) => {
    const r = track.current!.getBoundingClientRect();
    return from + (r.width > 0 ? Math.min(1, Math.max(0, (clientX - r.left) / r.width)) : 0) * (to - from);
  };
  const sel = drag ? selection(drag.a, drag.b, gran) : null;
  const bad = !!sel && (sel[0] < slotStart(now, gran) || !!overlapping(blocks, node.name, sel[0], sel[1]));
  return (
    <div className="flex items-stretch border-b border-line last:border-0">
      <div className="w-40 shrink-0 border-r border-line px-2 py-1.5">
        <div className="mono truncate text-xs font-medium" title={node.name}>
          {node.name}
        </div>
        <div className="truncate text-[11px] text-muted">{node.gpuTotal > 0 ? `${node.gpuModel ?? "GPU"} ×${node.gpuTotal}` : `${Math.round(node.cpu.allocatable)} 核`}</div>
      </div>
      <div
        ref={track}
        role="group"
        aria-label={`${node.name} 的预约`}
        className="relative h-11 min-w-0 flex-1 cursor-crosshair touch-none select-none"
        onPointerDown={(e) => {
          if ((e.target as HTMLElement).closest("button")) return;
          e.currentTarget.setPointerCapture?.(e.pointerId);
          const t = at(e.clientX);
          setDrag({ a: t, b: t });
        }}
        onPointerMove={(e) => drag && setDrag({ a: drag.a, b: at(e.clientX) })}
        onPointerUp={() => {
          if (sel && !bad) onDraft({ node: node.name, start: sel[0], end: sel[1] });
          setDrag(null);
        }}
        onPointerCancel={() => setDrag(null)}
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
          const style = { left: pct(Date.parse(b.start)), width: `calc(${pct(Date.parse(b.end))} - ${pct(Date.parse(b.start))})` };
          const title = `${b.user}：${fmtTime(b.start)} – ${fmtTime(b.end)}（${phaseLabel[b.phase]}）`;
          return canOpen ? (
            <button key={b.id} type="button" title={title} onClick={() => onPick(b)} style={style} className={`absolute inset-y-1 overflow-hidden rounded border px-1 text-left text-[11px] whitespace-nowrap hover:brightness-95 ${cls}`}>
              {text}
            </button>
          ) : (
            <div key={b.id} title={title} style={style} className={`absolute inset-y-1 overflow-hidden rounded border px-1 text-[11px] leading-8 whitespace-nowrap ${cls}`}>
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
            <span className="absolute -top-4 left-0 whitespace-nowrap rounded bg-surface px-1 font-medium shadow-sm">
              {fmtClock(sel[0])}–{fmtClock(sel[1])} · {fmtSpan(sel[1] - sel[0])}
              {bad && " · 不可预约"}
            </span>
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
  const [draft, setDraft] = useState<Draft | null>(null);
  const [picked, setPicked] = useState<Reservation | null>(null);
  const [cancel, setCancel] = useState<Reservation | null>(null);
  const del = useMutation({
    mutationFn: (id: string) => deleteReservation(id),
    onSettled: () => {
      void qc.invalidateQueries({ queryKey: ["reservations"] });
      void qc.invalidateQueries({ queryKey: ["node-choices"] });
    },
  });

  if (rules.isError) return <EmptyState tone="error" title="节点预约不可用" hint={apiMessage(rules.error)} />;
  if (!rules.data || !me) return <EmptyState title="加载中…" />;
  const g = rules.data.granularitySeconds * 1000;
  const all = q.data?.items ?? [];
  const live = all.filter((b) => b.phase !== "Cancelled");
  const nodes = (nodesQ.data?.items ?? []).filter((n) => n.reservable);
  const mine = all.filter((b) => b.mine).sort((a, b) => Date.parse(b.start) - Date.parse(a.start));
  const open = (b: Reservation) => setDraft({ node: b.node, start: Date.parse(b.start), end: Date.parse(b.end) + (b.phase === "Active" ? g : 0), edit: b });
  const first = slotStart(now, g) + g;

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
        <span className="text-[11px] text-muted">在空闲时段上拖动来预约；整台节点在预约期间只给你的 DevPod 用。北京时间。</span>
        <span className="flex-1" />
        <Button variant="primary" disabled={nodes.length === 0} onClick={() => setDraft({ node: nodes[0].name, start: first, end: first + 2 * g })}>
          预约节点
        </Button>
      </div>
      <Panel title="时间轴">
        {nodes.length === 0 ? (
          <EmptyState title="还没有开放预约的节点" hint={me.admin ? "在「节点」页把节点设为可预约。" : "需要管理员把节点设为可预约。"} />
        ) : (
          <div>
            <div className="flex border-b border-line text-[11px] text-muted">
              <div className="w-40 shrink-0 border-r border-line px-2 py-1">节点</div>
              <div className="relative h-6 min-w-0 flex-1">
                {Array.from({ length: days }, (_, i) => (
                  <span key={i} className="absolute top-1 truncate pl-1" style={{ left: `${(i / days) * 100}%`, width: `${100 / days}%` }}>
                    {fmtDay(from + i * DAY)}
                  </span>
                ))}
              </div>
            </div>
            {nodes.map((n) => (
              <Row key={n.name} node={n} blocks={live.filter((b) => b.node === n.name)} from={from} to={to} now={now} gran={g} admin={me.admin} onDraft={setDraft} onPick={setPicked} />
            ))}
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
          <EmptyState title="你还没有预约" />
        ) : (
          <table className="w-full border-collapse text-xs">
            <thead>
              <tr className="h-7 border-b border-line text-left text-[11px] text-muted">
                <th className="px-3 font-normal">节点</th>
                <th className="font-normal">时段</th>
                <th className="font-normal">时长</th>
                <th className="font-normal">状态</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {mine.map((b) => (
                <tr key={b.id} className="h-8 border-b border-line last:border-0">
                  <td className="mono px-3">{b.node}</td>
                  <td className="mono">
                    {fmtTime(b.start)} – {fmtTime(b.end)}
                  </td>
                  <td>{fmtSpan(Date.parse(b.end) - Date.parse(b.start))}</td>
                  <td>
                    <Badge tone={b.phase === "Active" ? "run" : b.phase === "Upcoming" ? "accent" : "neutral"}>{phaseLabel[b.phase]}</Badge>
                    {b.phase === "Cancelled" && b.cancelledBy && b.cancelledBy !== me.user && <span className="ml-1 text-muted">由 {b.cancelledBy} 取消</span>}
                  </td>
                  <td className="space-x-3 pr-3 text-right">
                    {(b.phase === "Upcoming" || b.phase === "Active") && (
                      <>
                        <button type="button" className="text-accent hover:underline" onClick={() => open(b)}>
                          {b.phase === "Active" ? "延长" : "修改"}
                        </button>
                        <button type="button" className="text-muted hover:text-fail" onClick={() => setCancel(b)}>
                          {b.phase === "Active" ? "提前释放" : "取消"}
                        </button>
                      </>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Panel>
      {draft && <ReserveDialog key={`${draft.node}-${draft.start}-${draft.edit?.id ?? ""}`} draft={draft} nodes={nodes} rules={rules.data} blocks={live} now={now} onClose={() => setDraft(null)} />}
      <ConfirmDialog
        open={!!picked}
        onOpenChange={(o) => !o && setPicked(null)}
        title={picked ? `${picked.node} · ${picked.mine ? "我的预约" : `${picked.user} 的预约`}` : ""}
        confirmLabel={picked?.mine ? (picked.phase === "Active" ? "延长…" : "修改…") : "取消这个预约"}
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
        title={cancel?.phase === "Active" ? "提前释放这台节点？" : "取消这个预约？"}
        confirmLabel={cancel?.phase === "Active" ? "提前释放" : "取消预约"}
        danger
        onConfirm={() => {
          if (cancel) del.mutate(cancel.id);
          setCancel(null);
        }}
      >
        {cancel && (
          <>
            {cancel.node}，{fmtTime(cancel.start)} – {fmtTime(cancel.end)}
            {!cancel.mine && `，预约人 ${cancel.user}`}。
            {cancel.phase === "Active" && "释放后，预约人在这台节点上的 DevPod 会被自动休眠，节点重新对所有人开放。"}
          </>
        )}
      </ConfirmDialog>
      {del.isError && <Notice>{apiMessage(del.error)}</Notice>}
    </div>
  );
}
