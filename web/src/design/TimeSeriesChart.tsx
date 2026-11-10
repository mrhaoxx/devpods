// uPlot time series (UI spec §5.2 monitor tab): colors come from the
// theme's CSS variables and are re-read when the theme changes.
import { useEffect, useRef, useState } from "react";
import uPlot from "uplot";
import "uplot/dist/uPlot.min.css";
import { stackSeries, toUplotData } from "./chartData";

export type ChartSeries = {
  label: string;
  values: (number | null)[];
  /** a theme variable, e.g. "--color-accent" */
  color: string;
  fill?: boolean;
  dash?: boolean;
};

const cssVar = (name: string) => getComputedStyle(document.documentElement).getPropertyValue(name).trim() || "#888";

function useThemeVersion(): number {
  const [v, setV] = useState(0);
  useEffect(() => {
    const mo = new MutationObserver(() => setV((x) => x + 1));
    mo.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
    return () => mo.disconnect();
  }, []);
  return v;
}

export function TimeSeriesChart({
  ts,
  series,
  stacked,
  limit,
  yFormat = (v) => String(+v.toFixed(2)),
  height = 140,
}: {
  /** timestamps in ms */
  ts: number[];
  series: ChartSeries[];
  stacked?: boolean;
  /** a dashed horizontal limit line */
  limit?: number | null;
  yFormat?: (v: number) => string;
  height?: number;
}) {
  const el = useRef<HTMLDivElement>(null);
  const plot = useRef<uPlot | null>(null);
  const theme = useThemeVersion();
  const signature = series.map((s) => s.label + s.color).join("|") + `|${stacked}|${limit != null}`;

  const values = stacked ? stackSeries(series.map((s) => s.values)) : series.map((s) => s.values);
  const all = limit != null ? [...values, ts.map(() => limit)] : values;
  const data = toUplotData(ts, all);

  useEffect(() => {
    if (!el.current) return;
    const muted = cssVar("--color-muted");
    const line = cssVar("--color-line");
    const opts: uPlot.Options = {
      width: el.current.clientWidth || 300,
      height,
      legend: { show: true, live: true },
      cursor: { drag: { x: false, y: false } },
      scales: { x: { time: true }, y: { range: (_u, min, max) => [Math.min(0, min), max > 0 ? max * 1.1 : 1] } },
      axes: [
        { stroke: muted, grid: { stroke: line, width: 1 }, ticks: { stroke: line } },
        { stroke: muted, grid: { stroke: line, width: 1 }, ticks: { stroke: line }, size: 56, values: (_u, vals) => vals.map((v) => (v == null ? "" : yFormat(v))) },
      ],
      series: [
        {},
        ...series.map((s) => {
          const c = cssVar(s.color);
          return { label: s.label, stroke: c, width: 1.25, fill: s.fill ? c + "33" : undefined, dash: s.dash ? [4, 4] : undefined, value: (_u: uPlot, v: number | null) => (v == null ? "—" : yFormat(v)) };
        }),
        ...(limit != null ? [{ label: "限额", stroke: cssVar("--color-fail"), width: 1, dash: [4, 4] }] : []),
      ],
    };
    plot.current = new uPlot(opts, data, el.current);
    const ro = new ResizeObserver(([e]) => plot.current?.setSize({ width: Math.floor(e.contentRect.width), height }));
    ro.observe(el.current);
    return () => {
      ro.disconnect();
      plot.current?.destroy();
      plot.current = null;
    };
    // rebuilt when the series set, the height or the theme changes
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [signature, height, theme]);

  useEffect(() => {
    plot.current?.setData(data);
  });

  return <div ref={el} className="w-full min-w-0 text-[11px]" />;
}
