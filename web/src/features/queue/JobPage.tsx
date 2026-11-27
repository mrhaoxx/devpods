// A job's page (design 2026-11-26): where it is in the queue or how it
// ran — its ranks, the machine and DevPod of each, how each ended — and,
// for its owner, each rank's output (live while it runs, kept after),
// the SSH login of a running rank, 取消 and 重新提交.
import { useEffect, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useParams } from "react-router-dom";
import { ApiFailure, cancelJob, getJob, getJobHistory, resubmitJob, sshCommand, type Job } from "../../data/api";
import { clusterSeg, segCluster } from "../../data/cluster";
import { useMe } from "../../data/queries";
import { HistoryList } from "../audit/History";
import { Button } from "../../design/Button";
import { ConfirmDialog } from "../../design/ConfirmDialog";
import { copyText } from "../../design/Copy";
import { EmptyState } from "../../design/EmptyState";
import { Notice, Select } from "../../design/Form";
import { Panel } from "../../design/Panel";
import { phaseLabel } from "../../design/StatusDot";
import { fmtTime } from "../../lib/reservations";
import { useNow } from "../../lib/useNow";
import { fmtHours, jobEnded, sizeText, stateText } from "./jobs";

const failure = (e: unknown) => (e instanceof ApiFailure ? e.body.message : String(e));
const MAX_LINES = 5000;

export function JobPage() {
  const { cluster: seg, id = "" } = useParams();
  const cluster = segCluster(seg);
  const nav = useNavigate();
  const qc = useQueryClient();
  const me = useMe().data;
  const now = useNow(30_000).getTime();
  const q = useQuery({
    queryKey: ["job", id],
    queryFn: () => getJob(id),
    refetchInterval: (query) => (query.state.data && jobEnded(query.state.data) ? false : 3_000),
  });
  const [confirming, setConfirming] = useState(false);
  const [error, setError] = useState("");
  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ["job", id] });
    void qc.invalidateQueries({ queryKey: ["jobs"] });
  };
  const cancel = useMutation({ mutationFn: () => cancelJob(id), onSuccess: refresh, onError: (e) => setError(failure(e)) });
  const again = useMutation({
    mutationFn: () => resubmitJob(id),
    onSuccess: (j) => {
      void qc.invalidateQueries({ queryKey: ["jobs"] });
      nav(`/queue/${clusterSeg(j.cluster ?? cluster)}/${j.id}`);
    },
    onError: (e) => setError(failure(e)),
  });

  if (q.isError && !q.data) return <EmptyState tone="error" title="读不到这个任务" hint={failure(q.error)} />;
  const j = q.data;
  if (!j) return <EmptyState title="加载中…" />;
  const ended = jobEnded(j);
  const running = j.phase === "Running" || j.phase === "Starting";
  const meta: [string, string][] = [
    ["规模", sizeText(j) + (j.gpus > 0 ? ` · 每台 ${j.cpus} 核` : "")],
    ["优先级", `${j.priority}${j.preemptible ? " · ⚡ 可被抢占" : ""}`],
    ["被停时", j.requeue === false ? "结束，不再排队" : "重新排队"],
    ["时长上限", fmtHours(j.timeLimitSeconds * 1000)],
    ["提交", fmtTime(j.submitted)],
    ...(j.startedAt ? ([["开始", fmtTime(j.startedAt)]] as [string, string][]) : []),
    ...(j.finishedAt ? ([["结束", fmtTime(j.finishedAt)]] as [string, string][]) : []),
    ...(j.attempts && j.attempts > 1 ? ([["启动", `${j.attempts} 次`]] as [string, string][]) : []),
    ...(j.preemptions ? ([["被抢占", `${j.preemptions} 次`]] as [string, string][]) : []),
  ];

  return (
    <div className="space-y-3 p-3">
      <Link to="/queue" className="text-xs text-accent hover:underline">
        ← 队列
      </Link>
      <header className="flex flex-wrap items-center gap-2">
        <h2 className="mono text-base font-semibold">{`${j.id}${j.title ? ` · ${j.title}` : ""}`}</h2>
        {j.cluster && <span className="rounded border border-line px-1.5 text-[11px] text-muted">{j.cluster}</span>}
        <span className="text-xs">{stateText(j, now)}</span>
        <span className="flex-1" />
        {j.canManage && !ended && (
          <Button variant="danger" onClick={() => setConfirming(true)} disabled={cancel.isPending}>
            取消任务
          </Button>
        )}
        {j.canManage && ended && (
          <Button onClick={() => again.mutate()} disabled={again.isPending} title="同样的命令、规模和来源，重新排队">
            重新提交
          </Button>
        )}
      </header>
      {error && <Notice>{error}</Notice>}
      {q.isError && <Notice tone="warm">刷新失败（{failure(q.error)}），显示的是上次的数据。</Notice>}
      <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-xs sm:grid-cols-[auto_1fr_auto_1fr]">
        <dt className="text-muted">用户</dt>
        <dd className="mono">{j.owner}</dd>
        {meta.map(([k, v]) => (
          <div key={k} className="contents">
            <dt className="text-muted">{k}</dt>
            <dd>{v}</dd>
          </div>
        ))}
      </dl>
      {(j.command || j.source) && (
        <Panel title="运行">
          <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 p-3 text-xs">
            {j.source && (
              <>
                <dt className="text-muted">来自</dt>
                <dd className="mono">{j.source.devpod ? `DevPod ${j.source.devpod}` : `配置 ${j.source.config ?? ""}`}</dd>
              </>
            )}
            {j.command && (
              <>
                <dt className="text-muted">命令</dt>
                <dd className="mono whitespace-pre-wrap break-all">{j.command}</dd>
              </>
            )}
            {j.workdir && (
              <>
                <dt className="text-muted">工作目录</dt>
                <dd className="mono">{j.workdir}</dd>
              </>
            )}
          </dl>
        </Panel>
      )}
      {(j.ranks ?? []).length > 0 && (
        <Panel title="各台">
          <div className="relative overflow-x-auto">
            <table aria-label="各台" className="w-full text-xs">
              <thead className="text-left text-muted">
                <tr>
                  <th className="px-3 py-1.5 font-normal">序号</th>
                  <th className="px-3 py-1.5 font-normal">机器</th>
                  <th className="px-3 py-1.5 font-normal">DevPod</th>
                  <th className="px-3 py-1.5 font-normal">状态</th>
                  <th className="px-3 py-1.5 font-normal">退出码</th>
                  <th className="px-3 py-1.5" />
                </tr>
              </thead>
              <tbody>
                {j.ranks!.map((r) => {
                  const up = running && r.phase !== "Succeeded" && r.phase !== "Failed";
                  return (
                    <tr key={r.rank} className="border-t border-line">
                      <td className="px-3 py-1.5">{r.rank}</td>
                      <td className="mono px-3 py-1.5">{r.node ?? "—"}</td>
                      <td className="mono px-3 py-1.5">
                        {r.devpod && up && j.canManage ? (
                          <Link to={`/devpods/${clusterSeg(j.cluster ?? cluster)}/${r.devpod}`} className="text-accent hover:underline">
                            {r.devpod}
                          </Link>
                        ) : (
                          (r.devpod ?? "—")
                        )}
                      </td>
                      <td className="px-3 py-1.5">{r.phase ? phaseLabel(r.phase) : "—"}</td>
                      <td className="mono px-3 py-1.5">{r.exitCode ?? "—"}</td>
                      <td className="px-3 py-1.5 text-right">
                        {up && j.canManage && r.devpod && me && (
                          <Button onClick={() => void copyText(sshCommand(me, j.owner, r.devpod!, j.cluster ?? cluster))} title={sshCommand(me, j.owner, r.devpod, j.cluster ?? cluster)}>
                            复制 SSH
                          </Button>
                        )}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </Panel>
      )}
      {j.canManage && (j.ranks ?? []).length > 0 && <RankLogs job={j} />}
      {me?.features.audit && (
        <Panel title="记录">
          <HistoryList queryKey={["job-history", j.id]} load={(cursor) => getJobHistory(j.id, cursor)} />
        </Panel>
      )}
      <ConfirmDialog
        open={confirming}
        onOpenChange={setConfirming}
        title={`取消任务 ${j.id}？`}
        confirmLabel="取消任务"
        danger
        onConfirm={() => {
          setConfirming(false);
          cancel.mutate();
        }}
      >
        {running ? "正在运行的各台会被停下，输出会保留。" : "它会离开队列。"}
      </ConfirmDialog>
    </div>
  );
}

/** RankLogs: one rank's output — followed live while the rank runs, else
 *  what was kept. */
function RankLogs({ job }: { job: Job }) {
  const [rank, setRank] = useState(0);
  // a job started more than once (preempted, queued again): each start's
  // output, the last one first
  const attempts = job.attempts ?? 1;
  const [attempt, setAttempt] = useState(attempts);
  const earlier = attempt < attempts;
  const r = job.ranks?.find((x) => x.rank === rank);
  const live = !earlier && (job.phase === "Running" || job.phase === "Starting") && r?.phase !== "Succeeded" && r?.phase !== "Failed";
  const [lines, setLines] = useState<string[]>([]);
  const [text, setText] = useState<string | null>(null);
  const [err, setErr] = useState("");
  const pre = useRef<HTMLPreElement>(null);
  const url = `/api/jobs/${job.id}/logs?rank=${rank}${earlier ? `&attempt=${attempt}` : ""}`;

  useEffect(() => {
    setLines([]);
    setText(null);
    setErr("");
    if (live && typeof EventSource !== "undefined") {
      // each connection replays the rank's output from its start
      const es = new EventSource(`${url}&follow=1`);
      es.onopen = () => setLines([]);
      es.onmessage = (m: MessageEvent<string>) => setLines((ls) => (ls.length >= MAX_LINES ? [...ls.slice(ls.length - MAX_LINES + 1), m.data] : [...ls, m.data]));
      return () => es.close();
    }
    let alive = true;
    void fetch(url).then(
      async (res) => {
        if (!alive) return;
        if (res.ok) setText(await res.text());
        else {
          const body = (await res.json().catch(() => ({}))) as { message?: string };
          setErr(body.message ?? res.statusText);
        }
      },
      (e: unknown) => alive && setErr(String(e)),
    );
    return () => {
      alive = false;
    };
  }, [url, live]);

  useEffect(() => {
    if (live && pre.current) pre.current.scrollTop = pre.current.scrollHeight;
  }, [lines, live]);

  const shown = live ? lines.join("\n") : (text ?? "");
  return (
    <Panel
      title="输出"
      actions={
        <>
          {attempts > 1 && (
            <Select aria-label="看第几次" value={String(attempt)} onChange={(e) => setAttempt(Number(e.target.value))} className="!h-7 text-xs">
              {Array.from({ length: attempts }, (_, i) => attempts - i).map((n) => (
                <option key={n} value={n}>
                  {n === attempts ? `第 ${n} 次（最近）` : `第 ${n} 次`}
                </option>
              ))}
            </Select>
          )}
          <Select aria-label="看第几台" value={String(rank)} onChange={(e) => setRank(Number(e.target.value))} className="!h-7 text-xs">
            {(job.ranks ?? []).map((x) => (
              <option key={x.rank} value={x.rank}>
                {`第 ${x.rank} 台${x.node ? `（${x.node}）` : ""}`}
              </option>
            ))}
          </Select>
          <a href={url} download={`${job.id}-${earlier ? `attempt-${attempt}-` : ""}rank-${rank}.log`} className="text-xs text-accent hover:underline">
            下载
          </a>
        </>
      }
    >
      {err ? (
        <p className="p-3 text-xs text-muted">{err}</p>
      ) : (
        <pre ref={pre} className="mono relative max-h-[60vh] min-h-24 overflow-auto whitespace-pre-wrap break-all p-3 text-[11px] leading-snug">
          {shown || (live ? "等待输出…" : text === null ? "加载中…" : "（没有输出）")}
        </pre>
      )}
    </Panel>
  );
}
