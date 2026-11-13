// Picking nodes when there are many: a list to search and filter, with
// each node's free GPUs and state; picked in order — the order numbers
// the DevPods (INDEX) or pairs them with the nodes of a migration. A
// node someone else holds now cannot be picked. single: one node.
import { keyOf } from "../../data/cluster";
import { useLayoutEffect, useMemo, useRef, useState } from "react";
import { rowWindow, useView } from "../../design/DataTable";
import type { NodeChoice } from "../../data/api";
import { useNodeChoices } from "../../data/queries";
import { fmtCores } from "../../lib/format";
import { fmtTime } from "../../lib/reservations";

const gpuName = (m?: string) => (m ?? "GPU").replace(/^NVIDIA-/, "");
const usable = (n: NodeChoice) => n.selectable && n.ready;

function state(n: NodeChoice): string | null {
  if (!n.selectable && n.held) return `${n.held.user} 预约至 ${fmtTime(n.held.until)}`;
  if (!n.ready) return "未就绪";
  if (n.held) return `你的预约至 ${fmtTime(n.held.until)}`;
  if (n.next) return `${fmtTime(n.next.start)} 起被 ${n.next.user} 预约`;
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
  const [onlyUsable, setOnlyUsable] = useState(false);
  const [onlyGpu, setOnlyGpu] = useState(false);
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
  useLayoutEffect(() => {
    const h = list.current?.querySelector("li[data-node]")?.getBoundingClientRect().height ?? 0;
    if (h > 0 && Math.abs(h - rowH) > 0.25) setRowH(h);
  });

  const toggle = (name: string) => {
    if (single) return setPicked([name]);
    if (picked.includes(name)) setPicked(picked.filter((x) => x !== name));
    else if (!full) setPicked([...picked, name]);
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
          <input type="checkbox" checked={onlyUsable} onChange={(e) => setOnlyUsable(e.target.checked)} />
          只看可用
        </label>
        <label className="inline-flex items-center gap-1 text-[11px]">
          <input type="checkbox" checked={onlyGpu} onChange={(e) => setOnlyGpu(e.target.checked)} />
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
      <ul ref={list} className="max-h-72 overflow-y-auto rounded-md border border-line">
        {first > 0 && <li aria-hidden="true" style={{ height: first * rowH }} />}
        {shown.slice(first, last).map((n) => {
          const at = picked.indexOf(n.name);
          const st = state(n);
          const free = n.gpuTotal - n.gpuUsed;
          return (
            <li key={keyOf(n)} data-node className="border-b border-line last:border-0">
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
