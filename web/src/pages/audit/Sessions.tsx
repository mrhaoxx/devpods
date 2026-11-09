import { useState } from "react";
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { listAuditSessions, me } from "../../api";
import { SessionsTable } from "../../components/SessionsTable";
import { useNow } from "../../components/DevPodTable";
import { Shell } from "../../ui";
import { AuditHeader, FilterInput, LoadMore, RangeChips, useRange } from "./common";

export default function AuditSessions() {
  const meQ = useQuery({ queryKey: ["me"], queryFn: me });
  const now = useNow(10_000);
  const { range, setRange, from, to } = useRange();
  const [user, setUser] = useState("");
  const [devpod, setDevpod] = useState("");
  const [hasFiles, setHasFiles] = useState(false);
  const [hasAlerts, setHasAlerts] = useState(false);
  const filters = { from, to, user, devpod, hasFiles: hasFiles ? "true" : "", hasAlerts: hasAlerts ? "true" : "" };
  const q = useInfiniteQuery({
    queryKey: ["audit-sessions", filters],
    queryFn: ({ pageParam }) => listAuditSessions({ ...filters, cursor: pageParam }),
    initialPageParam: "",
    getNextPageParam: (last) => last.next || undefined,
  });
  const items = q.data?.pages.flatMap((p) => p.items) ?? [];

  return (
    <Shell wide>
      <AuditHeader title={meQ.data?.admin ? "Sessions" : "My sessions"} />
      <div className="mono mb-3 flex flex-wrap items-center gap-2 text-xs">
        <RangeChips range={range} setRange={setRange} />
        {meQ.data?.admin && <FilterInput value={user} onChange={setUser} placeholder="user" />}
        <FilterInput value={devpod} onChange={setDevpod} placeholder="devpod name" />
        <label className="flex items-center gap-1 text-muted">
          <input type="checkbox" checked={hasFiles} onChange={(e) => setHasFiles(e.target.checked)} /> with files
        </label>
        <label className="flex items-center gap-1 text-muted">
          <input type="checkbox" checked={hasAlerts} onChange={(e) => setHasAlerts(e.target.checked)} /> blocked / alerts
        </label>
      </div>
      <SessionsTable items={items} showDevPod now={now} />
      <LoadMore hasMore={q.hasNextPage} busy={q.isFetchingNextPage} onClick={() => q.fetchNextPage()} />
    </Shell>
  );
}
