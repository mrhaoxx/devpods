// How a link is: since when, its round trip, what it carries — the same
// cells for a cluster's agent, a gateway replica and a cluster's link to
// a gateway.
import { fmtBytes, fmtDuration, fmtRate } from "../../lib/format";
import type { LinkStats } from "./api";

export const ago = (iso?: string) => {
  if (!iso) return "—";
  const s = Math.max(0, Math.round((Date.now() - new Date(iso).getTime()) / 1000));
  if (s < 60) return "刚刚";
  if (s < 3600) return `${Math.floor(s / 60)} 分钟前`;
  if (s < 86400) return `${Math.floor(s / 3600)} 小时前`;
  return `${Math.floor(s / 86400)} 天前`;
};

const at = (iso: string) => new Date(iso).toLocaleString("zh-CN");

/** Since: how long it has been up (when, on hover). */
export function Since({ l }: { l: LinkStats }) {
  return <span title={`连上于 ${at(l.since)}`}>已连 {fmtDuration(Date.now() - new Date(l.since).getTime())}</span>;
}

/** Rtt: the latest ping's round trip; an end of before pings both ways does not answer. */
export function Rtt({ l }: { l?: LinkStats }) {
  if (!l) return <span className="text-faint">—</span>;
  if (!l.rttMs) return <span className="text-faint" title="对端版本较旧，不回应延迟测量">—</span>;
  const ms = l.rttMs;
  return <span className={`mono ${ms > 200 ? "text-warm" : ""}`}>{ms < 10 ? ms.toFixed(1) : Math.round(ms)} ms</span>;
}

/** Traffic: received / sent per second, the totals and the streams on hover. */
export function Traffic({ l }: { l?: LinkStats }) {
  if (!l) return <span className="text-faint">—</span>;
  return (
    <span className="mono whitespace-nowrap" title={`累计 收 ${fmtBytes(l.rx)} · 发 ${fmtBytes(l.tx)} · ${l.streams} 个流在用，${l.idle} 个空闲`}>
      {fmtRate(l.rxRate)} / {fmtRate(l.txRate)}
    </span>
  );
}
