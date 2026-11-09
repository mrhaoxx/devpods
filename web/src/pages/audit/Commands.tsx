import { useState } from "react";
import { Link } from "react-router-dom";
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { me, searchCommands } from "../../api";
import { Shell, cx } from "../../ui";
import { AuditHeader, FilterInput, LoadMore, RangeChips, Table, fmtTS, td, useRange } from "./common";

export default function AuditCommands() {
  const meQ = useQuery({ queryKey: ["me"], queryFn: me });
  const { range, setRange, from, to } = useRange();
  const [text, setText] = useState("");
  const [user, setUser] = useState("");
  const [devpod, setDevpod] = useState("");
  const filters = { from, to, q: text, user, devpod };
  const q = useInfiniteQuery({
    queryKey: ["audit-commands", filters],
    queryFn: ({ pageParam }) => searchCommands({ ...filters, cursor: pageParam }),
    initialPageParam: "",
    getNextPageParam: (last) => last.next || undefined,
  });
  const items = q.data?.pages.flatMap((p) => p.items) ?? [];
  return (
    <Shell wide>
      <AuditHeader title="Command search" />
      <div className="mono mb-3 flex flex-wrap items-center gap-2 text-xs">
        <RangeChips range={range} setRange={setRange} />
        <FilterInput value={text} onChange={setText} placeholder="command contains…" />
        {meQ.data?.admin && <FilterInput value={user} onChange={setUser} placeholder="user" />}
        <FilterInput value={devpod} onChange={setDevpod} placeholder="devpod name" />
      </div>
      <Table head={["Time", "User", "Command", "Exit", "Source", ""]} empty={!q.isLoading && items.length === 0}>
        {items.map((c) => (
          <tr key={c.event_id} className="h-[28px] border-b border-line/60 last:border-0 hover:bg-sunk/50">
            <td className={cx(td, "text-muted")}>{fmtTS(c.ts)}</td>
            <td className={td}>{c.user}</td>
            <td className={cx(td, "max-w-[40rem] truncate text-ink")} title={c.cmdline}>{c.cmdline}</td>
            <td className={cx(td, c.exit_code ? "text-fail" : "text-muted")}>{c.exit_code ?? "—"}</td>
            <td className={cx(td, "text-faint")}>{c.source}</td>
            <td className={td}>
              {c.session_id && <Link to={`/audit/sessions/${c.session_id}`} className="text-accent hover:underline">session</Link>}
            </td>
          </tr>
        ))}
      </Table>
      <LoadMore hasMore={q.hasNextPage} busy={q.isFetchingNextPage} onClick={() => q.fetchNextPage()} />
    </Shell>
  );
}
