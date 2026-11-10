import { useState } from "react";
import { Link } from "react-router-dom";
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { fileContentURL, me, searchFiles } from "../../api";
import { fmtBytes } from "../../lib/format";
import { Shell, cx } from "../../ui";
import { AuditHeader, FilterInput, LoadMore, RangeChips, Table, fmtTS, td, useRange } from "./common";

export default function AuditFiles() {
  const meQ = useQuery({ queryKey: ["me"], queryFn: me });
  const { range, setRange, from, to } = useRange();
  const [path, setPath] = useState("");
  const [sha, setSha] = useState("");
  const [user, setUser] = useState("");
  const filters = { from, to, path, sha256: sha, user };
  const q = useInfiniteQuery({
    queryKey: ["audit-files", filters],
    queryFn: ({ pageParam }) => searchFiles({ ...filters, cursor: pageParam }),
    initialPageParam: "",
    getNextPageParam: (last) => last.next || undefined,
  });
  const items = q.data?.pages.flatMap((p) => p.items) ?? [];
  return (
    <Shell wide>
      <AuditHeader title="File transfers" />
      <div className="mono mb-3 flex flex-wrap items-center gap-2 text-xs">
        <RangeChips range={range} setRange={setRange} />
        <FilterInput value={path} onChange={setPath} placeholder="path contains…" />
        <FilterInput value={sha} onChange={setSha} placeholder="sha256" />
        {meQ.data?.admin && <FilterInput value={user} onChange={setUser} placeholder="user" />}
      </div>
      <Table head={["Time", "User", "Proto", "Op", "Path", "Size", "SHA-256", ""]} empty={!q.isLoading && items.length === 0}>
        {items.map((f) => (
          <tr key={f.event_id} className="h-[28px] border-b border-line/60 last:border-0 hover:bg-sunk/50">
            <td className={cx(td, "text-muted")}>{fmtTS(f.ts)}</td>
            <td className={td}>{f.user}</td>
            <td className={cx(td, "text-muted")}>{f.protocol}</td>
            <td className={td}>
              {f.op}
              {f.blocked && <span className="ml-1 rounded border border-warm/30 bg-warm-soft px-1 text-[9.5px] text-warm">blocked</span>}
              {!f.blocked && f.s3_key && !f.complete && <span className="ml-1 rounded border border-line px-1 text-[9.5px] text-muted">incomplete</span>}
            </td>
            <td className={cx(td, "max-w-[28rem] truncate")} title={f.path2 ? `${f.path} → ${f.path2}` : f.path}>
              {f.path}
              {f.path2 && <span className="text-faint"> → {f.path2}</span>}
            </td>
            <td className={cx(td, "text-muted")}>{f.s3_key ? fmtBytes(f.size) : "—"}</td>
            <td className={cx(td, "text-faint")} title={f.sha256}>{f.sha256.slice(0, 12)}</td>
            <td className={td}>
              {f.s3_key && (
                <a
                  href={fileContentURL(f.event_id)}
                  onClick={(e) => {
                    if (!confirm("此操作将被记录")) e.preventDefault();
                  }}
                  className="mr-2 text-accent hover:underline"
                >
                  download
                </a>
              )}
              <Link to={`/audit/sessions/${f.session_id}`} className="text-accent hover:underline">session</Link>
            </td>
          </tr>
        ))}
      </Table>
      <LoadMore hasMore={q.hasNextPage} busy={q.isFetchingNextPage} onClick={() => q.fetchNextPage()} />
    </Shell>
  );
}
