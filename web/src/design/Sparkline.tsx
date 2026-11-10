// A 60-sample line in a table cell. Nulls (no data) break the line.
export function sparkPoints(values: (number | null)[], w: number, h: number, max?: number): string[] {
  const top = max ?? Math.max(1e-9, ...values.filter((v): v is number => v != null));
  const n = values.length;
  const step = n > 1 ? w / (n - 1) : 0;
  const runs: string[] = [];
  let cur: string[] = [];
  values.forEach((v, i) => {
    if (v == null) {
      if (cur.length) runs.push(cur.join(" "));
      cur = [];
      return;
    }
    const x = n > 1 ? i * step : w;
    const y = h - (Math.min(v, top) / top) * h;
    cur.push(`${+x.toFixed(2)},${+y.toFixed(2)}`);
  });
  if (cur.length) runs.push(cur.join(" "));
  return runs;
}

export function Sparkline({
  values,
  max,
  width = 60,
  height = 16,
  className = "text-accent",
}: {
  values: (number | null)[];
  max?: number;
  width?: number;
  height?: number;
  className?: string;
}) {
  return (
    <svg width={width} height={height} className={className} aria-hidden="true">
      {sparkPoints(values, width, height - 1, max).map((pts, i) => (
        <polyline key={i} points={pts} fill="none" stroke="currentColor" strokeWidth={1.25} vectorEffect="non-scaling-stroke" />
      ))}
    </svg>
  );
}
