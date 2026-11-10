import type uPlot from "uplot";

/** toUplotData: millisecond timestamps → uPlot's seconds, plus series. */
export function toUplotData(tsMs: number[], series: (number | null)[][]): uPlot.AlignedData {
  return [tsMs.map((t) => t / 1000), ...series] as uPlot.AlignedData;
}

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
