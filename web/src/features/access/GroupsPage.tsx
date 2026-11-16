// 用户组: groups of users, named in the access rules.
import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Button } from "../../design/Button";
import { Drawer } from "../../design/Drawer";
import { Field, Input, Notice } from "../../design/Form";
import { Panel } from "../../design/Panel";
import { EmptyState } from "../../design/EmptyState";
import { ConfirmDialog } from "../../design/ConfirmDialog";
import { createGroup, deleteGroup, listGroups, splitList, updateGroup, type Group } from "./api";

function GroupDrawer({ group, onClose }: { group: Group | null; onClose: () => void }) {
  const qc = useQueryClient();
  const [name, setName] = useState(group?.name ?? "");
  const [displayName, setDisplayName] = useState(group?.displayName ?? "");
  const [members, setMembers] = useState((group?.members ?? []).join(", "));
  const save = useMutation({
    mutationFn: () => {
      const g = { name: name.trim(), displayName: displayName.trim(), members: splitList(members) };
      return group ? updateGroup(g) : createGroup(g);
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["groups"] });
      onClose();
    },
  });
  return (
    <Drawer
      open
      onOpenChange={(o) => !o && onClose()}
      title={group ? `用户组 ${group.name}` : "新建用户组"}
      actions={
        <>
          <Button onClick={onClose}>取消</Button>
          <Button variant="primary" disabled={!name.trim() || save.isPending} onClick={() => save.mutate()}>
            保存
          </Button>
        </>
      }
    >
      <div className="space-y-3">
        <Field label="组名" hint="小写字母、数字和 -">
          <Input aria-label="组名" className="mono" value={name} disabled={!!group} onChange={(e) => setName(e.target.value)} />
        </Field>
        <Field label="显示名">
          <Input aria-label="显示名" value={displayName} onChange={(e) => setDisplayName(e.target.value)} />
        </Field>
        <Field label="成员" hint="用户名，逗号或空格分开">
          <Input aria-label="成员" className="mono" value={members} onChange={(e) => setMembers(e.target.value)} />
        </Field>
        {save.isError && <Notice>保存失败：{String(save.error)}</Notice>}
      </div>
    </Drawer>
  );
}

export function GroupsPage() {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["groups"], queryFn: listGroups });
  const [editing, setEditing] = useState<{ group: Group | null } | null>(null);
  const [removing, setRemoving] = useState<Group | null>(null);
  const del = useMutation({
    mutationFn: (name: string) => deleteGroup(name),
    onSettled: () => void qc.invalidateQueries({ queryKey: ["groups"] }),
  });
  if (q.isError) return <EmptyState tone="error" title="无法加载用户组" hint={String(q.error)} />;
  const items = q.data?.items ?? [];
  return (
    <div className="space-y-3 p-3">
      <Panel title="用户组（访问规则里可以按组授权）" actions={<Button onClick={() => setEditing({ group: null })}>新建组</Button>}>
        {del.isError && (
          <div className="p-2">
            <Notice>删除失败：{String(del.error)}</Notice>
          </div>
        )}
        {items.length === 0 ? (
          <EmptyState title={q.isLoading ? "加载中…" : "还没有用户组"} />
        ) : (
          <table className="w-full text-xs">
            <thead>
              <tr className="border-b border-line text-left text-[11px] text-muted">
                <th className="px-2 py-1">组名</th>
                <th className="px-2">显示名</th>
                <th className="px-2">成员</th>
                <th className="w-32 px-2" />
              </tr>
            </thead>
            <tbody>
              {items.map((g) => (
                <tr key={g.name} className="border-b border-line">
                  <td className="px-2 py-1 mono">{g.name}</td>
                  <td className="px-2">{g.displayName}</td>
                  <td className="px-2 mono">{g.members.join(", ")}</td>
                  <td className="px-2 text-right whitespace-nowrap">
                    <Button onClick={() => setEditing({ group: g })}>编辑</Button>{" "}
                    <Button variant="danger" onClick={() => setRemoving(g)}>
                      删除
                    </Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Panel>
      {editing && <GroupDrawer key={editing.group?.name ?? "new"} group={editing.group} onClose={() => setEditing(null)} />}
      <ConfirmDialog
        open={!!removing}
        onOpenChange={(o) => !o && setRemoving(null)}
        title={`删除用户组 ${removing?.name}？`}
        confirmLabel="删除"
        danger
        onConfirm={() => {
          const g = removing!;
          setRemoving(null);
          del.mutate(g.name);
        }}
      >
        被访问规则引用的组不能删除，先改掉那些规则。
      </ConfirmDialog>
    </div>
  );
}
