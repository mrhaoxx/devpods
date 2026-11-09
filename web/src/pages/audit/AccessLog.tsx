import { Link } from "react-router-dom";
import { useInfiniteQuery } from "@tanstack/react-query";
import { listAccessLog } from "../../api";
import { Shell, cx } from "../../ui";
import { AuditHeader, LoadMore, RangeChips, Table, fmtTS, td, useRange } from "./common";

export default function AuditAccessLog() {
  const { range, setRange, from, to } = useRange();
  const q = useInfiniteQuery({
    queryKey: ["audit-access", from, to],
    queryFn: ({ pageParam }) => listAccessLog({ from, to, cursor: pageParam }),
    initialPageParam: "",
    getNextPageParam: (last) => last.next || undefined,
  });
  const items = q.data?.pages.flatMap((p) => p.items) ?? [];
  return (
    <Shell wide>
      <AuditHeader title="Access log" />
      <div className="mono mb-3 text-xs"><RangeChips range={range} setRange={setRange} /></div>
      <Table head={["Time", "Viewer", "Action", "Session", "Target", "From"]} empty={!q.isLoading && items.length === 0}>
        {items.map((a, i) => (
          <tr key={i} className="h-[28px] border-b border-line/60 last:border-0 hover:bg-sunk/50">
            <td className={cx(td, "text-muted")}>{fmtTS(a.ts)}</td>
            <td className={td}>{a.viewer}</td>
            <td className={td}>{a.action}</td>
            <td className={td}>
              {a.target_session && <Link to={`/audit/sessions/${a.target_session}`} className="text-accent hover:underline">{a.target_session.slice(0, 10)}…</Link>}
            </td>
            <td className={cx(td, "text-muted")}>{a.target_file}</td>
            <td className={cx(td, "text-faint")}>{a.client_ip}</td>
          </tr>
        ))}
      </Table>
      <LoadMore hasMore={q.hasNextPage} busy={q.isFetchingNextPage} onClick={() => q.fetchNextPage()} />
    </Shell>
  );
}
