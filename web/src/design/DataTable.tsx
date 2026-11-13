// Dense full-width table (UI spec §3, §5.1): sorting, multi-select with a
// bulk bar floating at the bottom, columns of exact widths (dragged or by keyboard; the room they
// leave goes to a filler, never into them), a 列 menu to choose the columns,
// and sideways scrolling inside the table with the pinned columns staying.
// On a list page (fill) the table takes the rest of the page and scrolls
// itself: its header stays in view, and only the rows in sight (and a few
// around) are drawn — a thousand DevPods or nodes scroll as lightly as ten.
import { useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from "react";
import * as ContextMenu from "@radix-ui/react-context-menu";
import * as Menu from "@radix-ui/react-dropdown-menu";
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
    /** how much the column is wanted: on a phone only 1 and 2 show by default */
    priority?: number;
    /** width in px (until dragged) */
    width?: number;
    mono?: boolean;
    align?: "right";
    /** stays in place when the table scrolls sideways, and is always shown */
    pin?: "left" | "right";
  }
}

/** phoneHidden: the columns a phone-wide table leaves out until chosen —
 *  every one past priority 2; none on a wider screen (or before the width
 *  is known). */
export function phoneHidden(width: number, metas: { id: string; priority: number }[]): string[] {
  if (width <= 0 || width >= PHONE) return [];
  return metas.filter((m) => m.priority > 2).map((m) => m.id);
}
const PHONE = 640;

/** useView: where a scroller is scrolled to and how tall it is, read once
 *  a frame while it scrolls or changes size. Height -1: not read yet;
 *  0: read, and there is no layout to go by. */
export function useView(ref: React.RefObject<HTMLElement>, on: boolean): { top: number; height: number } {
  const [view, setView] = useState({ top: 0, height: -1 });
  // before the first paint: the first draw is never of every row
  useLayoutEffect(() => {
    const el = ref.current;
    if (!on || !el) return;
    let frame = 0;
    const read = () => {
      frame = 0;
      setView((v) => (v.top === el.scrollTop && v.height === el.clientHeight ? v : { top: el.scrollTop, height: el.clientHeight }));
    };
    const soon = () => {
      if (!frame) frame = requestAnimationFrame(read);
    };
    read();
    el.addEventListener("scroll", soon, { passive: true });
    const ro = typeof ResizeObserver === "undefined" ? undefined : new ResizeObserver(soon);
    ro?.observe(el);
    return () => {
      el.removeEventListener("scroll", soon);
      ro?.disconnect();
      if (frame) cancelAnimationFrame(frame);
    };
  }, [ref, on]);
  return view;
}

/** rowWindow: the rows to draw of n, each h tall under a header head tall,
 *  in a scroller at top showing height — those in sight and over rows
 *  either side. Before the height is read: a first screenful (a thousand
 *  rows are not drawn to be thrown away); with no layout at all: all. */
export function rowWindow(n: number, view: { top: number; height: number }, h: number, head: number, over = 12): { first: number; last: number } {
  if (view.height < 0) return { first: 0, last: Math.min(n, FIRST_DRAW) };
  if (view.height === 0 || h <= 0) return { first: 0, last: n };
  const first = Math.max(0, Math.floor(view.top / h) - over);
  const last = Math.min(n, Math.ceil((view.top + view.height - head) / h) + over);
  return { first: Math.min(first, last), last };
}

const FIRST_DRAW = 60;

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

/** RowAction: one thing a row menu (or a bulk bar) offers. */
export type RowAction = { label: string; onSelect(): void; danger?: boolean; disabled?: boolean; title?: string };

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
  /** rowMenu: what a right click on a row offers — for that row, or, on a
   *  row of a selection of several, for the selection (bulk). Nothing
   *  offered (or Shift held): the browser's own menu. */
  rowMenu?(rows: T[], bulk: boolean, clear: () => void): RowAction[];
  /** sorted: data comes in the order to show (the caller sorts, say to
   *  hold an order while values change); the headers still say and ask */
  sorted?: boolean;
  /** fill: a list page's table — it takes the rest of its flex column and
   *  scrolls itself, the header staying, drawing only the rows in sight */
  fill?: boolean;
};

const MIN_COL = 48;
const CHECK = 28; // the selection column
const BAR_ROOM = 64; // under the last row while the bulk bar floats
// where a list was scrolled to, per history entry: going back to it finds the place
const scrollKey = (k: string) => `devpod.scroll.${k}`;
const entry = () => String((window.history.state as { key?: string } | null)?.key ?? "");
const isScrolls = (v: unknown): v is [string, number][] => Array.isArray(v);
function readScroll(k: string | undefined): number {
  if (!k) return 0;
  try {
    const v: unknown = JSON.parse(sessionStorage.getItem(scrollKey(k)) ?? "null");
    return (isScrolls(v) ? v.find(([e]) => e === entry())?.[1] : 0) ?? 0;
  } catch {
    return 0;
  }
}
function writeScroll(k: string | undefined, top: number) {
  if (!k) return;
  try {
    const v: unknown = JSON.parse(sessionStorage.getItem(scrollKey(k)) ?? "null");
    const rest = (isScrolls(v) ? v : []).filter(([e]) => e !== entry());
    sessionStorage.setItem(scrollKey(k), JSON.stringify([[entry(), top], ...rest].slice(0, 30)));
  } catch {
    // not kept: no matter
  }
}
const ROW_H = 29; // a row's height (h-7 and its border) until one is measured
const sizesKey = (k: string) => `devpod.columns.${k}`;
const hiddenKey = (k: string) => `devpod.columns-hidden.${k}`;
function readJSON<T>(key: string | undefined, ok: (v: unknown) => v is T): T | undefined {
  if (!key) return undefined;
  try {
    const v: unknown = JSON.parse(localStorage.getItem(key) ?? "null");
    return ok(v) ? v : undefined;
  } catch {
    return undefined;
  }
}
function writeJSON(key: string | undefined, v: unknown) {
  if (!key) return;
  try {
    if (v === undefined) localStorage.removeItem(key);
    else localStorage.setItem(key, JSON.stringify(v));
  } catch {
    // not kept: no matter
  }
}
const isSizes = (v: unknown): v is Record<string, number> => !!v && typeof v === "object" && !Array.isArray(v);
const isIds = (v: unknown): v is string[] => Array.isArray(v) && v.every((x) => typeof x === "string");
// a pinned cell covers what scrolls under it: a background of its own, as its row's
const pinnedBg = (selected: boolean) =>
  selected ? "bg-[color-mix(in_srgb,var(--color-accent-soft)_60%,var(--color-surface))]" : "bg-surface group-hover:bg-sunk";

export function DataTable<T>(p: DataTableProps<T>) {
  const wrap = useRef<HTMLDivElement>(null);
  const width = useWidth(wrap);
  const scroller = useRef<HTMLDivElement>(null);
  const head = useRef<HTMLTableSectionElement>(null);
  const view = useView(scroller, !!p.fill);
  const [rowH, setRowH] = useState(ROW_H);
  const [selection, setSelection] = useState<RowSelectionState>({});
  // Each column is exactly as wide as it says (or as it was dragged to),
  // never as its cells happen to be, nor stretched to fill the row.
  const [sizes, setSizes] = useState<Record<string, number>>(() => readJSON(p.storageKey && sizesKey(p.storageKey), isSizes) ?? {});
  const sizesRef = useRef(sizes);
  sizesRef.current = sizes;
  // the columns hidden by choice (undefined: none chosen — the default)
  const [chosen, setChosen] = useState<string[] | undefined>(() => readJSON(p.storageKey && hiddenKey(p.storageKey), isIds));

  const metas = useMemo(
    () =>
      p.columns.map((c, i) => {
        const id = c.id ?? String(i);
        const label = typeof c.header === "string" ? c.header : "";
        return { id, label, priority: c.meta?.priority ?? 1, pin: c.meta?.pin, width: sizes[id] ?? c.meta?.width ?? 120 };
      }),
    [p.columns, sizes],
  );
  const widthOf = (id: string) => metas.find((m) => m.id === id)?.width ?? 120;
  const setWidth = (id: string, w: number, save: boolean) => {
    const next = { ...sizesRef.current, [id]: Math.max(MIN_COL, Math.round(w)) };
    setSizes(next);
    if (save) writeJSON(p.storageKey && sizesKey(p.storageKey), next);
  };
  // by pointer — a mouse, a finger, a pen alike (mouse events never come
  // from a touch, so a phone could not drag an edge)
  const startResize = (id: string, e: React.PointerEvent) => {
    if (e.pointerType === "mouse" && e.button !== 0) return;
    e.preventDefault();
    e.stopPropagation();
    const x0 = e.clientX;
    const w0 = widthOf(id);
    const pointer = e.pointerId;
    document.body.classList.add("col-resizing");
    const move = (ev: PointerEvent) => {
      if (ev.pointerId === pointer) setWidth(id, w0 + ev.clientX - x0, false);
    };
    const end = (ev: PointerEvent) => {
      if (ev.pointerId !== pointer) return;
      // a cancelled touch (the browser took it) keeps the width it had reached
      setWidth(id, ev.type === "pointercancel" ? (sizesRef.current[id] ?? w0) : w0 + ev.clientX - x0, true);
      document.body.classList.remove("col-resizing");
      document.removeEventListener("pointermove", move);
      document.removeEventListener("pointerup", end);
      document.removeEventListener("pointercancel", end);
    };
    document.addEventListener("pointermove", move);
    document.addEventListener("pointerup", end);
    document.addEventListener("pointercancel", end);
  };
  const keyResize = (id: string, e: React.KeyboardEvent) => {
    const step = e.shiftKey ? 32 : 8;
    if (e.key === "ArrowRight" || e.key === "ArrowLeft") {
      e.preventDefault();
      setWidth(id, widthOf(id) + (e.key === "ArrowRight" ? step : -step), true);
    }
  };
  const resetSize = (id: string) => {
    const { [id]: _, ...rest } = sizesRef.current;
    setSizes(rest);
    writeJSON(p.storageKey && sizesKey(p.storageKey), rest);
  };

  const hiddenIds = new Set((chosen ?? phoneHidden(width, metas)).filter((id) => !metas.find((m) => m.id === id)?.pin));
  const choosable = metas.filter((m) => !m.pin && m.label);
  const choose = (next: string[] | undefined) => {
    setChosen(next);
    writeJSON(p.storageKey && hiddenKey(p.storageKey), next);
  };
  const toggle = (id: string) => {
    const now = [...hiddenIds];
    choose(hiddenIds.has(id) ? now.filter((x) => x !== id) : [...now, id]);
  };
  const columnVisibility = Object.fromEntries(metas.map((m) => [m.id, !hiddenIds.has(m.id)]));
  const shown = metas.filter((m) => !hiddenIds.has(m.id));

  // where pinned columns sit: from the left after the checkbox, from the right
  const offset: Record<string, { left?: number; right?: number }> = {};
  let left = p.selectable ? CHECK : 0;
  for (const m of shown.filter((m) => m.pin === "left")) {
    offset[m.id] = { left };
    left += m.width;
  }
  let right = 0;
  const rightPinned = shown.filter((m) => m.pin === "right");
  for (const m of [...rightPinned].reverse()) {
    offset[m.id] = { right };
    right += m.width;
  }
  // the 列 menu sits in the last pinned-right header, or a cell of its own
  const menuHost = rightPinned.at(-1)?.id;
  const fillerBefore = rightPinned[0]?.id; // undefined: at the end
  const sticky = (id: string): React.CSSProperties | undefined => (offset[id] ? { position: "sticky", ...offset[id], zIndex: 1 } : undefined);

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
    getSortedRowModel: p.sorted ? undefined : getSortedRowModel(),
    manualSorting: !!p.sorted,
  });
  // in the order the rows were ticked (bulk actions such as 迁移 pair
  // the selection with a list, one to one)
  const selected = useMemo(() => {
    const byId = new Map(p.data.map((r) => [p.getRowId(r), r]));
    return Object.keys(liveSelection).flatMap((id) => (byId.has(id) ? [byId.get(id)!] : []));
  }, [liveSelection, p.data, p.getRowId]);
  const clear = () => setSelection({});
  // the right-click menu: worked out as it opens, for the row under the
  // pointer — or the selection, when that row is one of several chosen
  const [menuFor, setMenuFor] = useState<{ bulk: number; actions: RowAction[] } | null>(null);
  const onContext = (e: React.MouseEvent) => {
    const id = (e.target as HTMLElement).closest("tr[data-row]")?.getAttribute("data-row");
    const row = id == null ? undefined : p.data.find((r) => p.getRowId(r) === id);
    const bulk = row !== undefined && !!liveSelection[id!] && selected.length > 1;
    const actions = row === undefined || e.shiftKey ? [] : (p.rowMenu?.(bulk ? selected : [row], bulk, clear) ?? []);
    if (actions.length === 0) {
      e.stopPropagation(); // the menu does not open; the browser's does
      return;
    }
    setMenuFor({ bulk: bulk ? selected.length : 0, actions });
  };
  // Esc lets go of the rows — unless it is closing a dialog or a menu
  const any = selected.length > 0;
  useEffect(() => {
    if (!any) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape" && !e.defaultPrevented && !document.querySelector('[role="dialog"],[role="menu"],[role="alertdialog"]')) setSelection({});
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [any]);
  const rows = table.getRowModel().rows;
  const colCount = shown.length + (p.selectable ? 1 : 0) + 1 + (menuHost ? 0 : 1);
  // the rows drawn: those in sight on a list page; the others are room
  const { first, last } = p.fill ? rowWindow(rows.length, view, rowH, head.current?.offsetHeight ?? ROW_H) : { first: 0, last: rows.length };
  const drawn = first === 0 && last === rows.length ? rows : rows.slice(first, last);
  const room = (n: number, key: string) =>
    n > 0 && (
      <tr key={key} aria-hidden="true" style={{ height: n * rowH }}>
        <td colSpan={colCount} style={{ padding: 0, border: 0 }} />
      </tr>
    );
  // back at a history entry (the browser's back, a mouse's side button):
  // scrolled to where it was, once there are rows to scroll to
  const want = useRef<number | undefined>(undefined);
  if (want.current === undefined) want.current = p.fill ? readScroll(p.storageKey) : 0;
  // (tried once, with the first rows: later the scroll is the reader's)
  useLayoutEffect(() => {
    const el = scroller.current;
    if (!el || !want.current || rows.length === 0) return;
    el.scrollTop = want.current;
    want.current = 0;
  });
  useEffect(() => {
    if (!p.fill || want.current) return;
    const t = setTimeout(() => writeScroll(p.storageKey, view.top), 250); // once the scrolling pauses
    return () => clearTimeout(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [view.top]);
  // a row as tall as it is drawn (zoom, fonts): measured, not assumed
  useLayoutEffect(() => {
    if (!p.fill) return;
    const h = scroller.current?.querySelector("tbody tr[data-row]")?.getBoundingClientRect().height ?? 0;
    if (h > 0 && Math.abs(h - rowH) > 0.25) setRowH(h);
  });

  const menu = choosable.length > 0 && (
    <Menu.Root modal={false}>
      <Menu.Trigger aria-label="选择列" title="选择显示哪些列" className="rounded px-1 font-normal text-muted hover:bg-sunk hover:text-ink">
        列
      </Menu.Trigger>
      <Menu.Portal>
        <Menu.Content align="end" className="z-50 max-h-96 min-w-40 overflow-y-auto rounded-md border border-line bg-surface p-1 text-xs shadow-lg">
          {choosable.map((m) => (
            <Menu.CheckboxItem
              key={m.id}
              checked={!hiddenIds.has(m.id)}
              onCheckedChange={() => toggle(m.id)}
              onSelect={(e) => e.preventDefault()}
              className="flex cursor-pointer items-center gap-2 rounded px-2 py-1 outline-none data-[highlighted]:bg-sunk"
            >
              <span aria-hidden="true" className="w-3 text-accent">{hiddenIds.has(m.id) ? "" : "✓"}</span>
              {m.label}
            </Menu.CheckboxItem>
          ))}
          <Menu.Separator className="my-1 h-px bg-line" />
          <Menu.Item onSelect={() => choose(undefined)} className="cursor-pointer rounded px-2 py-1 text-muted outline-none data-[highlighted]:bg-sunk">
            恢复默认
          </Menu.Item>
        </Menu.Content>
      </Menu.Portal>
    </Menu.Root>
  );
  const filler = (kind: "th" | "td") =>
    kind === "th" ? (
      <th key="filler" data-filler aria-hidden="true" className="bg-surface shadow-[inset_0_-1px_0_var(--color-line)]" />
    ) : (
      <td key="filler" data-filler aria-hidden="true" />
    );

  return (
    // while rows are chosen the bar floats over the window's bottom: room is
    // kept under the last row so it hides nothing, and nothing above moves
    <div
      ref={wrap}
      className={p.fill ? "flex min-h-0 min-w-0 flex-1 flex-col" : "min-w-0"}
      style={selected.length > 0 && !p.fill ? { paddingBottom: BAR_ROOM } : undefined}
    >
      <div
        ref={scroller}
        data-table-scroller={p.fill ? "" : undefined}
        className={p.fill ? "min-h-0 flex-1 overflow-auto" : "overflow-x-auto"}
        style={selected.length > 0 && p.fill ? { paddingBottom: BAR_ROOM } : undefined}
      >
        <table className="border-collapse text-xs" style={{ tableLayout: "fixed", width: "100%" }}>
          <thead ref={head} className="sticky top-0 z-[2] bg-surface">
            {table.getHeaderGroups().map((hg) => (
              <tr key={hg.id} className="h-7 border-b border-line text-left text-[11px] text-muted">
                {p.selectable && (
                  <th className="bg-surface px-2 shadow-[inset_0_-1px_0_var(--color-line)]" style={{ width: CHECK, position: "sticky", left: 0, zIndex: 1 }}>
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
                {hg.headers.flatMap((h) => {
                  const id = h.column.id;
                  const sortable = h.column.getCanSort();
                  const dir = h.column.getIsSorted();
                  const label = flexRender(h.column.columnDef.header, h.getContext());
                  const name = typeof h.column.columnDef.header === "string" ? h.column.columnDef.header : id;
                  const right = h.column.columnDef.meta?.align === "right";
                  const cell = (
                    <th
                      key={h.id}
                      aria-sort={dir === "asc" ? "ascending" : dir === "desc" ? "descending" : undefined}
                      style={{ width: widthOf(id), ...sticky(id) }}
                      className={`group/h relative bg-surface px-2 font-medium whitespace-nowrap shadow-[inset_0_-1px_0_var(--color-line)] ${right ? "text-right" : ""}`}
                    >
                      <span className="flex min-w-0 items-center gap-1">
                        {sortable ? (
                          <button type="button" onClick={h.column.getToggleSortingHandler()} className={`inline-flex min-w-0 items-center gap-0.5 hover:text-ink ${right ? "ml-auto" : ""}`}>
                            <span className="truncate">{label}</span>
                            <span aria-hidden="true" className={dir ? "text-accent" : "invisible text-faint group-hover/h:visible"}>
                              {dir === "asc" ? "↑" : dir === "desc" ? "↓" : "↕"}
                            </span>
                          </button>
                        ) : (
                          <span className="truncate">{label}</span>
                        )}
                        {id === menuHost && <span className="ml-auto">{menu}</span>}
                      </span>
                      {/* the edge: a line between headers, grabbed across its middle */}
                      <span
                        role="separator"
                        aria-orientation="vertical"
                        aria-label={`调整 ${name} 列宽`}
                        aria-valuenow={widthOf(id)}
                        tabIndex={0}
                        title="拖动或用 ←/→ 调整列宽，双击恢复"
                        onPointerDown={(e) => startResize(id, e)}
                        onDoubleClick={() => resetSize(id)}
                        onKeyDown={(e) => keyResize(id, e)}
                        className="absolute inset-y-1 -right-1 z-[3] w-2 cursor-col-resize touch-none outline-none [@media(pointer:coarse)]:-right-2.5 [@media(pointer:coarse)]:w-5 after:absolute after:inset-y-0 after:left-1/2 after:w-px after:bg-line hover:after:bg-accent focus-visible:after:bg-accent"
                      />
                    </th>
                  );
                  return id === fillerBefore ? [filler("th"), cell] : [cell];
                })}
                {!fillerBefore && filler("th")}
                {!menuHost && (
                  <th className="bg-surface px-1 text-right shadow-[inset_0_-1px_0_var(--color-line)]" style={{ width: 32, position: "sticky", right: 0, zIndex: 1 }}>
                    {menu}
                  </th>
                )}
              </tr>
            ))}
          </thead>
          <Body menu={p.rowMenu ? { onContext, menuFor, close: () => setMenuFor(null) } : undefined} className={p.dim ? "opacity-60" : undefined}>
            {rows.length === 0 && (
              <tr>
                <td colSpan={colCount}>{p.empty ?? <EmptyState title="没有数据" />}</td>
              </tr>
            )}
            {room(first, "room-above")}
            {drawn.map((row) => {
              const sel = row.getIsSelected();
              return (
                <tr
                  key={row.id}
                  data-row={row.id}
                  aria-selected={p.selectable ? sel : undefined}
                  onClick={(e) => {
                    if ((e.target as HTMLElement).closest("button,a,input,select,label,[role='menu']")) return;
                    if (p.onRowClick) return p.onRowClick(row.original);
                    // a selectable row with nothing else to do on a click: the click picks it
                    // (anywhere on it, not only the box) — unless text was being selected
                    if (p.selectable && !window.getSelection()?.toString()) row.toggleSelected();
                  }}
                  className={`group h-7 border-b border-line ${sel ? "bg-accent-soft/60" : "hover:bg-sunk"} ${p.onRowClick ? "cursor-pointer" : ""} ${p.rowClass?.(row.original) ?? ""}`}
                >
                  {p.selectable && (
                    <td className={`px-2 ${pinnedBg(sel)}`} style={{ position: "sticky", left: 0, zIndex: 1 }}>
                      <input type="checkbox" aria-label={`选择 ${row.id}`} checked={sel} onChange={row.getToggleSelectedHandler()} />
                    </td>
                  )}
                  {row.getVisibleCells().flatMap((cell) => {
                    const id = cell.column.id;
                    const td = (
                      <td
                        key={cell.id}
                        style={sticky(id)}
                        className={`truncate px-2 whitespace-nowrap ${offset[id] ? pinnedBg(sel) : ""} ${cell.column.columnDef.meta?.mono ? "mono" : ""} ${
                          cell.column.columnDef.meta?.align === "right" ? "text-right" : ""
                        }`}
                      >
                        {flexRender(cell.column.columnDef.cell, cell.getContext())}
                      </td>
                    );
                    return id === fillerBefore ? [filler("td"), td] : [td];
                  })}
                  {!fillerBefore && filler("td")}
                  {!menuHost && <td className={pinnedBg(sel)} style={{ position: "sticky", right: 0, zIndex: 1 }} />}
                </tr>
              );
            })}
            {room(rows.length - last, "room-below")}
          </Body>
        </table>
      </div>
      {selected.length > 0 && (
        <div className="pointer-events-none fixed inset-x-0 bottom-4 z-40 flex justify-center px-3">
          <div
            role="toolbar"
            aria-label="批量操作"
            className="pointer-events-auto flex max-w-full flex-wrap items-center gap-2 rounded-lg border border-line bg-surface px-3 py-1.5 text-xs shadow-lg"
          >
            <span className="font-medium text-accent">已选 {selected.length}</span>
            {p.bulkBar?.(selected, clear)}
            <button type="button" onClick={clear} title="Esc" className="ml-2 text-muted hover:text-ink">
              取消选择
            </button>
          </div>
        </div>
      )}
    </div>
  );
}

/** Body: the rows, and — when they have a menu — what a right click opens. */
function Body({
  menu,
  className,
  children,
}: {
  menu?: { onContext(e: React.MouseEvent): void; menuFor: { bulk: number; actions: RowAction[] } | null; close(): void };
  className?: string;
  children: ReactNode;
}) {
  if (!menu) return <tbody className={className}>{children}</tbody>;
  const item = "cursor-pointer rounded px-2 py-1 outline-none data-[disabled]:cursor-default data-[disabled]:opacity-50";
  return (
    <ContextMenu.Root modal={false} onOpenChange={(open) => !open && menu.close()}>
      <ContextMenu.Trigger asChild>
        {/* in the capture phase, before the menu's own handler: no row, nothing offered or Shift → the browser's menu */}
        <tbody className={className} onContextMenuCapture={menu.onContext}>
          {children}
        </tbody>
      </ContextMenu.Trigger>
      <ContextMenu.Portal>
        <ContextMenu.Content className="z-50 min-w-40 rounded-md border border-line bg-surface p-1 text-xs shadow-lg">
          {menu.menuFor && menu.menuFor.bulk > 0 && (
            <ContextMenu.Label className="px-2 py-1 font-medium text-accent">已选 {menu.menuFor.bulk}</ContextMenu.Label>
          )}
          {menu.menuFor?.actions.map((a) => (
            <ContextMenu.Item
              key={a.label}
              disabled={a.disabled}
              title={a.title}
              onSelect={a.onSelect}
              className={`${item} ${a.danger ? "text-fail data-[highlighted]:bg-fail-soft" : "data-[highlighted]:bg-sunk"}`}
            >
              {a.label}
            </ContextMenu.Item>
          ))}
        </ContextMenu.Content>
      </ContextMenu.Portal>
    </ContextMenu.Root>
  );
}
