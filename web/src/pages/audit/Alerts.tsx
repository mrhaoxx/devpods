import { Link } from "react-router-dom";
import { useInfiniteQuery } from "@tanstack/react-query";
import { listAlerts } from "../../api";
import { Shell, cx } from "../../ui";
import { AuditHeader, LoadMore, RangeChips, Table, fmtTS, td, useRange } from "./common";

export default function AuditAlerts() {
  const { range, setRange, from, to } = useRange();
  const q = useInfiniteQuery({
    queryKey: ["audit-alerts", from, to],
    queryFn: ({ pageParam }) => listAlerts({ from, to, cursor: pageParam }),
    initialPageParam: "",
    getNextPageParam: (last) => last.next || undefined,
  });
  const items = q.data?.pages.flatMap((p) => p.items) ?? [];
  return (
    <Shell wide>
      <AuditHeader title="Alerts" />
      <div className="mono mb-3 text-xs"><RangeChips range={range} setRange={setRange} /></div>
      <Table head={["Time", "Kind", "Source", "Detail", ""]} empty={!q.isLoading && items.length === 0}>
        {items.map((a, i) => (
          <tr key={i} className="h-[28px] border-b border-line/60 last:border-0 hover:bg-sunk/50">
            <td className={cx(td, "text-muted")}>{fmtTS(a.ts)}</td>
            <td className={cx(td, "text-fail")}>{a.kind}</td>
            <td className={cx(td, "text-muted")}>{a.source}</td>
            <td className={cx(td, "max-w-[40rem] truncate")} title={a.detail}>{a.detail}</td>
            <td className={td}>
              {a.session_id && <Link to={`/audit/sessions/${a.session_id}`} className="text-accent hover:underline">session</Link>}
            </td>
          </tr>
        ))}
      </Table>
      <LoadMore hasMore={q.hasNextPage} busy={q.isFetchingNextPage} onClick={() => q.fetchNextPage()} />
    </Shell>
  );
}
