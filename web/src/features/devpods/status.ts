// The one-line explanation shown instead of metrics for a DevPod that is
// not running (UI spec §5.1): pull progress, hibernation, failure.
import type { DevPod, PodInfo } from "../../data/api";
import { failureRetryIn, fmtAge } from "../../lib/format";

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

/** reservationNotes: what node reservations are doing to a DevPod
 *  (conditions set by the controller), most pressing first. */
const NOTE: Record<string, [label: string, tone: "fail" | "warm"]> = {
  NodeReserved: ["节点被预约", "fail"],
  ReservationClearing: ["即将清场", "warm"],
  ReservationEnding: ["预约将到期", "warm"],
};
export function reservationNotes(dp: DevPod): { type: string; label: string; tone: "fail" | "warm"; message: string }[] {
  const notes = Object.keys(NOTE).flatMap((type) => {
    const c = dp.status?.conditions?.find((x) => x.type === type && x.status === "True");
    if (!c) return [];
    // pinned to a node someone else holds: not hibernated, just waiting
    const more = type === "NodeReserved" ? "节点空出来后会自动启动，不需要操作。" : "";
    return [{ type, label: NOTE[type][0], tone: NOTE[type][1], message: [c.message, more].filter(Boolean).join("。").replace("。。", "。") }];
  });
  // the annotation outlives a restart: it only says something while stopped
  if (!dp.spec.running && dp.metadata.annotations?.["devpod.io/hibernated-by"]?.startsWith("reservation/"))
    notes.push({ type: "HibernatedByReservation", label: "被预约清场休眠", tone: "warm", message: "这台节点被预约，DevPod 已自动休眠，数据保留。预约结束后可以重新启动，或改到其他节点。" });
  return notes;
}
