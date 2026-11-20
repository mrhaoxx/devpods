// A condition (the access rules' and grants' language) as it is typed,
// with how to write one and what it names, previewed once typing pauses —
// the server reads it: the page has no parser of its own.
import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Field } from "../../design/Form";
import { useClusterList } from "../../data/clusterFilter";
import { useDebounced } from "../../lib/useDebounced";
import { previewMatch, type Group, type Preview } from "./api";

export type Condition = {
  text: string;
  setText: (v: string) => void;
  err?: { offset: number; message: string };
  hits: Exclude<Preview, { error: { offset: number; message: string } }> | null;
  /** the condition as typed is wrong */
  bad: boolean;
};

/** useCondition: a condition being typed (enabled: previewed at all). */
export function useCondition(initial: string, enabled = true): Condition {
  const [match, setMatch] = useState(initial);
  const [text, setText] = useDebounced(match, setMatch, 300);
  const preview = useQuery({ queryKey: ["access-preview", match], queryFn: () => previewMatch(match), enabled });
  const err = preview.data?.error;
  const hits = preview.data && !preview.data.error ? preview.data : null;
  return { text, setText, err, hits, bad: enabled && !!err && text === match };
}

export function ConditionField({ c, groups, hint, warnUnreservable }: { c: Condition; groups: Group[]; hint?: string; warnUnreservable?: boolean }) {
  const clusters = useClusterList().data?.items ?? [];
  const { err, hits } = c;
  const machines = (hits?.machines ?? []).reduce((a, m) => ({ count: a.count + m.count, maybe: a.maybe + m.maybe, unreservable: a.unreservable + m.unreservable }), { count: 0, maybe: 0, unreservable: 0 });
  return (
    <>
      <Field label="条件" hint={hint}>
        <textarea
          aria-label="条件"
          className="mono w-full rounded border border-line bg-surface px-2 py-1"
          rows={3}
          spellCheck={false}
          value={c.text}
          onChange={(ev) => c.setText(ev.target.value)}
        />
      </Field>
      <p className="text-[11px] text-muted">
        字段 <code>cluster</code> <code>machine</code> <code>user</code> <code>group</code>；运算 <code>=</code> <code>!=</code> <code>in (…)</code> <code>not in (…)</code>；组合{" "}
        <code>and</code> <code>or</code> <code>not</code> 和括号。名字可用 <code>*</code> 通配，机器还可以写标签 <code>key=value</code>。例：<code>cluster in (st2, a5)</code>、
        <code>cluster = st and machine != st5</code>、<code>everyone and not group = interns</code>
      </p>
      {(clusters.length > 0 || groups.length > 0) && (
        <p className="text-[11px] text-muted">
          {clusters.length > 0 && <>集群：{clusters.map((x) => x.name).join("、")}　</>}
          {groups.length > 0 && <>组：{groups.map((g) => g.name).join("、")}</>}
        </p>
      )}
      {err && (
        <p className="text-fail">
          第 {err.offset + 1} 个字符：{err.message}
        </p>
      )}
      {hits && (
        <p className="text-muted">
          命中集群 {hits.clusters.length ? hits.clusters.map((x) => (x.maybe ? `${x.name}（可能）` : x.name)).join("、") : "无"}；{machines.count} 台机器
          {machines.maybe > 0 && `（${machines.maybe} 台可能命中，取决于其他条件）`}；{hits.users.count} / {hits.users.total} 个用户
          {hits.users.maybe > 0 && `（${hits.users.maybe} 个可能命中，取决于集群和机器）`}，以后的新用户也按条件判断
          {warnUnreservable && machines.unreservable > 0 && <span className="text-warm">；其中 {machines.unreservable} 台没标可预约，需预约的人将用不了它们</span>}
        </p>
      )}
      {hits && hits.machines.length > 1 && (
        <p className="text-[11px] text-muted">
          各集群：
          {hits.machines.map((m, i) => (
            <span key={m.cluster ?? i} title={m.names.join(" ")}>
              {i > 0 && "、"}
              {`${m.cluster ?? ""} ${m.count} 台${m.maybe > 0 ? `（${m.maybe} 台可能）` : ""}`}
            </span>
          ))}
        </p>
      )}
    </>
  );
}
