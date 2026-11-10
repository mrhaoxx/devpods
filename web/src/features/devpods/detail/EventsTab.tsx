import type { K8sEvent } from "../../../data/api";
import { EmptyState } from "../../../design/EmptyState";

/** Events grouped by reason+message, newest first. */
export function groupEvents(byUID: Record<string, K8sEvent>): (K8sEvent & { total: number })[] {
  const byKey = new Map<string, K8sEvent & { total: number }>();
  for (const ev of Object.values(byUID)) {
    const key = `${ev.reason}:${ev.message}`;
    const cur = byKey.get(key);
    if (!cur || (ev.lastTimestamp ?? "") > (cur.lastTimestamp ?? "")) byKey.set(key, { ...ev, total: (cur?.total ?? 0) + (ev.count ?? 1) });
    else cur.total += ev.count ?? 1;
  }
  return [...byKey.values()].sort((a, b) => (b.lastTimestamp ?? "").localeCompare(a.lastTimestamp ?? ""));
}

export function EventList({ events }: { events: Record<string, K8sEvent> }) {
  const list = groupEvents(events);
  if (list.length === 0) return <EmptyState title="没有事件" />;
  return (
    <ul className="divide-y divide-line text-xs">
      {list.map((e, i) => (
        <li key={i} className="flex gap-2 px-3 py-1.5">
          <time className="mono shrink-0 text-faint">{e.lastTimestamp ? new Date(e.lastTimestamp).toLocaleTimeString() : ""}</time>
          <span className={`shrink-0 font-medium ${e.type === "Warning" ? "text-fail" : "text-muted"}`}>{e.reason}</span>
          <span className="min-w-0 flex-1 break-words">{e.message}</span>
          {e.total > 1 && <span className="mono shrink-0 text-faint">×{e.total}</span>}
        </li>
      ))}
    </ul>
  );
}
