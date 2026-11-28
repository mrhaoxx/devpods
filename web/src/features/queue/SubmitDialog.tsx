// 提交任务 (design 2026-11-26): what a job is made from, what it runs, how
// big, for how long, at what priority — and, before it is sent, where it
// would stand in the queue.
import { useEffect, useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "react-router-dom";
import * as Dialog from "@radix-ui/react-dialog";
import { ApiFailure, jobLimits, jobsFree, listDevPods, submitJob, type JobEstimate, type JobRequest } from "../../data/api";
import { clusterSeg, keyOf } from "../../data/cluster";
import { useClusterList } from "../../data/clusterFilter";
import { listConfigs } from "../configs/api";
import { Button } from "../../design/Button";
import { Field, Input, Notice, Select, Textarea } from "../../design/Form";
import { whenText } from "./jobs";

const CARDS = [8, 4, 2, 1, 0] as const;

/** vars: K=V lines as a map (blank lines and those without = are left out). */
function varsOf(text: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const line of text.split("\n")) {
    const i = line.indexOf("=");
    if (i > 0) out[line.slice(0, i).trim()] = line.slice(i + 1).trim();
  }
  return out;
}

const failure = (e: unknown) => (e instanceof ApiFailure ? e.body.message : String(e));

export function SubmitDialog({ onClose }: { onClose(): void }) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const devpods = useQuery({ queryKey: ["devpods", "mine"], queryFn: () => listDevPods("mine") });
  const configs = useQuery({ queryKey: ["configs", "all"], queryFn: () => listConfigs() });
  const clusterList = useClusterList().data?.items ?? [];
  const jobClusters = clusterList.filter((c) => c.capabilities?.includes("jobs"));

  const [from, setFrom] = useState(""); // devpod:<key> | config:<name>
  const [configCluster, setConfigCluster] = useState("");
  const [vars, setVars] = useState("");
  const [command, setCommand] = useState("");
  const [workdir, setWorkdir] = useState("");
  const [title, setTitle] = useState("");
  const [nodes, setNodes] = useState("1");
  const [perNode, setPerNode] = useState("1");
  const [gpus, setGpus] = useState("8");
  const [model, setModel] = useState("");
  const [cpus, setCpus] = useState("");
  const [hours, setHours] = useState("");
  const [priority, setPriority] = useState("0");
  const [preemptible, setPreemptible] = useState(false);
  const [requeue, setRequeue] = useState(true);
  const [hostNetwork, setHostNetwork] = useState(false);
  const [privileged, setPrivileged] = useState(false);
  const [error, setError] = useState("");

  // the DevPods a job can be made from: mine, not a job's rank
  const sources = (devpods.data?.items ?? []).filter((d) => !d.spec.job);
  const kind = from.slice(0, from.indexOf(":"));
  const ref = from.slice(from.indexOf(":") + 1);
  const source = kind === "devpod" ? sources.find((d) => keyOf({ cluster: d.cluster, name: d.metadata.name }) === ref) : undefined;
  const cluster = kind === "devpod" ? source?.cluster : configCluster || (jobClusters.length === 1 ? jobClusters[0].name : undefined);

  const limits = useQuery({ queryKey: ["job-limits", cluster ?? ""], queryFn: () => jobLimits(cluster) });
  // the GPU models its cluster has (the queue's free cards say them)
  const free = useQuery({ queryKey: ["jobs-free"], queryFn: jobsFree });
  const freeItems = free.data?.items ?? [];
  const models = (freeItems.length === 1 ? freeItems[0] : freeItems.find((f) => f.cluster === cluster))?.models ?? [];
  // a model chosen is the job's only while its cluster has it (another source, another cluster: none)
  const chosenModel = models.some((m) => m.model === model) ? model : "";
  const lim = limits.data;
  useEffect(() => {
    if (lim && hours === "") setHours(String(lim.defaultTimeLimitSeconds / 3600));
  }, [lim, hours]);

  const n = Number(nodes);
  const k = Number(perNode);
  const g = Number(gpus);
  const c = cpus.trim() === "" ? undefined : Number(cpus);
  const h = Number(hours);
  const p = Number(priority);
  const overCap = !!lim && !lim.unlimitedPriority && p > lim.maxPriority;
  const problem =
    !from
      ? "选择任务来自哪个 DevPod 或配置"
      : !command.trim()
        ? "填写要运行的命令"
        : !(Number.isInteger(n) && n >= 1)
          ? "台数至少 1"
          : !(Number.isInteger(k) && k >= 1)
            ? "每台任务数至少 1"
          : !(Number.isInteger(g) && g >= 0)
            ? "每台卡数是个整数"
            : g === 0 && c === undefined
              ? "不要卡时请填每台核数"
              : c !== undefined && !(c > 0)
                ? "每台核数要大于 0"
                : !(h > 0)
                  ? "填写时长上限"
                  : lim && h * 3600 > lim.maxTimeLimitSeconds
                    ? `时长上限最长 ${lim.maxTimeLimitSeconds / 3600} 小时`
                    : !Number.isInteger(p)
                      ? "优先级是个整数"
                      : overCap
                        ? "优先级超出上限"
                        : kind === "config" && jobClusters.length > 1 && !configCluster
                          ? "选择集群"
                          : "";

  const body = useMemo((): JobRequest | undefined => {
    if (!from) return undefined;
    const b: JobRequest = {
      from: kind === "devpod" ? { devpod: source?.metadata.name ?? ref } : { config: ref, ...(Object.keys(varsOf(vars)).length ? { vars: varsOf(vars) } : {}) },
      command: command.trim(),
      nodes: n,
      gpus: g,
      timeLimitSeconds: Math.round(h * 3600),
      priority: p,
    };
    if (k > 1) b.tasksPerNode = k;
    if (chosenModel && g > 0) b.gpuModel = chosenModel;
    if (c !== undefined) b.cpus = c;
    if (preemptible) b.preemptible = true;
    if (!requeue) b.requeue = false;
    if (hostNetwork || privileged) b.privileges = { ...(hostNetwork ? { hostNetwork: true } : {}), ...(privileged ? { privileged: true } : {}) };
    if (workdir.trim()) b.workdir = workdir.trim();
    if (title.trim()) b.title = title.trim();
    return b;
  }, [from, kind, source, ref, vars, command, n, k, g, chosenModel, c, h, p, preemptible, requeue, hostNetwork, privileged, workdir, title]);

  // where it would stand: asked again a moment after the form settles
  const [estimate, setEstimate] = useState<JobEstimate | undefined>();
  const [estimateError, setEstimateError] = useState("");
  const asked = problem && problem !== "优先级超出上限" ? undefined : body;
  const key = JSON.stringify(asked ?? null);
  useEffect(() => {
    setEstimate(undefined);
    setEstimateError("");
    if (!asked) return;
    let live = true;
    const t = setTimeout(() => {
      submitJob(cluster, { ...asked, dryRun: true }).then(
        (e) => live && setEstimate(e),
        (e: unknown) => live && setEstimateError(failure(e)),
      );
    }, 300);
    return () => {
      live = false;
      clearTimeout(t);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps -- key stands for asked
  }, [key, cluster]);

  const save = useMutation({
    mutationFn: () => submitJob(cluster, body!),
    onSuccess: (j) => {
      void qc.invalidateQueries({ queryKey: ["jobs"] });
      onClose();
      navigate(`/queue/${clusterSeg(j.cluster ?? cluster)}/${j.id}`);
    },
    onError: (e) => setError(failure(e)),
  });

  return (
    <Dialog.Root open onOpenChange={(o) => !o && onClose()}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/30" />
        <Dialog.Content
          aria-describedby={undefined}
          className="fixed left-1/2 top-4 z-50 max-h-[calc(100dvh-2rem)] w-[min(92vw,480px)] -translate-x-1/2 space-y-3 overflow-y-auto rounded-lg border border-line bg-surface p-4 shadow-xl sm:top-[8vh] sm:max-h-[84vh]"
        >
          <Dialog.Title className="text-sm font-semibold">提交任务</Dialog.Title>
          <form
            className="space-y-3"
            onSubmit={(ev) => {
              ev.preventDefault();
              if (problem || !body) return;
              setError("");
              save.mutate();
            }}
          >
            <Field label="来自" hint="任务的每一台都按它的镜像、挂载和环境创建">
              <Select aria-label="来自" value={from} onChange={(e) => setFrom(e.target.value)}>
                <option value="">选择…</option>
                {sources.length > 0 && (
                  <optgroup label="我的 DevPod">
                    {sources.map((d) => (
                      <option key={keyOf({ cluster: d.cluster, name: d.metadata.name })} value={`devpod:${keyOf({ cluster: d.cluster, name: d.metadata.name })}`}>
                        {`DevPod：${d.metadata.name}${d.cluster && clusterList.length > 1 ? `（${d.cluster}）` : ""}`}
                      </option>
                    ))}
                  </optgroup>
                )}
                {(configs.data?.items ?? []).length > 0 && (
                  <optgroup label="配置">
                    {configs.data!.items.map((cf) => (
                      <option key={cf.name} value={`config:${cf.name}`}>
                        {`配置：${cf.title || cf.name}`}
                      </option>
                    ))}
                  </optgroup>
                )}
              </Select>
            </Field>
            {kind === "config" && jobClusters.length > 1 && (
              <Field label="集群">
                <Select aria-label="集群" value={configCluster} onChange={(e) => setConfigCluster(e.target.value)}>
                  <option value="">选择…</option>
                  {jobClusters.map((cl) => (
                    <option key={cl.name} value={cl.name}>
                      {cl.name}
                    </option>
                  ))}
                </Select>
              </Field>
            )}
            {kind === "config" && (
              <Field label="变量（选填）" hint="每行一个 KEY=值">
                <Textarea aria-label="变量" rows={2} className="mono text-xs" value={vars} onChange={(e) => setVars(e.target.value)} />
              </Field>
            )}
            <Field label="命令">
              <Textarea aria-label="命令" rows={2} className="mono text-xs" placeholder="torchrun --nnodes $WORLD_SIZE --node-rank $RANK … train.py" value={command} onChange={(e) => setCommand(e.target.value)} />
            </Field>
            <p className="-mt-2 text-[11px] text-faint">
              每台都在登录 bash 里运行，带 <span className="mono">RANK</span>、<span className="mono">WORLD_SIZE</span>、<span className="mono">MASTER_ADDR</span>、
              <span className="mono">MASTER_PORT</span>；所有台都结束任务才结束。
            </p>
            <div className="grid grid-cols-3 gap-3">
              <Field label="台数">
                <Input aria-label="台数" type="number" min={1} value={nodes} onChange={(e) => setNodes(e.target.value)} />
              </Field>
              <Field label="每台任务数" hint="几个 rank 共用一台">
                <Input aria-label="每台任务数" type="number" min={1} value={perNode} onChange={(e) => setPerNode(e.target.value)} />
              </Field>
              <Field label="时长上限（小时）" hint={lim ? `最长 ${lim.maxTimeLimitSeconds / 3600}` : undefined}>
                <Input aria-label="时长上限（小时）" type="number" min={0} step="any" value={hours} onChange={(e) => setHours(e.target.value)} />
              </Field>
            </div>
            <div>
              <span className="mb-1 block text-xs font-medium text-ink">{k > 1 ? "每个任务" : "每台"}</span>
              <div className="flex flex-wrap items-center gap-2">
                <div role="radiogroup" aria-label="每台卡数" className="inline-flex rounded-md border border-line bg-sunk p-0.5 text-xs">
                  {CARDS.map((k) => (
                    <button
                      key={k}
                      type="button"
                      role="radio"
                      aria-checked={g === k}
                      onClick={() => setGpus(String(k))}
                      className={`rounded px-2.5 py-0.5 ${g === k ? "bg-surface font-medium shadow-sm" : "text-muted hover:text-ink"}`}
                    >
                      {k ? `${k} 卡` : "不要卡"}
                    </button>
                  ))}
                </div>
                <Input aria-label="每台卡数（自填）" type="number" min={0} value={gpus} onChange={(e) => setGpus(e.target.value)} className="!h-7 !w-16 text-xs" />
                <Input
                  aria-label="每台核数"
                  type="number"
                  min={0}
                  step="any"
                  value={cpus}
                  placeholder={estimate?.cpus ? `${estimate.cpus} 核` : "核数"}
                  onChange={(e) => setCpus(e.target.value)}
                  className="!h-7 !w-24 text-xs"
                />
              </div>
              <p className="mt-1 text-[11px] text-faint">核数留空时按卡数占机器的比例给。</p>
            </div>
            {g > 0 && models.length > 0 && (
              <Field label="GPU 型号">
                <Select aria-label="GPU 型号" value={chosenModel} onChange={(e) => setModel(e.target.value)}>
                  <option value="">任意</option>
                  {models.map((m) => (
                    <option key={m.model} value={m.model}>
                      {`${m.model}（空 ${m.gpu} / 共 ${m.total} 卡）`}
                    </option>
                  ))}
                </Select>
              </Field>
            )}
            <Field label="优先级">
              <Input aria-label="优先级" type="number" value={priority} onChange={(e) => setPriority(e.target.value)} />
            </Field>
            {lim && (
              <p className={`-mt-2 text-[11px] ${overCap ? "text-fail" : "text-faint"}`}>
                {lim.unlimitedPriority ? "数字越大越先运行；你的优先级不受限。" : overCap ? `超出上限：你最高可填 ${lim.maxPriority}` : `数字越大越先运行；你最高可填 ${lim.maxPriority}`}
              </p>
            )}
            <label className="flex items-start gap-2 text-xs">
              <input type="checkbox" className="mt-0.5" checked={preemptible} onChange={(e) => setPreemptible(e.target.checked)} />
              <span>
                可被抢占
                <span className="block text-[11px] text-faint">通常更早开始；但更高优先级的任务或节点预约需要这些资源时，它会被停下并重新排队。</span>
              </span>
            </label>
            <label className="flex items-start gap-2 text-xs">
              <input type="checkbox" className="mt-0.5" checked={requeue} onChange={(e) => setRequeue(e.target.checked)} />
              <span>
                被停时重新排队
                <span className="block text-[11px] text-faint">被抢占、被预约清场或机器关闭时：勾选则回到队列原来的位置再跑，不勾则直接结束。</span>
              </span>
            </label>
            <details className="text-xs">
              <summary className="cursor-pointer text-muted">高级</summary>
              <div className="mt-2 space-y-2">
                <label className="flex items-center gap-2">
                  <input type="checkbox" checked={hostNetwork} onChange={(e) => setHostNetwork(e.target.checked)} />
                  使用主机网络（MASTER_ADDR 为第 0 台的节点地址）
                </label>
                <label className="flex items-center gap-2">
                  <input type="checkbox" checked={privileged} onChange={(e) => setPrivileged(e.target.checked)} />
                  特权容器
                </label>
                <Field label="工作目录（选填）">
                  <Input aria-label="工作目录" className="mono text-xs" placeholder="留空：镜像的默认目录；最好在多台机器共享的存储上，如你的 home" value={workdir} onChange={(e) => setWorkdir(e.target.value)} />
                </Field>
                <Field label="名称（选填）">
                  <Input aria-label="名称" value={title} onChange={(e) => setTitle(e.target.value)} />
                </Field>
              </div>
            </details>
            {estimate && (
              <p className="text-[11px] text-muted">
                {estimate.startsNow
                  ? "提交前预估：现在就能开始。"
                  : `提交前预估：排第 ${estimate.position ?? "?"} 位${estimate.expectedStart ? `，预计 ${whenText(estimate.expectedStart, Date.now())} 前后开始` : ""}。`}
              </p>
            )}
            {estimate?.warnings?.map((w) => (
              <Notice key={w} tone="warm">
                提醒：{w}
              </Notice>
            ))}
            {estimateError && <Notice tone="warm">{estimateError}</Notice>}
            {error && <Notice>{error}</Notice>}
            <div className="flex items-center justify-end gap-2">
              {problem && <span className="mr-auto text-[11px] text-faint">{problem}</span>}
              <Dialog.Close asChild>
                <Button type="button">取消</Button>
              </Dialog.Close>
              <Button type="submit" variant="primary" disabled={!!problem || save.isPending}>
                提交
              </Button>
            </div>
          </form>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
