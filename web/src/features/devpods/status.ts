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
