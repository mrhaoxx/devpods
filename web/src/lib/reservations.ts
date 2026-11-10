// Time math for node reservations (reservation spec §3.1). Times align
// to the granularity in Asia/Shanghai, which is also how they are shown.
import type { Reservation, ReservationRules } from "../data/api";

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

/** overlapping: the live block on the node that [start, end) runs into. */
export function overlapping(blocks: Reservation[], node: string, start: number, end: number, skip?: string): Reservation | undefined {
  return blocks.find((b) => b.node === node && b.phase !== "Cancelled" && b.id !== skip && Date.parse(b.start) < end && start < Date.parse(b.end));
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
