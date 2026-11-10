// Sparkline points from the persistent history plus the live tail (UI
// feedback round 1): both are bucketed over the window ending now, so a
// page shows its curve at once and the live stream only extends it.
export type History = { ts: number[] /* unix seconds */; v: (number | null)[] };

export function sparkSeries(
  hist: History | undefined,
  live: ReadonlyArray<{ ts: number /* ms */; v: number | null | undefined }>,
  nowMs: number,
  windowMs = 15 * 60_000,
  buckets = 60,
): (number | null)[] {
  const start = nowMs - windowMs;
  const width = windowMs / buckets;
  const sum = new Array<number>(buckets).fill(0);
  const cnt = new Array<number>(buckets).fill(0);
  const add = (t: number, v: number | null | undefined) => {
    if (v == null || t < start || t >= nowMs) return;
    const i = Math.floor((t - start) / width);
    sum[i] += v;
    cnt[i]++;
  };
  hist?.ts.forEach((s, i) => add(s * 1000, hist.v[i]));
  for (const p of live) add(p.ts, p.v);
  return sum.map((s, i) => (cnt[i] ? s / cnt[i] : null));
}
