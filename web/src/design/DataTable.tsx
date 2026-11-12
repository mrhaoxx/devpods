// Dense full-width table (UI spec §3, §5.1): sorting, multi-select with a
// bulk bar, and columns hidden by priority when the table is narrow.
import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import {
  flexRender,
  getCoreRowModel,
  getSortedRowModel,
  useReactTable,
  type ColumnDef,
  type RowData,
  type RowSelectionState,
  type SortingState,
  type Updater,
} from "@tanstack/react-table";
import { EmptyState } from "./EmptyState";

declare module "@tanstack/react-table" {
  // eslint-disable-next-line @typescript-eslint/no-unused-vars
  interface ColumnMeta<TData extends RowData, TValue> {
    /** 1 = never hidden; higher numbers hide first. */
    priority?: number;
    /** approximate width in px, for the narrow-screen budget */
    width?: number;
    mono?: boolean;
    align?: "right";
  }
}

/** columnsForWidth picks the columns that fit, lowest priority number
 *  first; priority 1 always shows; width 0 (unknown) shows everything. */
export function columnsForWidth(width: number, metas: { id: string; priority: number; width?: number }[]): string[] {
  if (width <= 0) return metas.map((m) => m.id);
  const keep = new Set<string>();
  let used = 0;
  for (const m of [...metas].sort((a, b) => a.priority - b.priority)) {
    const w = m.width ?? 100;
    if (m.priority <= 1 || used + w <= width) {
      keep.add(m.id);
      used += w;
    }
  }
  return metas.filter((m) => keep.has(m.id)).map((m) => m.id);
}

function useWidth(ref: React.RefObject<HTMLElement>): number {
  const [w, setW] = useState(0);
  useEffect(() => {
    const el = ref.current;
    if (!el || typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(([e]) => setW(Math.floor(e.contentRect.width)));
    ro.observe(el);
    return () => ro.disconnect();
  }, [ref]);
  return w;
}

export type DataTableProps<T> = {
  data: T[];
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  columns: ColumnDef<T, any>[];
  getRowId(r: T): string;
  sorting: SortingState;
  onSortingChange(s: SortingState): void;
  selectable?: boolean;
  bulkBar?(rows: T[], clear: () => void): ReactNode;
  onRowClick?(r: T): void;
  empty?: ReactNode;
  /** dim: live data is stale (reconnecting); the last values stay */
  dim?: boolean;
  rowClass?(r: T): string;
  /** storageKey: remember column widths set by dragging, in this browser, under this name */
  storageKey?: string;
};

const MIN_COL = 48;
const sizesKey = (k: string) => `devpod.columns.${k}`;
function readSizes(k?: string): Record<string, number> {
  if (!k) return {};
  try {
    const v = JSON.parse(localStorage.getItem(sizesKey(k)) ?? "{}");
    return v && typeof v === "object" ? v : {};
  } catch {
    return {};
  }
}
function writeSizes(k: string | undefined, v: Record<string, number>) {
  if (!k) return;
  try {
    localStorage.setItem(sizesKey(k), JSON.stringify(v));
  } catch {
    // not kept: no matter
  }
}

export function DataTable<T>(p: DataTableProps<T>) {
  const wrap = useRef<HTMLDivElement>(null);
  const width = useWidth(wrap);
  const [showAll, setShowAll] = useState(false);
  const [selection, setSelection] = useState<RowSelectionState>({});
  // Each column is as wide as it says (or as it was dragged to), never as
  // its cells happen to be: live numbers changing do not move the columns.
  const [sizes, setSizes] = useState<Record<string, number>>(() => readSizes(p.storageKey));
  const sizesRef = useRef(sizes);
  sizesRef.current = sizes;

  const metas = useMemo(
    () => p.columns.map((c, i) => ({ id: c.id ?? String(i), priority: c.meta?.priority ?? 1, width: sizes[c.id ?? String(i)] ?? c.meta?.width ?? 120 })),
    [p.columns, sizes],
  );
  const widthOf = (id: string) => metas.find((m) => m.id === id)?.width ?? 120;
  const startResize = (id: string, e: React.MouseEvent) => {
    e.preventDefault();
    e.stopPropagation();
    const x0 = e.clientX;
    const w0 = widthOf(id);
    const move = (ev: MouseEvent) => setSizes((cur) => ({ ...cur, [id]: Math.max(MIN_COL, Math.round(w0 + ev.clientX - x0)) }));
    const up = (ev: MouseEvent) => {
      move(ev);
      document.removeEventListener("mousemove", move);
      document.removeEventListener("mouseup", up);
      writeSizes(p.storageKey, { ...sizesRef.current, [id]: Math.max(MIN_COL, Math.round(w0 + ev.clientX - x0)) });
    };
    document.addEventListener("mousemove", move);
    document.addEventListener("mouseup", up);
  };
  const resetSize = (id: string) => {
    const { [id]: _, ...rest } = sizesRef.current;
    setSizes(rest);
    writeSizes(p.storageKey, rest);
  };
  const visibleIds = showAll ? metas.map((m) => m.id) : columnsForWidth(width - (p.selectable ? 28 : 0), metas);
  const hidden = metas.length - visibleIds.length;
  const columnVisibility = Object.fromEntries(metas.map((m) => [m.id, visibleIds.includes(m.id)]));

  // drop selections whose rows are gone
  const ids = useMemo(() => new Set(p.data.map(p.getRowId)), [p.data, p.getRowId]);
  const liveSelection = useMemo(() => Object.fromEntries(Object.entries(selection).filter(([k, v]) => v && ids.has(k))), [selection, ids]);

  const table = useReactTable({
    data: p.data,
    columns: p.columns,
    getRowId: (r) => p.getRowId(r),
    state: { sorting: p.sorting, rowSelection: liveSelection, columnVisibility },
    onSortingChange: (u: Updater<SortingState>) => p.onSortingChange(typeof u === "function" ? u(p.sorting) : u),
    onRowSelectionChange: setSelection,
    enableRowSelection: !!p.selectable,
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: getSortedRowModel(),
  });
  // in the order the rows were ticked (bulk actions such as 迁移 pair
  // the selection with a list, one to one)
  const selected = useMemo(() => {
    const byId = new Map(p.data.map((r) => [p.getRowId(r), r]));
    return Object.keys(liveSelection).flatMap((id) => (byId.has(id) ? [byId.get(id)!] : []));
  }, [liveSelection, p.data, p.getRowId]);
  const clear = () => setSelection({});
  const rows = table.getRowModel().rows;
  const colCount = visibleIds.length + (p.selectable ? 1 : 0);
  const tableWidth = visibleIds.reduce((n, id) => n + widthOf(id), p.selectable ? 28 : 0);

  return (
    <div ref={wrap} className="min-w-0">
      {selected.length > 0 && (
        <div role="toolbar" aria-label="批量操作" className="flex flex-wrap items-center gap-2 border-b border-line bg-accent-soft px-3 py-1 text-xs">
          <span className="font-medium text-accent">已选 {selected.length}</span>
          {p.bulkBar?.(selected, clear)}
          <button type="button" onClick={clear} className="ml-auto text-muted hover:text-ink">
            取消选择
          </button>
        </div>
      )}
      {(hidden > 0 || showAll) && (
        <div className="flex justify-end px-2 py-0.5">
          <button type="button" onClick={() => setShowAll(!showAll)} className="text-[11px] text-muted hover:text-accent">
            {showAll ? "收起列" : `显示全部列（+${hidden}）`}
          </button>
        </div>
      )}
      <div className={showAll ? "overflow-x-auto" : "overflow-x-hidden"}>
        <table className="min-w-full border-collapse text-xs" style={{ tableLayout: "fixed", width: tableWidth }}>
          <thead className="sticky top-0 z-[1] bg-surface">
            {table.getHeaderGroups().map((hg) => (
              <tr key={hg.id} className="h-7 border-b border-line text-left text-[11px] text-muted">
                {p.selectable && (
                  <th className="px-2" style={{ width: 28 }}>
                    <input
                      type="checkbox"
                      aria-label="全选"
                      checked={table.getIsAllRowsSelected()}
                      ref={(el) => {
                        if (el) el.indeterminate = table.getIsSomeRowsSelected();
                      }}
                      onChange={table.getToggleAllRowsSelectedHandler()}
                    />
                  </th>
                )}
                {hg.headers.map((h) => {
                  const sortable = h.column.getCanSort();
                  const dir = h.column.getIsSorted();
                  const label = flexRender(h.column.columnDef.header, h.getContext());
                  return (
                    <th
                      key={h.id}
                      aria-sort={dir === "asc" ? "ascending" : dir === "desc" ? "descending" : undefined}
                      style={{ width: widthOf(h.column.id) }}
                      className={`relative truncate px-2 font-medium whitespace-nowrap ${h.column.columnDef.meta?.align === "right" ? "text-right" : ""}`}
                    >
                      {sortable ? (
                        <button type="button" onClick={h.column.getToggleSortingHandler()} className="inline-flex items-center gap-0.5 hover:text-ink">
                          {label}
                          <span aria-hidden="true" className="text-faint">
                            {dir === "asc" ? "↑" : dir === "desc" ? "↓" : ""}
                          </span>
                        </button>
                      ) : (
                        label
                      )}
                      <span
                        role="separator"
                        aria-orientation="vertical"
                        aria-label={`调整 ${typeof h.column.columnDef.header === "string" ? h.column.columnDef.header : h.column.id} 列宽`}
                        title="拖动调整列宽，双击恢复"
                        onMouseDown={(e) => startResize(h.column.id, e)}
                        onDoubleClick={() => resetSize(h.column.id)}
                        className="absolute inset-y-0 right-0 w-1.5 cursor-col-resize hover:bg-accent/40"
                      />
                    </th>
                  );
                })}
              </tr>
            ))}
          </thead>
          <tbody className={p.dim ? "opacity-60" : undefined}>
            {rows.length === 0 && (
              <tr>
                <td colSpan={colCount}>{p.empty ?? <EmptyState title="没有数据" />}</td>
              </tr>
            )}
            {rows.map((row) => (
              <tr
                key={row.id}
                aria-selected={p.selectable ? row.getIsSelected() : undefined}
                onClick={(e) => {
                  if (!p.onRowClick || (e.target as HTMLElement).closest("button,a,input,select,label")) return;
                  p.onRowClick(row.original);
                }}
                className={`h-7 border-b border-line ${row.getIsSelected() ? "bg-accent-soft/60" : "hover:bg-sunk"} ${
                  p.onRowClick ? "cursor-pointer" : ""
                } ${p.rowClass?.(row.original) ?? ""}`}
              >
                {p.selectable && (
                  <td className="px-2">
                    <input type="checkbox" aria-label={`选择 ${row.id}`} checked={row.getIsSelected()} onChange={row.getToggleSelectedHandler()} />
                  </td>
                )}
                {row.getVisibleCells().map((cell) => (
                  <td
                    key={cell.id}
                    className={`truncate px-2 whitespace-nowrap ${cell.column.columnDef.meta?.mono ? "mono" : ""} ${
                      cell.column.columnDef.meta?.align === "right" ? "text-right" : ""
                    }`}
                  >
                    {flexRender(cell.column.columnDef.cell, cell.getContext())}
                  </td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
