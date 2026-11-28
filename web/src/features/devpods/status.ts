// The one-line explanation shown instead of metrics for a DevPod that is
// not running (UI spec §5.1): pull progress, hibernation, failure.
import type { DevPod, PodInfo, Reservation } from "../../data/api";
import { failureRetryIn, fmtAge } from "../../lib/format";
import { fmtClock } from "../../lib/reservations";
import { whenText } from "../queue/jobs";

/** QueueState: where a DevPod waiting in the queue's line stands. */
export type QueueState = { position?: number; reason?: string; detail?: Record<string, string>; expected?: string };

/** queueOf: a DevPod's place in the queue's line (null: not in it). */
export function queueOf(dp: DevPod): QueueState | null {
  const a = dp.metadata.annotations ?? {};
  if (!a["devpod.io/queued-at"]) return null;
  try {
    return JSON.parse(a["devpod.io/queue-state"] ?? "{}") as QueueState;
  } catch {
    return {};
  }
}

/** queuePriorityOf: the priority a DevPod waits in line at (0: none said). */
export function queuePriorityOf(dp: DevPod): number {
  return Number(dp.metadata.annotations?.["devpod.io/queue-priority"] ?? 0) || 0;
}

/** queueWhy: why it waits, in a few words. */
export function queueWhy(q: QueueState): string {
  switch (q.reason) {
    case "capacity":
      return "等空闲资源";
    case "behind":
      return `排在 ${(q.detail?.head ?? "前面的").replace(/^devpod\//, "DevPod ")} 后`;
    case "quota":
      return "超出配额";
    case "preempting":
      return "正在让出资源";
    case "never":
      return "没有机器放得下（看看要的资源，或绑定的机器）";
  }
  return "";
}

export function phaseSummary(dp: DevPod, _pod: PodInfo | undefined, now: Date): string | null {
  const st = dp.status ?? {};
  const phase = st.phase;
  const q = queueOf(dp);
  if (q && !dp.spec.running) {
    const why = queueWhy(q);
    const when = q.expected ? ` · 预计 ${whenText(q.expected, now.getTime())} 前后` : "";
    return `排队中${q.position ? ` #${q.position}` : ""}${why ? ` · ${why}` : ""}${when}`;
  }
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
 *  running out, another's that starts and needs what it uses (the
 *  console marks it), and why one was hibernated. */
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
  // in a starting reservation's way: when it goes (the console's mark)
  const clearAt = dp.metadata.annotations?.["devpod.io/clear-at"];
  if (dp.spec.running && clearAt)
    notes.push({
      type: "ClearingSoon",
      label: "将被清场",
      tone: "warm",
      message: `${where.node || pinnedNode(dp)} 的预约 ${fmtClock(clearAt)} 起需要这台机器上的资源，届时这个 DevPod 会被自动休眠${lossWarning(dp)}；可以先改到其他机器。`,
    });
  // the annotation outlives a restart: it only says something while stopped
  const by = dp.metadata.annotations?.["devpod.io/hibernated-by"] ?? "";
  if (!dp.spec.running && by.startsWith("reservation/"))
    notes.push({ type: "HibernatedByReservation", label: "被预约清场休眠", tone: "warm", message: "这台节点被预约，DevPod 已自动休眠，数据保留。预约结束后可以重新启动，或改到其他节点。" });
  if (!dp.spec.running && by.startsWith("reservation-end/"))
    notes.push({ type: "HibernatedAtReservationEnd", label: "预约结束休眠", tone: "warm", message: "节点预约已结束，DevPod 已自动休眠。可以重新启动，或再预约这台节点。" });
  if (!dp.spec.running && by === "machine-closed")
    notes.push({ type: "HibernatedMachineClosed", label: "机器关闭预约", tone: "warm", message: "这台机器不再开放预约，只能靠预约在上面运行的 DevPod 已自动休眠，数据保留。重新启动会排队分到其他机器。" });
  // the queue put it where a reservation will want its room later
  const expectClear = dp.metadata.annotations?.["devpod.io/expect-clear"];
  if (dp.spec.running && expectClear && !clearAt)
    notes.push({
      type: "ExpectClear",
      label: "预计被清场",
      tone: "warm",
      message: `${where.node || pinnedNode(dp)} 在 ${fmtClock(expectClear)} 有预约需要这台机器的资源，届时这个 DevPod 会被自动休眠${lossWarning(dp)}；先停下再启动，会排到别的机器。`,
    });
  return notes;
}
