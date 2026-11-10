// The ECharts option for a time series chart, built as plain data so it
// can be tested without a canvas.
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

const rgba = (hex: string, a: number) => {
  const m = /^#?([0-9a-f]{6})$/i.exec(hex.trim());
  if (!m) return hex;
  const n = parseInt(m[1], 16);
  return `rgba(${n >> 16},${(n >> 8) & 255},${n & 255},${a})`;
};

export function chartOption(c: ChartInput, th: ChartTheme): Record<string, unknown> {
  const span = c.ts.length ? c.ts[c.ts.length - 1] - c.ts[0] : 0;
  const shown = c.series.filter((s) => !c.hidden?.has(s.label));
  const limit = c.limit ?? null;
  return {
    animation: false,
    grid: { left: 4, right: 10, top: 10, bottom: 2, containLabel: true },
    textStyle: { fontFamily: "inherit" },
    xAxis: {
      type: "time",
      min: c.ts[0],
      max: c.ts[c.ts.length - 1],
      axisLine: { lineStyle: { color: th.line } },
      axisTick: { show: false },
      splitLine: { show: false },
      axisLabel: { color: th.muted, fontSize: 11, hideOverlap: true, formatter: (v: number) => fmtAxisTime(v, span) },
    },
    yAxis: {
      type: "value",
      min: 0,
      max: limit != null ? (v: { max: number }) => Math.max(v.max, limit) * 1.06 : undefined,
      splitNumber: 3,
      axisLabel: { color: th.muted, fontSize: 11, formatter: (v: number) => c.yFormat(v) },
      splitLine: { lineStyle: { color: th.line, width: 1 } },
    },
    tooltip: {
      trigger: "axis",
      confine: true,
      transitionDuration: 0,
      backgroundColor: th.surface,
      borderColor: th.line,
      borderWidth: 1,
      padding: [6, 8],
      extraCssText: "box-shadow:0 4px 14px rgba(0,0,0,.12);border-radius:6px;",
      axisPointer: { type: "line", lineStyle: { color: th.lineStrong, width: 1, type: "solid" } },
    },
    series: shown.map((s, i) => {
      const color = th.color(s.color);
      return {
        name: s.label,
        type: "line",
        data: c.ts.map((t, k) => [t, s.values[k] ?? null]),
        stack: c.stacked ? "all" : undefined,
        showSymbol: false,
        symbol: "circle",
        symbolSize: 7,
        connectNulls: false,
        sampling: "lttb",
        lineStyle: { width: c.stacked ? 1.5 : 2, color, join: "round", cap: "round" },
        itemStyle: { color, borderColor: th.surface, borderWidth: 2 },
        emphasis: { disabled: true },
        areaStyle:
          c.stacked || s.fill
            ? {
                color: c.stacked
                  ? rgba(color, 0.22)
                  : { type: "linear", x: 0, y: 0, x2: 0, y2: 1, colorStops: [{ offset: 0, color: rgba(color, 0.2) }, { offset: 1, color: rgba(color, 0.02) }] },
              }
            : undefined,
        markLine:
          i === 0 && limit != null
            ? {
                silent: true,
                symbol: "none",
                animation: false,
                lineStyle: { color: th.faint, width: 1, type: "solid" },
                label: { formatter: `限额 ${c.yFormat(limit)}`, position: "insideEndTop", color: th.muted, fontSize: 10, distance: 2 },
                data: [{ yAxis: limit }],
              }
            : undefined,
      };
    }),
  };
}
