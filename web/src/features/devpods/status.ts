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
    return dp.spec.persistence ? `已休眠${age} · PVC ${dp.spec.persistence.size}` : `已休眠${age}`;
  }
  return st.message || "启动中";
}

/** ReservationsNow: the reservations around now (the console keeps and
 *  carries them out), what reservationNotes works from. */
export type ReservationsNow = { items: Reservation[]; noticeMs: number; now: number };
export type ReservationNote = { type: string; label: string; tone: "fail" | "warm"; message: string };

/** pinnedNode: the node a DevPod is pinned to ("" when the scheduler picks). */
export const pinnedNode = (dp: DevPod) => dp.spec.pod?.spec?.nodeSelector?.["kubernetes.io/hostname"] ?? "";
const lossWarning = (dp: DevPod) => (dp.spec.persistence ? "" : "；休眠后容器内数据丢失");

/** reservationNotes: what node reservations mean for a DevPod now — the
 *  owner's reservation of the machine it is on (node: where its Pod is)
 *  running out — and why one was hibernated. Reservations are permits:
 *  other people's keep nobody off, so they are not mentioned. */
export function reservationNotes(dp: DevPod, where: { node?: string; cluster?: string } = {}, rs?: ReservationsNow): ReservationNote[] {
  const notes: ReservationNote[] = [];
  const owner = dp.spec.owner;
  const node = where.node || pinnedNode(dp);
  if (rs && node && dp.spec.running) {
    const own = rs.items.filter((r) => r.node === node && (r.cluster ?? "") === (where.cluster ?? "") && r.user === owner && r.phase !== "Cancelled");
    const covering = own.filter((r) => Date.parse(r.start) <= rs.now && rs.now < Date.parse(r.end));
    if (covering.length > 0) {
      // the end of the stretch they cover, one following on from another
      let end = Math.max(...covering.map((r) => Date.parse(r.end)));
      for (let more = true; more; ) {
        more = false;
        for (const r of own) {
          if (Date.parse(r.start) <= end && Date.parse(r.end) > end) {
            end = Date.parse(r.end);
            more = true;
          }
        }
      }
      if (end - rs.now <= rs.noticeMs)
        notes.push({ type: "ReservationEnding", label: "预约将到期", tone: "warm", message: `你对 ${node} 的预约将于 ${fmtClock(end)} 结束；如果这个 DevPod 只能靠预约在这台机器上运行，届时会被自动休眠${lossWarning(dp)}` });
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
