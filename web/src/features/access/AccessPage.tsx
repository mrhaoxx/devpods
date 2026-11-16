// 访问控制 (design 2026-11-16): the ordered rules — the first whose
// resource and subject match decides — the default last; a check of the
// running DevPods against them, with the ones found stopped on request.
import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { ColumnDef } from "@tanstack/react-table";
import { ApiFailure, patchDevPod } from "../../data/api";
import { keyOf } from "../../data/cluster";
import { Button } from "../../design/Button";
import { Notice } from "../../design/Form";
import { Panel } from "../../design/Panel";
import { EmptyState } from "../../design/EmptyState";
import { DataTable } from "../../design/DataTable";
import { BulkToast, type BulkOutcome, type BulkProgress } from "../../design/BulkToast";
import { runBulk } from "../../lib/bulk";
import { RuleDrawer } from "./RuleDrawer";
import { checkRunning, effectText, getAccess, listGroups, putAccess, resourceText, subjectText, type AccessPolicy, type AccessRule, type Violation } from "./api";

const newRule = (): AccessRule => ({ subject: { kind: "everyone" }, effect: { allow: false } });

const violationColumns: ColumnDef<Violation, unknown>[] = [
  { id: "devpod", header: "DevPod", accessorFn: (v) => v.devpod, meta: { priority: 1, width: 180, mono: true } },
  { id: "cluster", header: "集群", accessorFn: (v) => v.cluster ?? "", meta: { priority: 3, width: 90 } },
  { id: "owner", header: "Owner", accessorFn: (v) => v.owner, meta: { priority: 2, width: 100, mono: true } },
  { id: "node", header: "机器", accessorFn: (v) => v.node, meta: { priority: 2, width: 140, mono: true } },
  { id: "rule", header: "规则", accessorFn: (v) => (v.rule < 0 ? "默认" : `#${v.rule + 1}`), meta: { priority: 3, width: 60 } },
  { id: "reason", header: "原因", accessorFn: (v) => v.reason, meta: { priority: 2, width: 320 } },
];

export function AccessPage() {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["access"], queryFn: getAccess });
  const groups = useQuery({ queryKey: ["groups"], queryFn: listGroups }).data?.items ?? [];
  // the rules as being edited (null: as saved)
  const [draft, setDraft] = useState<AccessPolicy | null>(null);
  const [editing, setEditing] = useState<{ at: number | "new" | "default" } | null>(null);
  const [conflict, setConflict] = useState(false);
  const cur = draft ?? q.data;
  const save = useMutation({
    mutationFn: putAccess,
    onSuccess: (p) => {
      setDraft(null);
      setConflict(false);
      qc.setQueryData(["access"], p);
    },
    onError: (e) => {
      if (e instanceof ApiFailure && e.status === 409) {
        setConflict(true);
        setDraft(null);
        void q.refetch();
      }
    },
  });
  const change = (rules: AccessRule[], dflt = cur!.default) => setDraft({ ...cur!, rules, default: dflt });
  const move = (i: number, d: -1 | 1) => {
    const rules = [...cur!.rules];
    [rules[i], rules[i + d]] = [rules[i + d], rules[i]];
    change(rules);
  };

  const [found, setFound] = useState<Violation[] | null>(null);
  const check = useMutation({ mutationFn: checkRunning, onSuccess: (r) => setFound(r.items) });
  const [progress, setProgress] = useState<BulkProgress | null>(null);
  const [outcome, setOutcome] = useState<BulkOutcome | null>(null);
  const stop = async (vs: Violation[], clear: () => void) => {
    setOutcome(null);
    setProgress({ action: "停止", done: 0, failed: 0, total: vs.length });
    const results = await runBulk(vs, async (v) => {
      try {
        await patchDevPod({ cluster: v.cluster, name: v.devpod }, false);
        setProgress((p) => p && { ...p, done: p.done + 1 });
      } catch (e) {
        setProgress((p) => p && { ...p, done: p.done + 1, failed: p.failed + 1 });
        throw e;
      }
    });
    setProgress(null);
    setOutcome({ action: "停止", results: results.map((x) => ({ ...x, item: x.item.devpod })) });
    clear();
    check.mutate();
  };

  if (q.isError) return <EmptyState tone="error" title="无法加载访问规则" hint={String(q.error)} />;
  if (!cur) return <EmptyState title="加载中…" />;
  const editingRule = editing && (editing.at === "new" ? newRule() : editing.at === "default" ? { subject: { kind: "everyone" as const }, effect: cur.default } : cur.rules[editing.at]);
  return (
    <div className="space-y-3 p-3">
      <Panel
        title="访问规则（从上往下，第一条命中的说了算）"
        actions={
          <span className="inline-flex gap-2">
            <Button onClick={() => setEditing({ at: "new" })}>新增规则</Button>
            {draft && <Button onClick={() => setDraft(null)}>放弃修改</Button>}
            <Button variant="primary" disabled={!draft || save.isPending} onClick={() => draft && save.mutate(draft)}>
              保存
            </Button>
          </span>
        }
      >
        {conflict && (
          <div className="p-2">
            <Notice tone="warm">规则已被别人改过，已重新载入：请重新修改后再保存。</Notice>
          </div>
        )}
        {save.isError && !conflict && (
          <div className="p-2">
            <Notice>保存失败：{String(save.error)}</Notice>
          </div>
        )}
        <table className="w-full text-xs">
          <thead>
            <tr className="border-b border-line text-left text-[11px] text-muted">
              <th className="w-10 px-2 py-1">#</th>
              <th className="px-2">资源</th>
              <th className="px-2">主体</th>
              <th className="px-2">效果</th>
              <th className="w-48 px-2" />
            </tr>
          </thead>
          <tbody>
            {cur.rules.map((r, i) => (
              <tr key={i} data-testid="access-rule" className="border-b border-line">
                <td className="px-2 py-1 mono">{i + 1}</td>
                <td className="px-2 mono">{resourceText(r)}</td>
                <td className="px-2">{subjectText(r.subject, groups)}</td>
                <td className={`px-2 ${r.effect.allow ? "" : "text-fail"}`}>{effectText(r.effect)}</td>
                <td className="px-2 text-right whitespace-nowrap">
                  <Button aria-label={`上移 规则 ${i + 1}`} disabled={i === 0} onClick={() => move(i, -1)}>
                    ↑
                  </Button>{" "}
                  <Button aria-label={`下移 规则 ${i + 1}`} disabled={i === cur.rules.length - 1} onClick={() => move(i, 1)}>
                    ↓
                  </Button>{" "}
                  <Button onClick={() => setEditing({ at: i })}>编辑</Button>{" "}
                  <Button variant="danger" onClick={() => change(cur.rules.filter((_, j) => j !== i))}>
                    删除
                  </Button>
                </td>
              </tr>
            ))}
            <tr data-testid="access-rule" className="text-muted">
              <td className="px-2 py-1">默认</td>
              <td className="px-2">其余一切</td>
              <td className="px-2">所有人</td>
              <td className="px-2">{effectText(cur.default)}</td>
              <td className="px-2 text-right">
                <Button onClick={() => setEditing({ at: "default" })}>编辑</Button>
              </td>
            </tr>
          </tbody>
        </table>
        {draft && <p className="p-2 text-[11px] text-warm">有未保存的修改。</p>}
      </Panel>

      <Panel
        title="运行中的 DevPod"
        actions={
          <Button disabled={check.isPending} onClick={() => check.mutate()}>
            检查运行中的 DevPod
          </Button>
        }
      >
        {check.isError && (
          <div className="p-2">
            <Notice>检查失败：{String(check.error)}</Notice>
          </div>
        )}
        {found === null ? (
          <p className="p-3 text-xs text-muted">按当前（已保存的）规则，找出正在运行、但不该在那台机器上的 DevPod。只列出来，不会自动停。</p>
        ) : found.length === 0 ? (
          <EmptyState title="都合规" />
        ) : (
          <DataTable<Violation>
            label="不合规的 DevPod"
            data={found}
            columns={violationColumns}
            getRowId={(v) => keyOf({ cluster: v.cluster, name: v.devpod })}
            sorting={[]}
            onSortingChange={() => {}}
            selectable
            bulkBar={(picked, clear) => (
              <Button variant="danger" onClick={() => void stop(picked, clear)}>
                停止所选
              </Button>
            )}
          />
        )}
      </Panel>
      <BulkToast progress={progress} outcome={outcome} onClose={() => setOutcome(null)} />

      {editing && editingRule && (
        <RuleDrawer
          key={String(editing.at)}
          rule={editingRule}
          isDefault={editing.at === "default"}
          groups={groups}
          onClose={() => setEditing(null)}
          onSave={(r) => {
            if (editing.at === "default") change(cur.rules, r.effect);
            else if (editing.at === "new") change([...cur.rules, r]);
            else change(cur.rules.map((x, j) => (j === editing.at ? r : x)));
            setEditing(null);
          }}
        />
      )}
    </div>
  );
}
