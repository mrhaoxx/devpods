// How the queue says a job (design 2026-11-26): its size, its time limit,
// where it is and why — in a few words.
import type { Job } from "../../data/api";
import { dayStart, fmtClock, fmtTime } from "../../lib/reservations";

/** fmtHours: 3h, 1h30m, 40m. */
export function fmtHours(ms: number): string {
  const m = Math.max(0, Math.round(ms / 60_000));
  const h = Math.floor(m / 60);
  if (h > 0 && m % 60) return `${h}h${m % 60}m`;
  if (h > 0) return `${h}h`;
  return `${m}m`;
}

/** sizeText: "2 台 × 8 卡", or cores for a job without cards. */
export const sizeText = (j: Pick<Job, "nodes" | "gpus" | "cpus">) => (j.gpus > 0 ? `${j.nodes} 台 × ${j.gpus} 卡` : `${j.nodes} 台 × ${j.cpus} 核`);

/** whenText: 今天 14:00, 明天 02:10, or the date and time. */
export function whenText(t: string, now: number): string {
  const day = Math.round((dayStart(Date.parse(t)) - dayStart(now)) / 86_400_000);
  if (day === 0) return `今天 ${fmtClock(t)}`;
  if (day === 1) return `明天 ${fmtClock(t)}`;
  return fmtTime(t);
}

export const jobEnded = (j: Pick<Job, "phase">) => j.phase === "Succeeded" || j.phase === "Failed" || j.phase === "TimedOut" || j.phase === "Cancelled";

/** stateText: where a job is, and why it waits. */
export function stateText(j: Job, now: number): string {
  const d = j.detail ?? {};
  switch (j.phase) {
    case "Running": {
      if (!j.startedAt) return "● 运行中";
      const ran = now - Date.parse(j.startedAt);
      return `● 运行中 · 已 ${fmtHours(ran)}，最多再 ${fmtHours(j.timeLimitSeconds * 1000 - ran)}`;
    }
    case "Starting":
      return "● 启动中";
    case "Succeeded":
      return "已完成";
    case "Failed":
      if (d.exit) return `失败（退出码 ${d.exit}）`;
      if (j.reason === "rank-stopped") return "失败（有一台被停止）";
      if (j.reason === "gang-timeout") return `失败（几次都没能全部启动${d.why ? `：${d.why}` : ""}）`;
      return "失败";
    case "TimedOut":
      return "超时";
    case "Cancelled":
      return "已取消";
  }
  let why = "等待";
  switch (j.reason) {
    case "capacity":
      why = j.gpus > 0 ? `空闲卡不够（要 ${d.need ?? "?"}，剩 ${d.free ?? "?"}）` : `空闲核不够（要 ${d.needCPU ?? "?"}，剩 ${d.freeCPU ?? "?"}）`;
      break;
    case "behind":
      why = `排在 ${d.head ?? "前面的任务"} 后`;
      break;
    case "quota":
      why = "超出配额";
      break;
    case "preempting":
      why = "正在让出资源";
      break;
    case "preempted":
      why = "被抢占，重新排队";
      break;
    case "gang-timeout":
      why = "上次没能全部启动，重新排队";
      break;
  }
  // no space after a full-width bracket: （要 32，剩 18）· 预计 …
  const sep = why.endsWith("）") ? "· " : " · ";
  return `○ 等待 · ${why}${j.expectedStart ? `${sep}预计 ${whenText(j.expectedStart, now)} 前后` : ""}`;
}
