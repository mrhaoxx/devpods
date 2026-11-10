// The bulk actions that need input (UI spec §5.1): migrate, collaborators,
// set image, and the redeploy confirmation. Each returns what to do per
// DevPod; the table runs it and shows the outcome.
import * as Dialog from "@radix-ui/react-dialog";
import { useState } from "react";
import type { SpecPatch } from "../../data/types";
import { useNodes } from "../../data/queries";
import { Button } from "../../design/Button";
import { Field, Input, Select } from "../../design/Form";

export type BulkKind = "migrate" | "collaborators" | "image" | "redeploy";
export type BulkTarget = { name: string; collaborators: string[] };
/** What a bulk action does to one DevPod: a spec patch, or a redeploy. */
export type BulkPlan = { action: string; patch?: (t: BulkTarget) => SpecPatch; redeploy?: boolean };

const TITLE: Record<BulkKind, string> = { migrate: "迁移", collaborators: "协作者", image: "设置镜像", redeploy: "重新部署" };
const names = (s: string) => [...new Set(s.split(/[\s,，]+/).filter(Boolean))];

/** collabPatch: add to / remove from / replace a DevPod's collaborators. */
export function collabPatch(mode: "add" | "remove" | "set", list: string[], current: string[]): string[] {
  if (mode === "set") return list;
  if (mode === "remove") return current.filter((c) => !list.includes(c));
  return [...current, ...list.filter((c) => !current.includes(c))];
}

function MigrateBody({ node, setNode }: { node: string; setNode: (n: string) => void }) {
  const nodes = useNodes().data?.items ?? [];
  return (
    <>
      <Field label="目标节点" hint="DevPod 会固定到该节点；运行中的会在新节点上重建，SSH 会话会断开。home 卷不变。">
        <Select aria-label="目标节点" value={node} onChange={(e) => setNode(e.target.value)}>
          <option value="">不指定（由调度器选择）</option>
          {nodes.map((n) => (
            <option key={n.name} value={n.name} disabled={!n.ready}>
              {n.name}
              {n.ready ? "" : "（未就绪）"}
            </option>
          ))}
        </Select>
      </Field>
    </>
  );
}

export function BulkDialog({ kind, count, onClose, onRun }: { kind: BulkKind | null; count: number; onClose: () => void; onRun: (p: BulkPlan) => void }) {
  const [node, setNode] = useState("");
  const [image, setImage] = useState("");
  const [users, setUsers] = useState("");
  const [mode, setMode] = useState<"add" | "remove" | "set">("add");
  if (!kind) return null;
  const list = names(users);
  const ok = kind === "image" ? image.trim() !== "" : kind === "collaborators" ? list.length > 0 || mode === "set" : true;
  const plan = (): BulkPlan =>
    kind === "migrate"
      ? { action: node ? `迁移到 ${node}` : "取消固定节点", patch: () => ({ node }) }
      : kind === "image"
        ? { action: "设置镜像", patch: () => ({ image: image.trim() }) }
        : kind === "collaborators"
          ? { action: "协作者", patch: (t) => ({ collaborators: collabPatch(mode, list, t.collaborators) }) }
          : { action: "重新部署", redeploy: true };
  return (
    <Dialog.Root open onOpenChange={(o) => !o && onClose()}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/30" />
        <Dialog.Content aria-describedby={undefined} className="fixed left-1/2 top-1/3 z-50 w-[min(92vw,440px)] -translate-x-1/2 space-y-3 rounded-lg border border-line bg-surface p-4 shadow-xl">
          <Dialog.Title className="text-sm font-semibold">
            {TITLE[kind]}：{count} 个 DevPod
          </Dialog.Title>
          {kind === "migrate" && <MigrateBody node={node} setNode={setNode} />}
          {kind === "image" && (
            <Field label="镜像" hint="运行中的 DevPod 会用新镜像重建，SSH 会话会断开。">
              <Input aria-label="镜像" className="mono" value={image} onChange={(e) => setImage(e.target.value)} placeholder="ubuntu:24.04" autoFocus />
            </Field>
          )}
          {kind === "collaborators" && (
            <>
              <div role="radiogroup" aria-label="方式" className="inline-flex rounded-md border border-line bg-sunk p-0.5 text-xs">
                {(
                  [
                    ["add", "添加"],
                    ["remove", "移除"],
                    ["set", "替换为"],
                  ] as const
                ).map(([id, label]) => (
                  <button key={id} type="button" role="radio" aria-checked={mode === id} onClick={() => setMode(id)} className={`rounded px-2.5 py-0.5 ${mode === id ? "bg-surface font-medium shadow-sm" : "text-muted hover:text-ink"}`}>
                    {label}
                  </button>
                ))}
              </div>
              <Field label="用户名" hint={mode === "set" ? "用逗号或空格分隔；留空表示清空协作者。" : "用逗号或空格分隔。不会重建 DevPod。"}>
                <Input aria-label="用户名" className="mono" value={users} onChange={(e) => setUsers(e.target.value)} placeholder="alice, bob" autoFocus />
              </Field>
            </>
          )}
          {kind === "redeploy" && <p className="text-xs text-muted">删除并重建所选 DevPod 的 Pod：进程和 SSH 会话会中断，home 卷保留。未运行的 DevPod 会被跳过并报错。</p>}
          <div className="flex justify-end gap-2">
            <Dialog.Close asChild>
              <Button>取消</Button>
            </Dialog.Close>
            <Button variant="primary" disabled={!ok} onClick={() => onRun(plan())}>
              {kind === "redeploy" ? "确认重新部署" : "应用"}
            </Button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
