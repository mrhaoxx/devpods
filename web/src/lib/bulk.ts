// runBulk applies fn to every item, at most `concurrency` at a time, and
// reports each outcome; one failure never stops the rest (UI spec §5.1).
export type BulkResult<T> = { item: T; ok: boolean; error?: string };

export async function runBulk<T>(items: T[], fn: (t: T) => Promise<unknown>, concurrency = 4): Promise<BulkResult<T>[]> {
  const out: BulkResult<T>[] = new Array(items.length);
  let next = 0;
  const worker = async () => {
    while (next < items.length) {
      const i = next++;
      try {
        await fn(items[i]);
        out[i] = { item: items[i], ok: true };
      } catch (e) {
        out[i] = { item: items[i], ok: false, error: e instanceof Error ? e.message : String(e) };
      }
    }
  };
  await Promise.all(Array.from({ length: Math.min(concurrency, items.length) }, worker));
  return out;
}
