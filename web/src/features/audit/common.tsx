// Shared bits of the audit pages: the time range in the URL, paged
// queries, the error state when ClickHouse is unavailable.
import { useAutoRefresh } from "../../lib/autoRefresh";
import { RefreshControl } from "../../design/RefreshControl";
import { useDebounced } from "../../lib/useDebounced";
import { useMemo, useEffect, useRef } from "react";
import { useInfiniteQuery } from "@tanstack/react-query";
import { ApiFailure, type Page } from "../../data/api";
import { EmptyState } from "../../design/EmptyState";
import { Button } from "../../design/Button";
import { Input } from "../../design/Form";
import type { ColumnDef } from "@tanstack/react-table";

export const RANGES: Record<string, number> = { "24h": 1, "7d": 7, "30d": 30, "90d": 90 };

/** fromTo turns a range key into RFC 3339 bounds (minute-stable, so the
 *  query key does not change on every render). */
export function fromTo(range: string): { from: string; to: string } {
  const minute = Math.floor(Date.now() / 60_000) * 60_000 + 60_000;
  const days = RANGES[range] ?? 7;
  return { to: new Date(minute).toISOString(), from: new Date(minute - days * 86400_000).toISOString() };
}

export function RangePicker({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  return (
    <span role="radiogroup" aria-label="时间范围" className="inline-flex rounded-md border border-line bg-sunk p-0.5 text-xs">
      {Object.keys(RANGES).map((r) => (
        <button
          key={r}
          type="button"
          role="radio"
          aria-checked={value === r}
          onClick={() => onChange(r)}
          className={`rounded px-2 py-0.5 ${value === r ? "bg-surface font-medium shadow-sm" : "text-muted hover:text-ink"}`}
        >
          {r}
        </button>
      ))}
    </span>
  );
}

export function FilterBox({ label, value, onChange, width = "w-40" }: { label: string; value: string; onChange: (v: string) => void; width?: string }) {
  // each value is a query to the audit store: asked once typing pauses
  const [text, setText] = useDebounced(value, onChange, 300);
  return <Input aria-label={label} placeholder={label} value={text} onChange={(e) => setText(e.target.value)} className={`mono !h-7 ${width} text-xs`} />;
}

/** usePaged pages through a cursor API; the filters form the query key. */
export function usePaged<T>(
  key: string,
  filters: Record<string, string>,
  fetch: (q: Record<string, string | undefined>) => Promise<Page<T>>,
  opts: { enabled?: boolean; refetchInterval?: number } = {},
) {
  // how often it refreshes is the page's own choice (off until chosen),
  // unless the caller says
  const [every, setEvery] = useAutoRefresh(`audit.${key}`, 0);
  const q = useInfiniteQuery({
    queryKey: ["audit", key, filters],
    queryFn: ({ pageParam }) => fetch({ ...filters, cursor: pageParam || undefined }),
    initialPageParam: "",
    getNextPageParam: (last) => last.next || undefined,
    retry: false,
    refetchInterval: every || false,
    ...opts,
  });
  const items = useMemo(() => q.data?.pages.flatMap((p) => p.items ?? []) ?? [], [q.data]);
  return { ...q, items, every, setEvery };
}

/** PagedRefresh: a paged list's 自动刷新 and refresh now. */
export function PagedRefresh({ q }: { q: ReturnType<typeof usePaged<unknown>> }) {
  return <RefreshControl every={q.every} onEvery={q.setEvery} onRefresh={() => void q.refetch()} busy={q.isFetching && !q.isFetchingNextPage} />;
}

/** endOf: what a list does at its end — the next page, when there is one and none is coming. */
export const endOf = (q: { hasNextPage?: boolean; isFetchingNextPage?: boolean; fetchNextPage(): unknown }) =>
  q.hasNextPage && !q.isFetchingNextPage ? () => void q.fetchNextPage() : undefined;

export function AuditError({ error }: { error: unknown }) {
  if (error instanceof ApiFailure && error.code === "AMBIGUOUS_CLUSTER")
    return <EmptyState title="多个集群都有这个名字的 DevPod" hint="在顶栏的集群筛选里选一个集群，再按 DevPod 查。" />;
  return <EmptyState tone="error" title="审计数据暂不可用" hint={error instanceof ApiFailure ? error.body.message : String(error)} />;
}

export function LoadMore({ hasMore, busy, onClick, auto }: { hasMore?: boolean; busy?: boolean; onClick: () => void; /** load when it comes into sight */ auto?: boolean }) {
  const el = useRef<HTMLDivElement>(null);
  const go = useRef(onClick);
  go.current = onClick;
  useEffect(() => {
    const node = el.current;
    if (!auto || !node || !hasMore || busy || typeof IntersectionObserver === "undefined") return;
    const io = new IntersectionObserver(([e]) => e.isIntersecting && go.current());
    io.observe(node);
    return () => io.disconnect();
  }, [auto, hasMore, busy]);
  if (!hasMore) return null;
  return (
    <div ref={el} className="p-2 text-center">
      <Button variant="ghost" disabled={busy} onClick={onClick}>
        {busy ? "加载中…" : "加载更多"}
      </Button>
    </div>
  );
}

export const fmtTS = (iso: string) => new Date(iso).toLocaleString();

/** KeyName: the key a session signed in with — its whole SHA256
 *  fingerprint, and its name among the user's keys when it is still one
 *  of them. Through a trusted proxy (the web terminal) it is the proxy's
 *  key. */
export function KeyName({ fp, name, proxy }: { fp?: string; name?: string; proxy?: string }) {
  if (!fp) return <span className="text-faint">—</span>;
  return (
    <span className="inline-flex min-w-0 items-baseline gap-1.5" title={proxy ? `${proxy} 的密钥` : name ? undefined : "不在该用户现在的公钥里"}>
      {name && <span className="shrink-0">{name}</span>}
      <span className="mono truncate text-muted">{fp}</span>
    </span>
  );
}

/** confirmDownload: every audited download says it is logged first. */
export function confirmDownload(e: React.MouseEvent | MouseEvent) {
  if (!window.confirm("此操作将被记录")) e.preventDefault();
}

/** clusterColumn: an audit row's cluster (its DevPod's), shown in a console of several. */
export function clusterColumn<T extends { cluster?: string }>(): ColumnDef<T, unknown> {
  return { id: "cluster", header: "集群", accessorFn: (r) => r.cluster ?? "", meta: { priority: 3, width: 90, mono: true } };
}

/** withCluster puts the 集群 column after 用户 when there are several clusters. */
export function withCluster<T extends { cluster?: string }>(cols: ColumnDef<T, unknown>[], several: boolean): ColumnDef<T, unknown>[] {
  if (!several) return cols;
  const at = cols.findIndex((c) => c.id === "user");
  return [...cols.slice(0, at + 1), clusterColumn<T>(), ...cols.slice(at + 1)];
}
