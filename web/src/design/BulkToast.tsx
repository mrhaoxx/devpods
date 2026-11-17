// BulkToast: what an action on rows of a list is doing and how it ended —
// over the window's corner, moving nothing. A success says so for a few
// seconds; a failure stays until closed.
import { useEffect, useState } from "react";
import { Notice } from "./Form";
import { runBulk, type BulkResult } from "../lib/bulk";

export type BulkProgress = { action: string; done: number; failed: number; total: number };
export type BulkOutcome = { action: string; results: BulkResult<string>[] };

export function BulkToast({ progress, outcome, onClose }: { progress: BulkProgress | null; outcome: BulkOutcome | null; onClose(): void }) {
  const failed = outcome?.results.filter((r) => !r.ok) ?? [];
  const allOk = !!outcome && failed.length === 0;
  useEffect(() => {
    if (!allOk) return;
    const t = setTimeout(onClose, 5000);
    return () => clearTimeout(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [allOk, outcome]);
  if (!progress && !outcome) return null;
  return (
    <div className="pointer-events-none fixed right-4 bottom-16 z-40 flex max-w-[min(32rem,calc(100vw-2rem))] flex-col gap-2">
      {progress && (
        <div role="status" className="pointer-events-auto rounded-lg border border-line bg-surface p-1 shadow-lg">
          <Notice tone="idle">
            {progress.action}：进行中 {progress.done}/{progress.total}…{progress.failed > 0 && `（失败 ${progress.failed}）`}
          </Notice>
        </div>
      )}
      {outcome && (
        <div role="status" className="pointer-events-auto flex items-start gap-2 rounded-lg border border-line bg-surface p-1 shadow-lg">
          <div className="max-h-60 flex-1 overflow-y-auto">
            <Notice tone={failed.length ? "warm" : "run"}>
              {outcome.action}：成功 {outcome.results.length - failed.length}，失败 {failed.length}
              {failed.length > 0 && <>（{failed.map((f) => `${f.item}：${f.error}`).join("；")}）</>}
            </Notice>
          </div>
          <button type="button" onClick={onClose} className="px-1 text-xs text-muted hover:text-ink" aria-label="关闭结果">
            ✕
          </button>
        </div>
      )}
    </div>
  );
}

/** useBulkRun: an action on rows of a list, one by one (a few at a
 *  time), its progress and how it ended said in the toast. `name` says
 *  which row a failure was; a failure's message is what fn threw. */
export function useBulkRun() {
  const [progress, setProgress] = useState<BulkProgress | null>(null);
  const [outcome, setOutcome] = useState<BulkOutcome | null>(null);
  const run = async <T,>(action: string, items: T[], fn: (t: T) => Promise<unknown>, name: (t: T) => string) => {
    setOutcome(null);
    setProgress({ action, done: 0, failed: 0, total: items.length });
    const results = await runBulk(items, async (t) => {
      try {
        await fn(t);
        setProgress((p) => p && { ...p, done: p.done + 1 });
      } catch (e) {
        setProgress((p) => p && { ...p, done: p.done + 1, failed: p.failed + 1 });
        throw e;
      }
    });
    setProgress(null);
    setOutcome({ action, results: results.map((x) => ({ ...x, item: name(x.item) })) });
    return results;
  };
  const toast = <BulkToast progress={progress} outcome={outcome} onClose={() => setOutcome(null)} />;
  return { run, toast };
}
