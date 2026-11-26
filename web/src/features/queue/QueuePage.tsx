// 队列 (design 2026-11-26): the jobs waiting for the reservable machines'
// unreserved capacity and those running, in the order they run — why each
// waits and when the first is expected to start. A row opens the job.
import { useMemo, useState } from "react";
import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { useNavigate, useSearchParams } from "react-router-dom";
import type { ColumnDef, SortingState } from "@tanstack/react-table";
import { jobsFree, listJobs, type Job } from "../../data/api";
import { clusterSeg } from "../../data/cluster";
import { useClusterFilter } from "../../data/clusterFilter";
import { Button } from "../../design/Button";
import { Chip } from "../../design/Chip";
import { DataTable } from "../../design/DataTable";
import { EmptyState } from "../../design/EmptyState";
import { Notice } from "../../design/Form";
import { useNow } from "../../lib/useNow";
import { fmtHours, sizeText, stateText } from "./jobs";
import { SubmitDialog } from "./SubmitDialog";

const failure = (e: unknown) => (e instanceof Error ? e.message : String(e));

export function QueuePage() {
  const [params, setParams] = useSearchParams();
  const mine = params.get("mine") === "1";
  const ended = params.get("ended") === "1";
  // one update for all the keys: two in a row would each start from the
  // same (stale) params
  const set = (keys: Record<string, boolean>) =>
    setParams(
      (p) => {
        const next = new URLSearchParams(p);
        for (const [k, on] of Object.entries(keys)) {
          if (on) next.set(k, "1");
          else next.delete(k);
        }
        return next;
      },
      { replace: true },
    );
  const navigate = useNavigate();
  const [submitting, setSubmitting] = useState(false);
  const [sorting, setSorting] = useState<SortingState>([]); // none: the queue's own order
  const now = useNow(30_000).getTime();
  const { matches, several } = useClusterFilter();
  const q = useQuery({ queryKey: ["jobs", { mine, ended }], queryFn: () => listJobs({ mine, ended }), refetchInterval: 5_000, placeholderData: keepPreviousData });
  // what the queue can use now (by cluster): not shown when it cannot be said
  const free = useQuery({ queryKey: ["jobs-free"], queryFn: jobsFree, refetchInterval: 15_000 });
  const freeText = (free.data?.items ?? [])
    .filter((f) => matches(f))
    .map((f) => `${several && f.cluster ? `${f.cluster} ` : ""}${f.gpu} 卡 · ${f.cpu} 核${f.gpu > 0 ? `（单台最多 ${f.maxGPU} 卡）` : ""}`)
    .join("；");

  const rows = useMemo(() => {
    const items = (q.data?.items ?? []).filter(matches);
    // running first, then the waiting in their order, then the ended (latest first)
    const rank = (j: Job) => (j.phase === "Running" || j.phase === "Starting" ? 0 : j.phase === "Queued" ? 1 : 2);
    return [...items].sort(
      (a, b) =>
        rank(a) - rank(b) ||
        (a.position ?? 0) - (b.position ?? 0) ||
        Date.parse(b.finishedAt ?? b.submitted) - Date.parse(a.finishedAt ?? a.submitted),
    );
  }, [q.data, matches]);

  const columns = useMemo<ColumnDef<Job>[]>(
    () => [
      { id: "position", header: "排位", accessorFn: (j) => j.position ?? 0, meta: { priority: 1, width: 56 }, cell: ({ row }) => (row.original.position ? `#${row.original.position}` : "") },
      ...(several ? [{ id: "cluster", header: "集群", accessorFn: (j: Job) => j.cluster ?? "", meta: { priority: 2, width: 90, mono: true } }] : []),
      {
        id: "job",
        header: "任务",
        accessorFn: (j) => j.id,
        meta: { priority: 1, width: 220, pin: "left" },
        cell: ({ row }) => (
          <span className="inline-flex min-w-0 items-center gap-1">
            <span className="mono truncate">{`${row.original.id}${row.original.title ? ` · ${row.original.title}` : ""}`}</span>
            {row.original.preemptible && (
              <span title="可被抢占" className="text-warm">
                ⚡
              </span>
            )}
          </span>
        ),
      },
      { id: "owner", header: "用户", accessorKey: "owner", meta: { priority: 1, width: 100, mono: true } },
      { id: "size", header: "规模", accessorFn: (j) => j.nodes * Math.max(j.gpus, 1), meta: { priority: 1, width: 100 }, cell: ({ row }) => sizeText(row.original) },
      { id: "priority", header: "优先级", accessorKey: "priority", meta: { priority: 2, width: 64 } },
      { id: "limit", header: "时长上限", accessorKey: "timeLimitSeconds", meta: { priority: 2, width: 80 }, cell: ({ row }) => fmtHours(row.original.timeLimitSeconds * 1000) },
      { id: "state", header: "状态", accessorFn: (j) => j.phase, meta: { priority: 1, width: 400 }, cell: ({ row }) => stateText(row.original, now) },
    ],
    [several, now],
  );

  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex flex-wrap items-center gap-2 border-b border-line px-3 py-2">
        <Chip active={!mine && !ended} onClick={() => set({ mine: false, ended: false })}>
          全部
        </Chip>
        <Chip active={mine} onClick={() => set({ mine: !mine })}>
          我的
        </Chip>
        <Chip active={ended} onClick={() => set({ ended: !ended })}>
          已结束
        </Chip>
        {freeText && <span className="text-xs text-muted">队列现在可用：{freeText}</span>}
        <span className="flex-1" />
        <Button variant="primary" onClick={() => setSubmitting(true)}>
          提交任务…
        </Button>
      </div>
      <p className="border-b border-line px-3 py-1.5 text-[11px] text-muted">
        任务排队等开放预约的机器上没被预约的卡和核：优先级高的先运行，同级按提交先后；一个任务的几台一起开始。在 DevPod 里也可以用 <span className="mono">dp job submit</span> 或{" "}
        <span className="mono">sbatch</span> 提交，SSH 命令行里是 <span className="mono">job submit</span>。
      </p>
      {q.isError && q.data && (
        <div className="border-b border-line px-3 py-1.5">
          <Notice tone="warm">刷新失败（{failure(q.error)}），显示的是上次的数据。</Notice>
        </div>
      )}
      {q.isError && !q.data ? (
        <EmptyState tone="error" title="队列不可用" hint={failure(q.error)} />
      ) : (
        <DataTable<Job>
          fill
          storageKey="queue"
          label="队列"
          data={rows}
          columns={columns}
          getRowId={(j) => `${j.cluster ?? ""}/${j.id}`}
          sorting={sorting}
          onSortingChange={setSorting}
          empty={<EmptyState title={q.isLoading ? "加载中…" : ended ? "没有任务" : "队列是空的"} hint={q.isLoading ? undefined : "点「提交任务…」，或在 DevPod 里运行 dp job submit。"} />}
          onRowClick={(j) => navigate(`/queue/${clusterSeg(j.cluster)}/${j.id}`)}
        />
      )}
      {submitting && <SubmitDialog onClose={() => setSubmitting(false)} />}
    </div>
  );
}
