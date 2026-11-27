// 记录: what was done to one DevPod or job, newest first (the audit
// store's events), on its own page.
import { useInfiniteQuery } from "@tanstack/react-query";
import { ApiFailure, type AuditEvent, type Page } from "../../data/api";
import { EmptyState } from "../../design/EmptyState";
import { actionLabel, viaLabel } from "./events";
import { fmtTS, LoadMore } from "./common";

/** HistoryList: load(cursor) gives a page (no cursor: the newest); 加载更多 the next. */
export function HistoryList({ queryKey, load }: { queryKey: unknown[]; load: (cursor?: string) => Promise<Page<AuditEvent>> }) {
  const q = useInfiniteQuery({
    queryKey,
    queryFn: ({ pageParam }) => load(pageParam || undefined),
    initialPageParam: "",
    getNextPageParam: (last) => last.next || undefined,
    retry: false,
  });
  if (q.isError) {
    const off = q.error instanceof ApiFailure && q.error.body.code === "AUDIT_DISABLED";
    return <EmptyState title={off ? "审计没有开启：没有记录" : `读不到记录：${String(q.error)}`} />;
  }
  if (q.isLoading) return <EmptyState title="加载中…" />;
  const items = q.data?.pages.flatMap((p) => p.items ?? []) ?? [];
  if (items.length === 0) return <EmptyState title="还没有记录" />;
  return (
    <>
      <ul className="divide-y divide-line text-xs">
        {items.map((e, i) => (
          <li key={i} className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5 px-3 py-1.5">
            <time className="mono shrink-0 text-faint">{fmtTS(e.ts)}</time>
            <span className="shrink-0 font-medium">{actionLabel(e.action)}</span>
            <span className="mono shrink-0 text-muted" title={e.client_ip}>
              {e.actor}
            </span>
            <span className="shrink-0 text-faint">{viaLabel(e.via)}{e.origin ? `（${e.origin}）` : ""}</span>
            {e.node && <span className="mono shrink-0 text-faint">{e.node}</span>}
            {e.detail && <span className="min-w-0 basis-full break-words text-muted sm:basis-auto sm:flex-1">{e.detail}</span>}
            {e.reason && <span className="min-w-0 break-words text-faint">原因：{e.reason}</span>}
          </li>
        ))}
      </ul>
      <LoadMore hasMore={q.hasNextPage} busy={q.isFetchingNextPage} onClick={() => void q.fetchNextPage()} />
    </>
  );
}
