// Data shaping for the time series chart, kept free of the canvas so it
// can be tested.
export type ChartSeries = {
  label: string;
  values: (number | null)[];
  /** a theme variable, e.g. "--chart-1" */
  color: string;
  /** a light wash under the line */
  fill?: boolean;
};
export type ChartTheme = { ink: string; muted: string; faint: string; line: string; lineStrong: string; surface: string; color: (name: string) => string };
export type ChartInput = {
  ts: number[]; // ms
  series: ChartSeries[];
  hidden?: ReadonlySet<string>;
  stacked?: boolean;
  limit?: number | null;
  yFormat: (v: number) => string;
};

/** latest: the newest non-null value of a series. */
export function latest(values: (number | null)[]): number | null {
  for (let i = values.length - 1; i >= 0; i--) if (values[i] != null) return values[i];
  return null;
}

const pad = (n: number) => String(n).padStart(2, "0");
/** fmtAxisTime: HH:mm, with the date once the range spans days. */
export function fmtAxisTime(ms: number, spanMs: number): string {
  const d = new Date(ms);
  const hm = `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  return spanMs > 36 * 3600e3 ? `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${hm}` : hm;
}
export function fmtTipTime(ms: number): string {
  const d = new Date(ms);
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

export const rgba = (hex: string, a: number) => {
  const m = /^#?([0-9a-f]{6})$/i.exec(hex.trim());
  if (!m) return hex;
  const n = parseInt(m[1], 16);
  return `rgba(${n >> 16},${(n >> 8) & 255},${n & 255},${a})`;
};

/** stackSeries sums each layer onto the ones below it; a null stays a
 *  gap in its own layer and counts as 0 for the layers above. */
export function stackSeries(series: (number | null)[][]): (number | null)[][] {
  const sum: number[] = [];
  return series.map((layer) =>
    layer.map((v, i) => {
      if (v == null) return null;
      sum[i] = (sum[i] ?? 0) + v;
      return sum[i];
    }),
  );
}

export type Plotted = { label: string; color: string; fill: boolean; raw: (number | null)[]; drawn: (number | null)[] };
/** plotData lays the visible series out for uPlot: seconds on x, and for
 *  a stacked chart cumulative layers listed top first, so each lower
 *  layer paints over the one above it. */
export function plotData(c: ChartInput): { x: number[]; series: Plotted[]; yMax: number } {
  const shown = c.series.filter((s) => !c.hidden?.has(s.label));
  const drawn = c.stacked ? stackSeries(shown.map((s) => s.values)) : shown.map((s) => s.values);
  let series = shown.map((s, i) => ({ label: s.label, color: s.color, fill: !!c.stacked || !!s.fill, raw: s.values, drawn: drawn[i] }));
  if (c.stacked) series = series.reverse();
  let yMax = c.limit ?? 0;
  for (const s of series) for (const v of s.drawn) if (v != null && v > yMax) yMax = v;
  return { x: c.ts.map((t) => t / 1000), series, yMax: yMax > 0 ? yMax * 1.06 : 1 };
}

/** nextHidden: a legend click, Grafana style. A plain click shows only
 *  that series (again: all of them); with multi it toggles just it. */
export function nextHidden(hidden: ReadonlySet<string>, labels: string[], label: string, multi: boolean): Set<string> {
  if (multi) {
    const n = new Set(hidden);
    if (!n.delete(label)) n.add(label);
    return n.size === labels.length ? new Set() : n;
  }
  const others = labels.filter((l) => l !== label);
  const isolated = !hidden.has(label) && others.length > 0 && others.every((l) => hidden.has(l));
  return isolated ? new Set() : new Set(others);
}
