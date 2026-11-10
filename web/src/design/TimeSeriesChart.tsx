// Time series chart (UI spec §5.2 monitor tab) on ECharts: 2px lines in
// the fixed chart palette, hairline grid, a crosshair tooltip listing
// every series, and a legend row of our own showing each series' latest
// value (click to hide a series). Colors are theme variables, re-read
// when the theme changes; data updates redraw in place.
import { useEffect, useMemo, useRef, useState } from "react";
import * as echarts from "echarts/core";
import { LineChart } from "echarts/charts";
import { GridComponent, MarkLineComponent, TooltipComponent } from "echarts/components";
import { CanvasRenderer } from "echarts/renderers";
import { chartOption, fmtTipTime, latest, type ChartSeries, type ChartTheme } from "./chartData";

export type { ChartSeries };
echarts.use([LineChart, GridComponent, TooltipComponent, MarkLineComponent, CanvasRenderer]);

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
    val.style.cssText = "font-weight:600;font-variant-numeric:tabular-nums";
    val.textContent = p.value[1] == null ? "—" : yFormat(p.value[1]);
    const name = document.createElement("span");
    name.style.cssText = `color:${th.muted}`;
    name.textContent = p.seriesName;
    row.append(key, val, name);
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
}: {
  /** timestamps in ms */
  ts: number[];
  series: ChartSeries[];
  stacked?: boolean;
  /** a labelled horizontal reference line */
  limit?: number | null;
  yFormat?: (v: number) => string;
  height?: number;
}) {
  const el = useRef<HTMLDivElement>(null);
  const chart = useRef<echarts.ECharts | null>(null);
  const theme = useThemeVersion();
  const [hidden, setHidden] = useState<ReadonlySet<string>>(new Set());
  const fmt = useRef(yFormat);
  fmt.current = yFormat;

  useEffect(() => {
    if (!el.current) return;
    let c: echarts.ECharts;
    try {
      c = echarts.init(el.current, undefined, { renderer: "canvas" });
    } catch {
      return; // no canvas (tests): the legend row still shows the values
    }
    chart.current = c;
    const ro = new ResizeObserver(() => c.resize());
    ro.observe(el.current);
    return () => {
      ro.disconnect();
      c.dispose();
      chart.current = null;
    };
  }, []);

  const th = useMemo(readTheme, [theme]); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => {
    const c = chart.current;
    if (!c) return;
    const opt = chartOption({ ts, series, hidden, stacked, limit, yFormat: (v) => fmt.current(v) }, th);
    (opt.tooltip as Record<string, unknown>).formatter = (params: TipParam[]) => tip(params, th, (v) => fmt.current(v));
    c.setOption(opt, { notMerge: true, lazyUpdate: true });
  });

  const toggle = (label: string) =>
    setHidden((h) => {
      const n = new Set(h);
      if (!n.delete(label)) n.add(label);
      return n;
    });

  return (
    <figure className="m-0 min-w-0">
      <div className="mb-1 flex flex-wrap items-center gap-x-3 gap-y-0.5 px-1 text-[11px]">
        {series.map((s) => {
          const v = latest(s.values);
          const off = hidden.has(s.label);
          return (
            <button
              key={s.label}
              type="button"
              aria-pressed={!off}
              title={off ? "点击显示" : "点击隐藏"}
              onClick={() => toggle(s.label)}
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
      <div ref={el} style={{ height }} className="w-full min-w-0" />
    </figure>
  );
}
