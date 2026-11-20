// 管理授权 (design 2026-11-20): grants of switching machines reservable,
// seeing the audit and cutting sessions, by a condition on the cluster,
// machine, user and group — each its own, saved on its own.
import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Drawer } from "../../design/Drawer";
import { Button } from "../../design/Button";
import { Field, Input, Notice } from "../../design/Form";
import { Panel } from "../../design/Panel";
import { ConditionField, useCondition } from "./ConditionField";
import { createGrant, deleteGrant, GRANT_PERMISSIONS, grantText, listGrants, updateGrant, type Grant, type GrantPermission, type Group } from "./api";

export function GrantsPanel({ groups }: { groups: Group[] }) {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["grants"], queryFn: listGrants });
  const [editing, setEditing] = useState<Grant | "new" | null>(null);
  const del = useMutation({ mutationFn: deleteGrant, onSettled: () => qc.invalidateQueries({ queryKey: ["grants"] }) });
  const items = q.data?.items ?? [];
  const broken = items.filter((g) => g.problem);
  return (
    <Panel
      title="管理授权（任一条成立即授予，没有先后）"
      actions={<Button onClick={() => setEditing("new")}>新增授权</Button>}
    >
      {broken.length > 0 && (
        <div className="p-2">
          <Notice>
            这些授权读不懂或点名了不存在的组，现在什么都不授予，请修改：
            {broken.map((g) => (
              <span key={g.name} className="block">
                {g.name}：{g.problem}
              </span>
            ))}
          </Notice>
        </div>
      )}
      {del.isError && (
        <div className="p-2">
          <Notice>删除失败：{String(del.error)}</Notice>
        </div>
      )}
      <table className="w-full text-xs">
        <thead>
          <tr className="border-b border-line text-left text-[11px] text-muted">
            <th className="px-2 py-1">名字</th>
            <th className="px-2">条件</th>
            <th className="px-2">授予</th>
            <th className="w-32 px-2" />
          </tr>
        </thead>
        <tbody>
          {items.map((g) => (
            <tr key={g.name} data-testid="grant" className={`border-b border-line ${g.problem ? "text-fail" : ""}`} title={g.description}>
              <td className="mono px-2 py-1">{g.name}</td>
              <td className="mono px-2">{g.match}</td>
              <td className="px-2">{grantText(g.permissions)}</td>
              <td className="px-2 text-right whitespace-nowrap">
                <Button onClick={() => setEditing(g)}>编辑</Button>{" "}
                <Button variant="danger" disabled={del.isPending} onClick={() => del.mutate(g.name)}>
                  删除
                </Button>
              </td>
            </tr>
          ))}
          {items.length === 0 && (
            <tr>
              <td colSpan={4} className="px-2 py-3 text-muted">
                还没有授权：只有管理员能切换可预约、看别人的审计、断开别人的会话。
              </td>
            </tr>
          )}
        </tbody>
      </table>
      {editing && <GrantDrawer key={editing === "new" ? "new" : editing.name} grant={editing === "new" ? null : editing} groups={groups} onClose={() => setEditing(null)} />}
    </Panel>
  );
}

function GrantDrawer({ grant, groups, onClose }: { grant: Grant | null; groups: Group[]; onClose: () => void }) {
  const qc = useQueryClient();
  const [name, setName] = useState(grant?.name ?? "");
  const c = useCondition(grant?.match ?? "");
  const [perms, setPerms] = useState<GrantPermission[]>(grant?.permissions ?? []);
  const [description, setDescription] = useState(grant?.description ?? "");
  // disconnecting goes with seeing the audit: ticking it ticks that, unticking that unticks it
  const tick = (p: GrantPermission, on: boolean) => {
    let next = on ? [...perms, p] : perms.filter((x) => x !== p);
    if (on && p === "disconnectSessions" && !next.includes("viewAudit")) next = [...next, "viewAudit"];
    if (!on && p === "viewAudit") next = next.filter((x) => x !== "disconnectSessions");
    setPerms(GRANT_PERMISSIONS.map(([x]) => x).filter((x) => next.includes(x)));
  };
  const save = useMutation({
    mutationFn: () => {
      const body = { name: name.trim(), match: c.text.trim(), permissions: perms, ...(description.trim() ? { description: description.trim() } : {}) };
      return grant ? updateGrant({ ...body, name: grant.name, resourceVersion: grant.resourceVersion }) : createGrant(body);
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["grants"] });
      onClose();
    },
  });
  const empty = !name.trim() || !c.text.trim() || perms.length === 0;
  return (
    <Drawer
      open
      onOpenChange={(o) => !o && onClose()}
      title={grant ? `授权 ${grant.name}` : "新增授权"}
      actions={
        <>
          <Button onClick={onClose}>取消</Button>
          <Button variant="primary" disabled={c.bad || empty || save.isPending} title={c.bad ? "条件有错" : empty ? "要有名字、条件和至少一项权限" : undefined} onClick={() => save.mutate()}>
            确定
          </Button>
        </>
      }
    >
      <div className="space-y-3 text-xs">
        {!grant && (
          <Field label="名字" hint="小写字母、数字和 -，如 st2-admins-reservable">
            <Input aria-label="名字" className="mono" value={name} onChange={(ev) => setName(ev.target.value)} />
          </Field>
        )}
        <ConditionField c={c} groups={groups} hint="必填；授给所有人写 everyone" />
        <fieldset className="space-y-1">
          <legend className="mb-1 font-medium text-ink">授予</legend>
          <div className="flex flex-wrap gap-x-4 gap-y-1">
            {GRANT_PERMISSIONS.map(([p, label]) => (
              <label key={p} className="inline-flex items-center gap-1.5">
                <input type="checkbox" checked={perms.includes(p)} onChange={(ev) => tick(p, ev.target.checked)} />
                {label}
              </label>
            ))}
          </div>
          <p className="text-[11px] text-muted">看审计：会话、回放、命令、文件、HTTP 记录、告警和实时观看；断开会话须同时看审计。</p>
        </fieldset>
        <Field label="说明（可选）">
          <Input aria-label="说明" value={description} onChange={(ev) => setDescription(ev.target.value)} />
        </Field>
        {save.isError && <Notice>保存失败：{String(save.error)}</Notice>}
      </div>
    </Drawer>
  );
}
