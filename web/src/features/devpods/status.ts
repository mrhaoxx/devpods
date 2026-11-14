// The one-line explanation shown instead of metrics for a DevPod that is
// not running (UI spec §5.1): pull progress, hibernation, failure.
import type { DevPod, PodInfo, Reservation } from "../../data/api";
import { failureRetryIn, fmtAge } from "../../lib/format";
import { fmtClock } from "../../lib/reservations";

export function phaseSummary(dp: DevPod, _pod: PodInfo | undefined, now: Date): string | null {
  const st = dp.status ?? {};
  const phase = st.phase;
  if (phase === "Running") return null;
  if (phase === "Failed") {
    const msg = st.message || "启动失败";
    if (st.retryCount && st.lastFailureAt) {
      const s = failureRetryIn(st.retryCount, st.lastFailureAt, now);
      return s > 0 ? `${msg} · ${s}s 后重试` : `${msg} · 正在重试`;
    }
    return msg;
  }
  if (phase === "Stopped" || !dp.spec.running) {
    const age = st.hibernatedAt ? ` ${fmtAge(st.hibernatedAt, now)}` : "";
    const pvc = dp.spec.persistence ? `PVC ${dp.spec.persistence.size}` : "无持久化";
    return `已休眠${age} · ${pvc}`;
  }
  return st.message || "启动中";
}

/** ReservationsNow: the reservations around now (the console keeps and
 *  carries them out), what reservationNotes works from. */
export type ReservationsNow = { items: Reservation[]; noticeMs: number; now: number };
export type ReservationNote = { type: string; label: string; tone: "fail" | "warm"; message: string };

const pinnedNode = (dp: DevPod) => dp.spec.pod?.spec?.nodeSelector?.["kubernetes.io/hostname"] ?? "";
const lossWarning = (dp: DevPod) => (dp.spec.persistence ? "" : "；休眠后容器内数据丢失");

/** reservationNotes: what node reservations mean for a DevPod now — it is
 *  on (node: where its Pod is) or pinned to a node someone holds, soon
 *  will, or whose hold of the owner's is running out — and why one was
 *  hibernated. Most pressing first. */
export function reservationNotes(dp: DevPod, where: { node?: string; cluster?: string } = {}, rs?: ReservationsNow): ReservationNote[] {
  const notes: ReservationNote[] = [];
  const owner = dp.spec.owner;
  const pinned = pinnedNode(dp);
  const node = where.node || pinned;
  if (rs && node) {
    const on = rs.items.filter((r) => r.node === node && (r.cluster ?? "") === (where.cluster ?? "") && r.phase !== "Cancelled");
    const held = on.find((r) => Date.parse(r.start) <= rs.now && rs.now < Date.parse(r.end));
    if (held && held.user !== owner) {
      if (!dp.spec.running && pinned === node)
        notes.push({ type: "NodeReserved", label: "节点被预约", tone: "fail", message: `${node} 被 ${held.user} 预约至 ${fmtClock(held.end)}，期间不能在这台节点启动；可以改到其他节点` });
    } else if (dp.spec.running) {
      const next = on.find((r) => r.user !== owner && Date.parse(r.start) > rs.now && Date.parse(r.start) - rs.now <= rs.noticeMs);
      if (next) notes.push({ type: "ReservationClearing", label: "即将清场", tone: "warm", message: `${node} 将于 ${fmtClock(next.start)} 被 ${next.user} 预约，届时自动休眠${lossWarning(dp)}` });
      else if (held && Date.parse(held.end) - rs.now <= rs.noticeMs)
        notes.push({ type: "ReservationEnding", label: "预约将到期", tone: "warm", message: `预约将于 ${fmtClock(held.end)} 结束，届时自动休眠${lossWarning(dp)}` });
    }
  }
  // the annotation outlives a restart: it only says something while stopped
  const by = dp.metadata.annotations?.["devpod.io/hibernated-by"] ?? "";
  if (!dp.spec.running && by.startsWith("reservation/"))
    notes.push({ type: "HibernatedByReservation", label: "被预约清场休眠", tone: "warm", message: "这台节点被预约，DevPod 已自动休眠，数据保留。预约结束后可以重新启动，或改到其他节点。" });
  if (!dp.spec.running && by.startsWith("reservation-end/"))
    notes.push({ type: "HibernatedAtReservationEnd", label: "预约结束休眠", tone: "warm", message: "节点预约已结束，DevPod 已自动休眠。可以重新启动，或再预约这台节点。" });
  return notes;
}
