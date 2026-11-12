// The bulk actions that need input (UI spec §5.1): migrate, collaborators,
// set image, and the redeploy confirmation. Each returns what to do per
// DevPod; the table runs it and shows the outcome.
import * as Dialog from "@radix-ui/react-dialog";
import { useState } from "react";
import type { SpecPatch } from "../../data/types";
import { useNodeChoices } from "../../data/queries";
import { fmtTime } from "../../lib/reservations";
import { MY_IMAGES_LIST, MyImagesList } from "../images/MyImages";
import { Button } from "../../design/Button";
import { Field, Input } from "../../design/Form";

export type BulkKind = "migrate" | "collaborators" | "image" | "redeploy";
export type BulkTarget = { name: string; collaborators: string[] };
/** What a bulk action does to one DevPod: a spec patch, or a redeploy. */
export type BulkPlan = { action: string; patch?: (t: BulkTarget, index: number) => SpecPatch; redeploy?: boolean };

const TITLE: Record<BulkKind, string> = { migrate: "迁移", collaborators: "协作者", image: "设置镜像", redeploy: "重新部署" };
const names = (s: string) => [...new Set(s.split(/[\s,，]+/).filter(Boolean))];

/** collabPatch: add to / remove from / replace a DevPod's collaborators. */
export function collabPatch(mode: "add" | "remove" | "set", list: string[], current: string[]): string[] {
  if (mode === "set") return list;
  if (mode === "remove") return current.filter((c) => !list.includes(c));
  return [...current, ...list.filter((c) => !current.includes(c))];
}

/** migrateTargets pairs DevPods with nodes: one node takes them all;
 *  otherwise there must be exactly one node per DevPod, in the order
 *  both were picked. null = the counts do not line up. */
export function migrateTargets(devpods: string[], nodes: string[]): [string, string][] | null {
  if (nodes.length === 1) return devpods.map((d) => [d, nodes[0]]);
  if (nodes.length === devpods.length && nodes.length > 0) return devpods.map((d, i) => [d, nodes[i]]);
  return null;
}

/** OrderedNodes: pick nodes one after another; the order of the clicks is
 *  kept and shown. A node someone else holds now cannot be picked. */
export function OrderedNodes({ picked, setPicked, disabled, legend = "目标节点（按点击顺序）" }: { picked: string[]; setPicked: (n: string[]) => void; disabled?: boolean; legend?: string }) {
  const nodes = useNodeChoices().data?.items ?? [];
  return (
    <fieldset disabled={disabled} className={disabled ? "opacity-40" : ""}>
      <legend className="mb-1 font-medium">{legend}</legend>
      <div className="flex flex-wrap gap-1.5">
        {nodes.map((n) => {
          const at = picked.indexOf(n.name);
          return (
            <button
              key={n.name}
              type="button"
              disabled={!n.selectable}
              aria-pressed={at >= 0}
              title={!n.selectable && n.held ? `${n.held.user} 预约至 ${fmtTime(n.held.until)}` : n.next ? `将于 ${fmtTime(n.next.start)} 被 ${n.next.user} 预约，届时自动休眠` : n.ready ? undefined : "节点未就绪"}
              onClick={() => setPicked(at >= 0 ? picked.filter((x) => x !== n.name) : [...picked, n.name])}
              className={`mono inline-flex items-center gap-1 rounded-md border px-2 py-0.5 disabled:opacity-40 ${at >= 0 ? "border-accent bg-accent-soft text-accent" : "border-line text-muted hover:text-ink"}`}
            >
              {at >= 0 && <span className="rounded bg-accent px-1 text-[10px] text-white">{at + 1}</span>}
              {n.name}
              {n.gpuTotal > 0 && (
                <span className="font-sans text-[10px]" title="空闲 / 总数">
                  {n.gpuModel ?? "GPU"} {n.gpuTotal - n.gpuUsed}/{n.gpuTotal}
                </span>
              )}
              {!n.selectable && <span className="font-sans text-[10px]">已被预约</span>}
              {n.selectable && !n.ready && <span className="font-sans text-[10px]">未就绪</span>}
            </button>
          );
        })}
        {nodes.length === 0 && <span className="text-faint">没有可选节点</span>}
      </div>
    </fieldset>
  );
}

function MigrateBody({ devpods, picked, setPicked, unpin, setUnpin }: { devpods: string[]; picked: string[]; setPicked: (n: string[]) => void; unpin: boolean; setUnpin: (b: boolean) => void }) {
  const pairs = migrateTargets(devpods, picked);
  return (
    <div className="space-y-2 text-xs">
      <p className="text-muted">
        选 1 个节点：全部迁到它。选多个：数量要和 DevPod 相同，按勾选顺序一一对应。运行中的 DevPod 会在目标节点上重建，SSH 会话会断开；home 卷不变。
      </p>
      <OrderedNodes picked={picked} setPicked={setPicked} disabled={unpin} />
      {!unpin && picked.length > 0 && !pairs && (
        <p role="alert" className="text-fail">
          已选 {picked.length} 个节点、{devpods.length} 个 DevPod：节点数要么是 1，要么等于 DevPod 数。
        </p>
      )}
      {!unpin && pairs && (
        <ol aria-label="迁移计划" className="mono max-h-40 space-y-0.5 overflow-y-auto rounded-md border border-line bg-sunk p-2">
          {pairs.map(([d, n]) => (
            <li key={d}>
              {d} → {n}
            </li>
          ))}
        </ol>
      )}
      <label className="flex items-center gap-1.5">
        <input type="checkbox" checked={unpin} onChange={(e) => setUnpin(e.target.checked)} />
        取消固定节点（交给调度器选择）
      </label>
    </div>
  );
}

export function BulkDialog({ kind, targets, onClose, onRun }: { kind: BulkKind | null; targets: BulkTarget[]; onClose: () => void; onRun: (p: BulkPlan) => void }) {
  const [nodes, setNodes] = useState<string[]>([]);
  const [unpin, setUnpin] = useState(false);
  const count = targets.length;
  const pairs = migrateTargets(targets.map((t) => t.name), nodes);
  const [image, setImage] = useState("");
  const [users, setUsers] = useState("");
  const [mode, setMode] = useState<"add" | "remove" | "set">("add");
  if (!kind) return null;
  const list = names(users);
  const ok = kind === "image" ? image.trim() !== "" : kind === "collaborators" ? list.length > 0 || mode === "set" : kind === "migrate" ? unpin || !!pairs : true;
  const plan = (): BulkPlan =>
    kind === "migrate"
      ? unpin
        ? { action: "取消固定节点", patch: () => ({ node: "" }) }
        : { action: nodes.length === 1 ? `迁移到 ${nodes[0]}` : "迁移", patch: (_t, i) => ({ node: pairs![i][1] }) }
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
          {kind === "migrate" && <MigrateBody devpods={targets.map((t) => t.name)} picked={nodes} setPicked={setNodes} unpin={unpin} setUnpin={setUnpin} />}
          {kind === "image" && (
            <Field label="镜像" hint="运行中的 DevPod 会用新镜像重建，SSH 会话会断开。">
              <Input aria-label="镜像" className="mono" value={image} onChange={(e) => setImage(e.target.value)} placeholder="ubuntu:24.04" list={MY_IMAGES_LIST} autoFocus />
              <MyImagesList />
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
