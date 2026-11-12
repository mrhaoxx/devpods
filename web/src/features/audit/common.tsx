// Shared bits of the audit pages: the time range in the URL, paged
// queries, the error state when ClickHouse is unavailable.
import { useMemo } from "react";
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
  return <Input aria-label={label} placeholder={label} value={value} onChange={(e) => onChange(e.target.value)} className={`mono !h-7 ${width} text-xs`} />;
}

/** usePaged pages through a cursor API; the filters form the query key. */
export function usePaged<T>(key: string, filters: Record<string, string>, fetch: (q: Record<string, string | undefined>) => Promise<Page<T>>) {
  const q = useInfiniteQuery({
    queryKey: ["audit", key, filters],
    queryFn: ({ pageParam }) => fetch({ ...filters, cursor: pageParam || undefined }),
    initialPageParam: "",
    getNextPageParam: (last) => last.next || undefined,
    retry: false,
  });
  const items = useMemo(() => q.data?.pages.flatMap((p) => p.items ?? []) ?? [], [q.data]);
  return { ...q, items };
}

export function AuditError({ error }: { error: unknown }) {
  if (error instanceof ApiFailure && error.code === "AMBIGUOUS_CLUSTER")
    return <EmptyState title="多个集群都有这个名字的 DevPod" hint="在顶栏的集群筛选里选一个集群，再按 DevPod 查。" />;
  return <EmptyState tone="error" title="审计数据暂不可用" hint={error instanceof ApiFailure ? error.body.message : String(error)} />;
}

export function LoadMore({ hasMore, busy, onClick }: { hasMore?: boolean; busy?: boolean; onClick: () => void }) {
  if (!hasMore) return null;
  return (
    <div className="p-2 text-center">
      <Button variant="ghost" disabled={busy} onClick={onClick}>
        {busy ? "加载中…" : "加载更多"}
      </Button>
    </div>
  );
}

export const fmtTS = (iso: string) => new Date(iso).toLocaleString();

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
