// Picking nodes when there are many: a list to search and filter, with
// each node's free GPUs and state; picked in order — the order numbers
// the DevPods (INDEX) or pairs them with the nodes of a migration. A
// node someone else holds now cannot be picked. single: one node. A drag
// across rows picks them in the order passed over (from a picked row it
// unpicks them); the filters are remembered in this browser.
import { keyOf } from "../../data/cluster";
import { useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { rowWindow, useView } from "../../design/DataTable";
import type { NodeChoice } from "../../data/api";
import { useNodeChoices } from "../../data/queries";
import { fmtCores } from "../../lib/format";
import { fmtTime } from "../../lib/reservations";

const gpuName = (m?: string) => (m ?? "GPU").replace(/^NVIDIA-/, "");

type Filters = { onlyUsable: boolean; onlyGpu: boolean };
const FILTERS = "devpod.nodeList.filters";
function readFilters(): Filters {
  try {
    const v: Partial<Filters> = JSON.parse(localStorage.getItem(FILTERS) ?? "{}");
    return { onlyUsable: v.onlyUsable === true, onlyGpu: v.onlyGpu === true };
  } catch {
    return { onlyUsable: false, onlyGpu: false };
  }
}
function saveFilters(f: Filters) {
  try {
    localStorage.setItem(FILTERS, JSON.stringify(f));
  } catch {
    // private window, storage off: not remembered
  }
}
const usable = (n: NodeChoice) => n.selectable && n.ready;

function state(n: NodeChoice): string | null {
  if (n.access === "denied") return "无权限";
  if (n.access === "reservation") return "需预约";
  if (!n.ready) return "未就绪";
  if (n.mine) return `你的预约至 ${fmtTime(n.mine.until)}`;
  return null;
}

export function NodeList({
  cluster,
  picked,
  setPicked,
  disabled,
  legend = "目标节点（按勾选顺序）",
  max = 32,
  single = false,
}: {
  /** whose nodes: one cluster's (all of them without) */
  cluster?: string;
  picked: string[];
  setPicked: (n: string[]) => void;
  disabled?: boolean;
  legend?: string;
  max?: number;
  single?: boolean;
}) {
  const all = useNodeChoices().data?.items;
  const nodes = useMemo(() => (all ?? []).filter((n) => !cluster || n.cluster === cluster), [all, cluster]);
  const [q, setQ] = useState("");
  const [filters, setFilters] = useState(readFilters);
  const { onlyUsable, onlyGpu } = filters;
  const setFilter = (p: Partial<Filters>) => {
    const f = { ...filters, ...p };
    setFilters(f);
    saveFilters(f);
  };
  const shown = useMemo(() => {
    const s = q.trim().toLowerCase();
    return nodes.filter(
      (n) =>
        (!s || n.name.toLowerCase().includes(s) || (n.gpuModel ?? "").toLowerCase().includes(s)) &&
        (!onlyUsable || usable(n)) &&
        (!onlyGpu || n.gpuTotal - n.gpuUsed > 0),
    );
  }, [nodes, q, onlyUsable, onlyGpu]);
  const full = !single && picked.length >= max;
  // a cluster of a thousand nodes: only the ones in sight are drawn
  const list = useRef<HTMLUListElement>(null);
  const view = useView(list, true);
  const [rowH, setRowH] = useState(29);
  const { first, last } = rowWindow(shown.length, view, rowH, 0);
  // a row as tall as it is drawn: the shortest drawn, measured when rows
  // first come and when the window's size or zoom changes — never on a
  // scroll, where rows of differing heights would make it go back and
  // forth (React #185), as in DataTable
  const [resized, setResized] = useState(0);
  useEffect(() => {
    const on = () => setResized((n) => n + 1);
    window.addEventListener("resize", on);
    return () => window.removeEventListener("resize", on);
  }, []);
  const anyRows = shown.length > 0;
  useLayoutEffect(() => {
    if (!anyRows) return;
    let h = Infinity;
    list.current?.querySelectorAll("li[data-node]").forEach((li) => {
      const x = li.getBoundingClientRect().height;
      if (x > 0) h = Math.min(h, x);
    });
    if (h !== Infinity && Math.abs(h - rowH) > 0.25) setRowH(h);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [anyRows, resized]);

  const toggle = (name: string) => {
    if (single) return setPicked([name]);
    if (picked.includes(name)) setPicked(picked.filter((x) => x !== name));
    else if (!full) setPicked([...picked, name]);
  };
  // a drag across rows: from the row pressed to the one under the pointer,
  // in that order; from a picked row, those passed over are unpicked. A
  // press that stays on its row is a click. Near the list's top or bottom
  // edge the list scrolls.
  const pickedNow = useRef(picked);
  pickedNow.current = picked;
  const shownNow = useRef(shown);
  shownNow.current = shown;
  const drag = useRef<{ from: string; base: string[]; on: boolean; moved: boolean } | null>(null);
  const afterDrag = useRef(false); // the click that ends a drag picks nothing more
  const [dragging, setDragging] = useState(false);
  const startDrag = (e: React.MouseEvent) => {
    afterDrag.current = false;
    if (single || disabled || e.button !== 0 || e.shiftKey || e.metaKey || e.ctrlKey || e.altKey) return;
    const from = (e.target as HTMLElement).closest<HTMLElement>("li[data-name]")?.dataset.name;
    const at = shownNow.current.find((n) => n.name === from);
    if (!from || !at?.selectable) return;
    drag.current = { from, base: pickedNow.current, on: !pickedNow.current.includes(from), moved: false };
    let x = e.clientX;
    let y = e.clientY;
    let raf = 0;
    const pick = () => {
      const d = drag.current;
      if (!d) return;
      // over the rows, even when the pointer has gone above or below them
      let py = y;
      const r = list.current?.getBoundingClientRect();
      if (r && r.height > 0) py = Math.min(Math.max(y, r.top + 1), r.bottom - 2);
      const id = (typeof document.elementFromPoint === "function" ? document.elementFromPoint(x, py) : null)?.closest<HTMLElement>("li[data-name]")?.dataset.name;
      const all = shownNow.current;
      const i = all.findIndex((n) => n.name === id);
      const j = all.findIndex((n) => n.name === d.from);
      if (i < 0 || j < 0 || (!d.moved && i === j)) return;
      if (!d.moved) {
        d.moved = true;
        setDragging(true);
        window.getSelection()?.removeAllRanges();
      }
      const step = i >= j ? 1 : -1;
      const passed: string[] = [];
      for (let k = j; k !== i + step; k += step) if (all[k].selectable) passed.push(all[k].name);
      if (d.on) {
        const more = passed.filter((n) => !d.base.includes(n));
        setPicked([...d.base, ...more].slice(0, Math.max(max, d.base.length)));
      } else setPicked(d.base.filter((n) => !passed.includes(n)));
    };
    const edge = () => {
      const ul = list.current;
      if (ul && drag.current?.moved) {
        const r = ul.getBoundingClientRect();
        const dy = y < r.top + 16 ? -Math.ceil((r.top + 16 - y) / 2) : y > r.bottom - 16 ? Math.ceil((y - r.bottom + 16) / 2) : 0;
        if (dy && r.height > 0) {
          ul.scrollTop += Math.max(-30, Math.min(30, dy));
          pick();
        }
      }
      raf = requestAnimationFrame(edge);
    };
    raf = requestAnimationFrame(edge);
    const move = (ev: MouseEvent) => {
      x = ev.clientX;
      y = ev.clientY;
      pick();
    };
    const end = () => {
      cancelAnimationFrame(raf);
      document.removeEventListener("mousemove", move);
      document.removeEventListener("mouseup", end);
      if (drag.current?.moved) {
        afterDrag.current = true;
        setTimeout(() => (afterDrag.current = false), 0);
      }
      drag.current = null;
      setDragging(false);
    };
    document.addEventListener("mousemove", move);
    document.addEventListener("mouseup", end);
  };

  const addShown = () => {
    const more = shown.filter((n) => n.selectable && !picked.includes(n.name)).map((n) => n.name);
    setPicked([...picked, ...more].slice(0, Math.max(max, picked.length)));
  };

  return (
    <fieldset disabled={disabled} className={`space-y-1.5 ${disabled ? "opacity-40" : ""}`}>
      <legend className="mb-1 flex w-full items-baseline justify-between font-medium">
        <span>{legend}</span>
        {!single && <span className="text-[11px] font-normal text-muted">已选 {picked.length} 个</span>}
      </legend>
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        <input
          aria-label="搜索节点"
          placeholder="搜索节点或 GPU 型号…"
          value={q}
          onChange={(e) => setQ(e.target.value)}
          className="h-7 min-w-0 flex-1 rounded-md border border-line-strong bg-surface px-2 text-xs focus:border-accent focus:outline-none"
        />
        <label className="inline-flex items-center gap-1 text-[11px]">
          <input type="checkbox" checked={onlyUsable} onChange={(e) => setFilter({ onlyUsable: e.target.checked })} />
          只看可用
        </label>
        <label className="inline-flex items-center gap-1 text-[11px]">
          <input type="checkbox" checked={onlyGpu} onChange={(e) => setFilter({ onlyGpu: e.target.checked })} />
          只看有空闲 GPU
        </label>
        {!single && (
          <button type="button" onClick={addShown} disabled={full} className="text-[11px] text-accent hover:underline disabled:text-faint disabled:no-underline">
            全选筛选结果
          </button>
        )}
      </div>
      {!single && picked.length > 0 && (
        <ol aria-label="已选节点" className="flex flex-wrap gap-1">
          {picked.map((name, i) => (
            <li key={name} className="mono inline-flex items-center gap-1 rounded-md border border-accent bg-accent-soft px-1.5 text-[11px] text-accent">
              <span className="rounded bg-accent px-1 text-[10px] text-white">{i}</span> {name}
              <button type="button" aria-label={`取消 ${name}`} onClick={() => toggle(name)} className="text-muted hover:text-fail">
                ×
              </button>
            </li>
          ))}
        </ol>
      )}
      {full && <p role="status" className="text-[11px] text-warm">最多选 {max} 个。</p>}
      <ul
        ref={list}
        className={`max-h-72 overflow-y-auto rounded-md border border-line ${dragging ? "select-none" : ""}`}
        onMouseDown={startDrag}
        onClickCapture={(e) => {
          if (afterDrag.current) {
            e.preventDefault();
            e.stopPropagation();
          }
        }}
      >
        {first > 0 && <li aria-hidden="true" style={{ height: first * rowH }} />}
        {shown.slice(first, last).map((n) => {
          const at = picked.indexOf(n.name);
          const st = state(n);
          const free = n.gpuTotal - n.gpuUsed;
          return (
            <li key={keyOf(n)} data-node data-name={n.name} className="border-b border-line last:border-0">
              <label className={`flex items-center gap-2 px-2 py-1 ${n.selectable ? "cursor-pointer hover:bg-sunk" : "opacity-50"} ${at >= 0 ? "bg-accent-soft" : ""}`}>
                <input
                  type={single ? "radio" : "checkbox"}
                  name={single ? "node" : undefined}
                  aria-label={n.name}
                  checked={at >= 0}
                  disabled={!n.selectable || (full && at < 0)}
                  onChange={() => toggle(n.name)}
                />
                <span className="w-4 text-right text-[10px] text-accent">{!single && at >= 0 ? at : ""}</span>
                <span className="mono min-w-0 flex-1 truncate">{n.name}</span>
                {n.gpuTotal > 0 && (
                  <span className={`mono shrink-0 text-[11px] ${free > 0 ? "text-run" : "text-muted"}`} title={n.gpuModel}>
                    {gpuName(n.gpuModel)} 空闲 {free}/{n.gpuTotal}
                  </span>
                )}
                <span className="mono shrink-0 text-[11px] text-faint">
                  CPU 空闲 {fmtCores(Math.max(0, n.cpu.allocatable - n.cpu.allocated))}/{fmtCores(n.cpu.allocatable)}
                </span>
                {st && <span className="shrink-0 text-[11px] text-warm">{st}</span>}
              </label>
            </li>
          );
        })}
        {last < shown.length && <li aria-hidden="true" style={{ height: (shown.length - last) * rowH }} />}
        {shown.length === 0 && <li className="px-2 py-2 text-faint">{nodes.length ? "没有符合条件的节点" : "没有可选节点"}</li>}
      </ul>
    </fieldset>
  );
}
