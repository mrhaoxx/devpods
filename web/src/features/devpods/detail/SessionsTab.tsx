import { Link } from "react-router-dom";
import type { SessionSummary } from "../../../data/api";
import { Badge } from "../../../design/Badge";
import { EmptyState } from "../../../design/EmptyState";
import { fmtDuration } from "../../../lib/format";

/** Sessions of one DevPod: the server returns only those the viewer may
 *  see; replay links go to the session page. */
export function SessionList({ items, error, now, onlyOpen }: { items?: SessionSummary[]; error?: unknown; now: Date; onlyOpen?: boolean }) {
  if (error) return <EmptyState tone="error" title="审计数据暂不可用" hint={String(error)} />;
  const rows = (items ?? []).filter((s) => !onlyOpen || !s.closed_at);
  if (!items) return <EmptyState title="加载中…" />;
  if (rows.length === 0) return <EmptyState title={onlyOpen ? "没有在线会话" : "没有会话"} />;
  return (
    <ul className="divide-y divide-line text-xs">
      {rows.map((s) => {
        const end = s.closed_at ? new Date(s.closed_at) : now;
        return (
          <li key={s.session_id} className="flex flex-wrap items-center gap-2 px-3 py-1.5">
            <span className="mono">{s.user}</span>
            <span className="mono text-muted">{s.client_ip}</span>
            <span className="text-muted">{new Date(s.opened_at).toLocaleString()}</span>
            <span className="mono text-muted">{fmtDuration(end.getTime() - new Date(s.opened_at).getTime())}</span>
            {!s.closed_at && <Badge tone="run">在线</Badge>}
            {s.policy_hits > 0 && <Badge tone="warm">拦截 {s.policy_hits}</Badge>}
            {s.alerts > 0 && <Badge tone="fail">告警 {s.alerts}</Badge>}
            <Link to={`/sessions/${s.session_id}`} className="ml-auto text-accent hover:underline">
              {s.closed_at ? "回放" : "直播"}
            </Link>
          </li>
        );
      })}
    </ul>
  );
}
