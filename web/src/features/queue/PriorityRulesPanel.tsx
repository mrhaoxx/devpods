// 任务规则 (design 2026-11-26): how high a priority each user may give a
// job — the highest of the rules whose condition holds for them (none: 0)
// — and how many jobs they may have waiting or running in a cluster — the
// most of those that hold and say (none saying: the console's default);
// admins are not held. Conditions are written as access rules are.
import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiFailure, getPriorityRules, putPriorityRules, type PriorityRule } from "../../data/api";
import { Button } from "../../design/Button";
import { Input, Notice } from "../../design/Form";
import { Panel } from "../../design/Panel";

export function PriorityRulesPanel() {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["job-priority-rules"], queryFn: getPriorityRules });
  const [draft, setDraft] = useState<PriorityRule[] | null>(null);
  const [error, setError] = useState<{ message: string; index?: number } | null>(null);
  useEffect(() => {
    if (q.data && draft === null) setDraft(q.data.items);
  }, [q.data, draft]);
  const rules = draft ?? q.data?.items ?? [];
  const dirty = !!q.data && JSON.stringify(rules) !== JSON.stringify(q.data.items);
  const set = (i: number, r: Partial<PriorityRule>) => setDraft(rules.map((x, j) => (j === i ? { ...x, ...r } : x)));

  const save = useMutation({
    mutationFn: () => putPriorityRules(rules),
    onSuccess: (v) => {
      setError(null);
      qc.setQueryData(["job-priority-rules"], v);
      setDraft(v.items);
    },
    onError: (e) => {
      if (e instanceof ApiFailure) {
        const d = e.body.detail as { index?: number } | undefined;
        setError({ message: e.body.message, index: d?.index });
      } else setError({ message: String(e) });
    },
  });

  return (
    <Panel
      title="任务规则"
      actions={
        <>
          <Button onClick={() => setDraft([...rules, { condition: "", maxPriority: 0, maxJobs: null }])}>添加规则</Button>
          <Button variant="primary" disabled={!dirty || save.isPending} onClick={() => save.mutate()}>
            保存
          </Button>
        </>
      }
    >
      <p className="p-3 pb-1 text-[11px] text-muted">
        提交任务时优先级最高能填多少：取条件成立的规则里最大的「最高优先级」，一条都不成立时为 0。同一集群里最多能有几个排队或运行中的任务：取条件成立且填了「最多任务数」的规则里最大的，都没填时用控制台的默认值。管理员都不受限。条件写法同上面的访问规则（
        <code>user</code> <code>group</code> <code>cluster</code>），例：<code>group = ml</code>、<code>cluster = st and user = alice</code>。
      </p>
      {q.isError && (
        <div className="p-2">
          <Notice>读不到优先级规则：{String(q.error)}</Notice>
        </div>
      )}
      <div className="space-y-2 p-3">
        {rules.length === 0 && q.data && <p className="text-xs text-muted">还没有规则：除管理员外，所有人只能用优先级 0，任务数用默认上限。</p>}
        {rules.map((r, i) => (
          <div key={i} className={`flex flex-wrap items-center gap-2 ${error?.index === i ? "rounded ring-1 ring-fail" : ""}`}>
            <Input aria-label={`规则 ${i + 1} 条件`} className="mono min-w-0 flex-1 text-xs" placeholder="group = ml" spellCheck={false} value={r.condition} onChange={(e) => set(i, { condition: e.target.value })} />
            <Input
              aria-label={`规则 ${i + 1} 最高优先级`}
              type="number"
              className="w-24 text-xs"
              value={String(r.maxPriority)}
              onChange={(e) => set(i, { maxPriority: Number(e.target.value) || 0 })}
            />
            <Input
              aria-label={`规则 ${i + 1} 最多任务数`}
              type="number"
              min={1}
              className="w-24 text-xs"
              placeholder="默认"
              title="同一集群里最多几个排队或运行中的任务；留空：这条规则不管任务数"
              value={r.maxJobs == null ? "" : String(r.maxJobs)}
              onChange={(e) => set(i, { maxJobs: e.target.value === "" ? null : Math.max(1, Math.floor(Number(e.target.value)) || 1) })}
            />
            <Button aria-label={`删除规则 ${i + 1}`} onClick={() => setDraft(rules.filter((_, j) => j !== i))}>
              删除
            </Button>
          </div>
        ))}
        {error && <Notice>{error.message}</Notice>}
        {dirty && <p className="text-[11px] text-warm">有未保存的修改。</p>}
      </div>
    </Panel>
  );
}
