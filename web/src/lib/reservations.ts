// Time math for node reservations (reservation spec §3.1). Times align
// to the granularity in Asia/Shanghai, which is also how they are shown.
import type { NodeChoice, Reservation, ReservationRules } from "../data/api";

export const TZ = "Asia/Shanghai";
const TZ_OFFSET_MS = 8 * 3600_000; // no daylight saving

/** slotStart: the start of the slot holding t (ms). */
export function slotStart(t: number, granMs: number): number {
  const local = t + TZ_OFFSET_MS;
  return local - (((local % granMs) + granMs) % granMs) - TZ_OFFSET_MS;
}
/** dayStart: local midnight of the day holding t. */
export const dayStart = (t: number) => slotStart(t, 86_400_000);

/** selection turns a drag between two instants into [start, end): whole
 *  slots covering both ends, at least one slot. */
export function selection(a: number, b: number, granMs: number): [number, number] {
  const lo = Math.min(a, b);
  const hi = Math.max(a, b);
  const start = slotStart(lo, granMs);
  return [start, Math.max(slotStart(hi, granMs) + granMs, start + granMs)];
}

/** fromNow: a selection that starts in the past but reaches past first
 *  (the start of the slot now is in) starts at first instead; one wholly
 *  in the past is left as it is — it cannot be reserved. */
export function fromNow(sel: [number, number], first: number): [number, number] {
  return sel[0] < first && sel[1] > first ? [first, sel[1]] : sel;
}

/** periodError mirrors the server's rule check; null = fine. */
export function periodError(start: number, end: number, r: ReservationRules, now: number): string | null {
  const g = r.granularitySeconds * 1000;
  if (Number.isNaN(start) || Number.isNaN(end)) return "请填写开始和结束时间";
  if (slotStart(start, g) !== start || slotStart(end, g) !== end) return `开始和结束要对齐到 ${fmtSpan(g)} 一档`;
  if (end <= start) return "结束时间要晚于开始时间";
  if (r.maxDurationSeconds > 0 && end - start > r.maxDurationSeconds * 1000) return `单次最长 ${fmtSpan(r.maxDurationSeconds * 1000)}`;
  if (start < slotStart(now, g)) return "开始时间已经过去（最早从当前这一档开始）";
  if (r.maxAdvanceSeconds > 0 && start > now + r.maxAdvanceSeconds * 1000) return `最多提前 ${fmtSpan(r.maxAdvanceSeconds * 1000)} 预约`;
  return null;
}

/** fmtSpan: 5_400_000 → "1 小时 30 分钟". */
export function fmtSpan(ms: number): string {
  const m = Math.round(ms / 60_000);
  const d = Math.floor(m / 1440);
  const h = Math.floor((m % 1440) / 60);
  const min = m % 60;
  return [d && `${d} 天`, h && `${h} 小时`, min && `${min} 分钟`].filter(Boolean).join(" ") || "0 分钟";
}

const two = (n: number) => String(n).padStart(2, "0");
const parts = (t: number) => {
  const d = new Date(t + TZ_OFFSET_MS);
  return { y: d.getUTCFullYear(), mo: d.getUTCMonth() + 1, d: d.getUTCDate(), h: d.getUTCHours(), mi: d.getUTCMinutes(), wd: d.getUTCDay() };
};
/** fmtTime: "11-12 10:30" in Shanghai time. */
export function fmtTime(t: number | string): string {
  const p = parts(typeof t === "string" ? Date.parse(t) : t);
  return `${two(p.mo)}-${two(p.d)} ${two(p.h)}:${two(p.mi)}`;
}
export const fmtClock = (t: number | string) => fmtTime(t).slice(6);
export function fmtDay(t: number): string {
  const p = parts(t);
  return `${p.mo}/${p.d} ${"日一二三四五六"[p.wd]}`;
}
/** toInput / fromInput: <input type="datetime-local"> values, read as Shanghai time. */
export function toInput(t: number): string {
  const p = parts(t);
  return `${p.y}-${two(p.mo)}-${two(p.d)}T${two(p.h)}:${two(p.mi)}`;
}
export const fromInput = (v: string): number => (v ? Date.parse(`${v}:00+08:00`) : NaN);

// Amounts (reservations v2, design 2026-11-26): a reservation holds some of
// a machine's cards and cores, or — neither said — all of it. Memory is
// not reserved.

/** Amount: cards and cores. */
export type Amount = { gpu: number; cpu: number };

/** capOf: what a machine has to reserve. */
export const capOf = (n: NodeChoice): Amount => ({ gpu: n.gpuTotal, cpu: n.cpu.allocatable });

/** isWhole: the reservation is the whole machine. */
export const isWhole = (b: Pick<Reservation, "gpu" | "cpu">) => b.gpu == null && b.cpu == null;

/** amountOf: what b holds of a machine that has cap — all of it when whole; else what it says (an amount not said: none). */
export const amountOf = (b: Pick<Reservation, "gpu" | "cpu">, cap: Amount): Amount => (isWhole(b) ? cap : { gpu: b.gpu ?? 0, cpu: b.cpu ?? 0 });

/** cpuShare: the cores that go with gpu of cap's cards (whole cores, rounded down, as the console). */
export const cpuShare = (gpu: number, cap: Amount) => (cap.gpu > 0 ? Math.floor((cap.cpu * gpu) / cap.gpu) : 0);

const span = (b: Reservation) => [Date.parse(b.start), Date.parse(b.end)] as const;

/** bookedAt: what blocks hold together at t. */
export function bookedAt(blocks: Reservation[], t: number, cap: Amount): Amount {
  const sum = { gpu: 0, cpu: 0 };
  for (const b of blocks) {
    const [s, e] = span(b);
    if (s <= t && t < e) {
      const a = amountOf(b, cap);
      sum.gpu += a.gpu;
      sum.cpu += a.cpu;
    }
  }
  return sum;
}

/** freeIn: the least that is free in [from, to) beside blocks (skipId: the one being changed, left out) — nothing beside a whole one. */
export function freeIn(blocks: Reservation[], from: number, to: number, cap: Amount, skipId?: string): Amount {
  const over = blocks.filter((b) => b.id !== skipId && span(b)[0] < to && from < span(b)[1] && span(b)[0] < span(b)[1]);
  if (over.some(isWhole)) return { gpu: 0, cpu: 0 };
  let peak = bookedAt(over, from, cap);
  for (const b of over) {
    const s = span(b)[0];
    if (s > from && s < to) {
      const a = bookedAt(over, s, cap);
      peak = { gpu: Math.max(peak.gpu, a.gpu), cpu: Math.max(peak.cpu, a.cpu) };
    }
  }
  return { gpu: Math.max(0, cap.gpu - peak.gpu), cpu: Math.max(0, cap.cpu - peak.cpu) };
}

/** stackBand: where each block sits in a band of cap units (pick: its units) — from the bottom, by start, at the lowest place free all its time; with none, at the top, over. */
export function stackBand(blocks: Reservation[], cap: number, pick: (b: Reservation) => number): Map<string, { y0: number; y1: number; over: boolean }> {
  const out = new Map<string, { y0: number; y1: number; over: boolean }>();
  const placed: { s: number; e: number; y0: number; y1: number }[] = [];
  const sorted = [...blocks].sort((x, y) => span(x)[0] - span(y)[0] || (x.id < y.id ? -1 : x.id > y.id ? 1 : 0));
  for (const b of sorted) {
    const a = pick(b);
    const [s, e] = span(b);
    if (!(a > 0)) {
      out.set(b.id, { y0: 0, y1: 0, over: false });
      continue;
    }
    const busy = placed.filter((p) => p.s < e && s < p.e);
    const fits = (y: number) => y + a <= cap && busy.every((p) => y + a <= p.y0 || p.y1 <= y);
    const y = [0, ...busy.map((p) => p.y1)].sort((m, n) => m - n).find(fits);
    const pos = y === undefined ? { y0: Math.max(0, cap - a), y1: Math.max(0, cap - a) + a, over: true } : { y0: y, y1: y + a, over: false };
    out.set(b.id, pos);
    placed.push({ s, e, y0: pos.y0, y1: pos.y1 });
  }
  return out;
}

/** presets: the amounts offered first — the whole machine, then halves of its cards down to one (with their cores); a CPU machine, halves of its cores. */
export function presets(cap: Amount): { label: string; gpu?: number; cpu?: number }[] {
  const out: { label: string; gpu?: number; cpu?: number }[] = [{ label: "整台" }];
  if (cap.gpu > 0) {
    for (let n = Math.floor(cap.gpu / 2); n >= 1; n = Math.floor(n / 2)) out.push({ label: `${n} 卡`, gpu: n, cpu: cpuShare(n, cap) });
    return out;
  }
  for (const c of [Math.floor(cap.cpu / 2), Math.floor(cap.cpu / 4)]) if (c >= 1) out.push({ label: `${c} 核`, cpu: c });
  return out;
}

/** fmtAmount: 整台, "4 卡 · 96 核", or "16 核". */
export function fmtAmount(b: Pick<Reservation, "gpu" | "cpu">): string {
  if (isWhole(b)) return "整台";
  const cores = b.cpu != null ? `${b.cpu} 核` : "";
  if (b.gpu) return cores ? `${b.gpu} 卡 · ${cores}` : `${b.gpu} 卡`;
  return cores;
}
