// Time series chart (UI spec §5.2 monitor tab) on uPlot, set up the way
// Grafana's panel is: thin lines in the fixed chart palette with a soft
// gradient, a faint grid, a crosshair shared by every chart on the page,
// a tooltip listing all series, drag to zoom (double-click resets), the
// limit as a labelled reference line, and the legend below with each
// series' latest value (click: only this one; ctrl / ⌘ click: toggle).
// Colors are theme variables, re-read when the theme changes.
//
// A zoom is what the x scale shows, whoever set it (this chart, a chart
// it is synced with, a double click anywhere): it holds while the data
// ticks in, survives the chart being rebuilt, and ends with zoomKey (a
// new time range). Zoomed in, the y axis fits what is shown. On a touch
// screen a finger moved along a chart shows the values under it.
import { useEffect, useMemo, useRef, useState } from "react";
import uPlot from "uplot";
import "uplot/dist/uPlot.min.css";
import { fmtAxisTime, fmtTipTime, latest, nextHidden, plotData, rgba, type ChartSeries, type ChartTheme } from "./chartData";

export type { ChartSeries };
const SYNC = "devpod-timeseries"; // charts sharing it move their crosshairs together
const FONT = '11px ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif';

let canvasOK: boolean | undefined;
const hasCanvas = () => {
  if (canvasOK === undefined) {
    try {
      canvasOK = !!document.createElement("canvas").getContext("2d");
    } catch {
      canvasOK = false;
    }
  }
  return canvasOK;
};

const cssVar = (name: string) => getComputedStyle(document.documentElement).getPropertyValue(name).trim() || "#888";
const readTheme = (): ChartTheme => ({
  ink: cssVar("--color-ink"),
  muted: cssVar("--color-muted"),
  faint: cssVar("--color-faint"),
  line: cssVar("--color-line"),
  lineStrong: cssVar("--color-line-strong"),
  surface: cssVar("--color-surface"),
  color: cssVar,
});

function useThemeVersion(): number {
  const [v, setV] = useState(0);
  useEffect(() => {
    const mo = new MutationObserver(() => setV((x) => x + 1));
    mo.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
    return () => mo.disconnect();
  }, []);
  return v;
}

/** visibleMax: the y range's top for the part of the data a zoom shows. */
function visibleMax(c: { limit: number | null; p: ReturnType<typeof plotData> }, min: number | undefined, max: number | undefined): number {
  const { x, series, yMax } = c.p;
  if (min == null || max == null) return yMax;
  let top = c.limit ?? 0;
  for (let i = 0; i < x.length; i++) {
    if (x[i] < min || x[i] > max) continue;
    for (const s of series) {
      const v = s.drawn[i];
      if (v != null && v > top) top = v;
    }
  }
  return top > 0 ? top * 1.06 : yMax;
}

type TipParam = { seriesName: string; value: [number, number | null]; color: string };
/** The tooltip, built as DOM so series names are never parsed as HTML. */
function tip(params: TipParam[], th: ChartTheme, yFormat: (v: number) => string): HTMLElement {
  const box = document.createElement("div");
  box.style.cssText = `font-size:12px;line-height:1.5;color:${th.ink};min-width:120px`;
  const head = document.createElement("div");
  head.style.cssText = `color:${th.muted};font-size:11px;margin-bottom:2px;font-variant-numeric:tabular-nums`;
  head.textContent = params.length ? fmtTipTime(params[0].value[0]) : "";
  box.append(head);
  for (const p of params) {
    const row = document.createElement("div");
    row.style.cssText = "display:flex;align-items:center;gap:6px;white-space:nowrap";
    const key = document.createElement("span");
    key.style.cssText = `width:10px;height:2px;border-radius:1px;background:${p.color};flex:none`;
    const val = document.createElement("span");
    val.style.cssText = "font-weight:600;font-variant-numeric:tabular-nums;margin-left:auto;padding-left:12px";
    val.textContent = p.value[1] == null ? "—" : yFormat(p.value[1]);
    const name = document.createElement("span");
    name.style.cssText = `color:${th.muted}`;
    name.textContent = p.seriesName;
    row.append(key, name, val);
    box.append(row);
  }
  return box;
}

export function TimeSeriesChart({
  ts,
  series,
  stacked,
  limit,
  yFormat = (v) => String(+v.toFixed(2)),
  height = 150,
  zoomKey,
}: {
  /** timestamps in ms */
  ts: number[];
  series: ChartSeries[];
  stacked?: boolean;
  /** a labelled horizontal reference line */
  limit?: number | null;
  yFormat?: (v: number) => string;
  height?: number;
  /** a change of it (a new time range) ends a zoom */
  zoomKey?: string;
}) {
  const el = useRef<HTMLDivElement>(null);
  const plot = useRef<uPlot | null>(null);
  const zoomed = useRef(false);
  // the zoom to put back when the chart is rebuilt (its series changed)
  const kept = useRef<{ min: number; max: number } | null>(null);
  const lastKey = useRef(zoomKey);
  const theme = useThemeVersion();
  const [hidden, setHidden] = useState<ReadonlySet<string>>(new Set());
  const th = useMemo(readTheme, [theme]); // eslint-disable-line react-hooks/exhaustive-deps
  // what the draw hooks read; refreshed on every render
  const cur = useRef({ yFormat, limit: limit ?? null, p: plotData({ ts, series, hidden, stacked, limit, yFormat }) });
  cur.current = { yFormat, limit: limit ?? null, p: plotData({ ts, series, hidden, stacked, limit, yFormat }) };
  const p = cur.current.p;
  const data = [p.x, ...p.series.map((s) => s.drawn)] as uPlot.AlignedData;
  const signature = p.series.map((s) => s.label + s.color + s.fill).join("|") + `|${stacked}`;

  useEffect(() => {
    const host = el.current;
    if (!host || !hasCanvas()) return; // no canvas (tests): the legend still shows the values
    const tipEl = document.createElement("div");
    tipEl.style.cssText = `position:absolute;z-index:20;pointer-events:none;display:none;padding:6px 8px;border-radius:6px;border:1px solid ${th.line};background:${th.surface};box-shadow:0 4px 14px rgba(0,0,0,.12)`;
    let inside = false;
    const span = () => (cur.current.p.x.length ? (cur.current.p.x[cur.current.p.x.length - 1] - cur.current.p.x[0]) * 1000 : 0);
    const opts: uPlot.Options = {
      width: host.clientWidth || 300,
      height,
      legend: { show: false },
      padding: [8, 8, 0, 0],
      cursor: {
        sync: { key: SYNC },
        drag: { x: true, y: false, setScale: true },
        points: { size: 7, width: 2, stroke: () => th.surface, fill: (_u, i) => th.color(cur.current.p.series[i - 1]?.color ?? "--chart-1") },
        x: true,
        y: false,
      },
      scales: {
        x: { time: true },
        // zoomed in: the highest value shown, not the highest of all
        y: { range: (u) => [0, zoomed.current ? visibleMax(cur.current, u.scales.x.min, u.scales.x.max) : cur.current.p.yMax] },
      },
      axes: [
        {
          stroke: th.muted,
          font: FONT,
          grid: { stroke: rgba(th.line, 0.7), width: 1 },
          ticks: { show: false },
          values: (_u, splits) => splits.map((v) => fmtAxisTime(v * 1000, span())),
        },
        {
          stroke: th.muted,
          font: FONT,
          size: 58,
          grid: { stroke: th.line, width: 1 },
          ticks: { show: false },
          values: (_u, splits) => splits.map((v) => (v == null ? "" : cur.current.yFormat(v))),
        },
      ],
      series: [
        {},
        ...p.series.map((s) => {
          const c = th.color(s.color);
          return {
            label: s.label,
            stroke: c,
            width: 1.5,
            points: { show: false },
            spanGaps: false,
            fill: !s.fill
              ? undefined
              : stacked
                ? rgba(c, 0.28)
                : (u: uPlot) => {
                    const g = u.ctx.createLinearGradient(0, u.bbox.top, 0, u.bbox.top + u.bbox.height);
                    g.addColorStop(0, rgba(c, 0.22));
                    g.addColorStop(1, rgba(c, 0.02));
                    return g;
                  },
          } satisfies uPlot.Series;
        }),
      ],
      hooks: {
        setScale: [
          (u, key) => {
            if (key !== "x") return;
            const x = cur.current.p.x;
            const { min, max } = u.scales.x;
            // zoomed: the scale shows less than the data spans
            zoomed.current = x.length > 1 && min != null && max != null && (min > x[0] + 1e-6 || max < x[x.length - 1] - 1e-6);
            kept.current = zoomed.current && min != null && max != null ? { min, max } : null;
          },
        ],
        draw: [
          (u) => {
            const lim = cur.current.limit;
            if (lim == null) return;
            const { ctx, bbox } = u;
            const y = Math.round(u.valToPos(lim, "y", true)) + 0.5;
            if (y < bbox.top || y > bbox.top + bbox.height) return;
            ctx.save();
            ctx.strokeStyle = th.faint;
            ctx.lineWidth = devicePixelRatio;
            ctx.beginPath();
            ctx.moveTo(bbox.left, y);
            ctx.lineTo(bbox.left + bbox.width, y);
            ctx.stroke();
            ctx.fillStyle = th.muted;
            ctx.font = `${10 * devicePixelRatio}px ui-sans-serif, system-ui, sans-serif`;
            ctx.textAlign = "right";
            ctx.textBaseline = "bottom";
            ctx.fillText(`限额 ${cur.current.yFormat(lim)}`, bbox.left + bbox.width - 4 * devicePixelRatio, y - 2 * devicePixelRatio);
            ctx.restore();
          },
        ],
        setCursor: [
          (u) => {
            const i = u.cursor.idx;
            if (!inside || i == null || u.cursor.left == null || u.cursor.left < 0) {
              tipEl.style.display = "none";
              return;
            }
            const now = cur.current;
            const rows = now.p.series.filter((s) => !s.hidden).map((s) => ({ seriesName: s.label, value: [now.p.x[i] * 1000, s.raw[i] ?? null] as [number, number | null], color: th.color(s.color) }));
            tipEl.replaceChildren(tip(stacked ? [...rows].reverse() : rows, th, (v) => now.yFormat(v)));
            tipEl.style.display = "block";
            const left = u.over.offsetLeft + u.cursor.left;
            const w = tipEl.offsetWidth;
            tipEl.style.left = `${left + 14 + w > host.clientWidth ? Math.max(0, left - w - 14) : left + 14}px`;
            tipEl.style.top = `${u.over.offsetTop + 6}px`;
          },
        ],
      },
    };
    const keep = kept.current;
    const u = new uPlot(opts, data, host);
    plot.current = u;
    if (keep) u.setScale("x", keep); // rebuilt (its series changed): the zoom stays
    host.append(tipEl);
    const enter = () => (inside = true);
    const leave = () => {
      inside = false;
      tipEl.style.display = "none";
    };
    // a finger along the chart shows the values under it (uPlot follows
    // the mouse only); the page still scrolls up and down (touch-action)
    let lift: ReturnType<typeof setTimeout> | undefined;
    const touch = (e: TouchEvent) => {
      const t = e.touches[0];
      if (!t) return;
      clearTimeout(lift);
      const r = u.over.getBoundingClientRect();
      inside = true;
      u.setCursor({ left: Math.min(Math.max(0, t.clientX - r.left), r.width), top: Math.min(Math.max(0, t.clientY - r.top), r.height) });
    };
    const untouch = () => {
      clearTimeout(lift);
      lift = setTimeout(() => {
        leave();
        u.setCursor({ left: -10, top: -10 });
      }, 1500);
    };
    u.over.style.touchAction = "pan-y";
    u.over.addEventListener("mouseenter", enter);
    u.over.addEventListener("mouseleave", leave);
    u.over.addEventListener("touchstart", touch, { passive: true });
    u.over.addEventListener("touchmove", touch, { passive: true });
    u.over.addEventListener("touchend", untouch);
    u.over.addEventListener("touchcancel", untouch);
    const ro = new ResizeObserver(([e]) => u.setSize({ width: Math.floor(e.contentRect.width), height }));
    ro.observe(host);
    return () => {
      clearTimeout(lift);
      ro.disconnect();
      u.destroy();
      tipEl.remove();
      plot.current = null;
    };
    // rebuilt when the series set, the height or the theme changes
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [signature, height, th]);

  useEffect(() => {
    // a new time range ends a zoom
    if (lastKey.current !== zoomKey) {
      lastKey.current = zoomKey;
      zoomed.current = false;
      kept.current = null;
    }
    // a zoom survives the data ticking in every second
    plot.current?.setData(data, !zoomed.current);
    if (zoomed.current) plot.current?.redraw();
  });

  const labels = series.map((x) => x.label);
  return (
    <figure className="m-0 min-w-0">
      <div ref={el} className="relative w-full min-w-0" style={{ height }} title="拖拽框选放大，双击还原" />
      <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-0.5 px-1 text-[11px]">
        {series.map((s) => {
          const v = latest(s.values);
          const off = hidden.has(s.label);
          return (
            <button
              key={s.label}
              type="button"
              aria-pressed={!off}
              title="点击只看这一条；Ctrl / ⌘ 点击开关"
              onClick={(e) => setHidden((h) => nextHidden(h, labels, s.label, e.ctrlKey || e.metaKey || e.shiftKey))}
              className={`inline-flex min-w-0 items-center gap-1.5 rounded px-1 py-0.5 hover:bg-sunk ${off ? "opacity-40" : ""}`}
            >
              <span className="h-0.5 w-3 shrink-0 rounded-full" style={{ background: `var(${s.color})` }} aria-hidden />
              <span className="truncate text-muted">{s.label}</span>
              <span className="mono font-medium tabular-nums text-ink">{v == null ? "—" : yFormat(v)}</span>
            </button>
          );
        })}
        {limit != null && (
          <span className="ml-auto text-faint">
            限额 <span className="mono tabular-nums text-muted">{yFormat(limit)}</span>
          </span>
        )}
      </div>
    </figure>
  );
}
