// The editor of one access rule (or of the default rule's effect).
import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Drawer } from "../../design/Drawer";
import { Button } from "../../design/Button";
import { Field, Input, Notice, Select } from "../../design/Form";
import { useClusterList } from "../../data/clusterFilter";
import { useDebounced } from "../../lib/useDebounced";
import { matchMachines, splitList, type AccessEffect, type AccessRule, type Group } from "./api";

export function RuleDrawer({
  rule,
  isDefault,
  groups,
  onSave,
  onClose,
}: {
  rule: AccessRule;
  isDefault: boolean;
  groups: Group[];
  onSave: (r: AccessRule) => void;
  onClose: () => void;
}) {
  const clusters = useClusterList().data?.items ?? [];
  const [r, setR] = useState<AccessRule>(rule);
  // the text as typed; the selector previewed once typing pauses
  const [machines, setMachines] = useState((rule.machines ?? []).join(" "));
  const [machinesText, setMachinesText] = useDebounced(machines, setMachines);
  const e = r.effect;
  const setEffect = (p: Partial<AccessEffect>) => setR({ ...r, effect: { ...e, ...p } });
  const match = useQuery({
    queryKey: ["access-match", r.cluster ?? "", machines],
    queryFn: () => matchMachines(r.cluster || undefined, splitList(machines)),
    enabled: !isDefault,
  });
  const count = (match.data?.items ?? []).reduce((a, x) => a + x.count, 0);
  const unreservable = (match.data?.items ?? []).reduce((a, x) => a + x.unreservable, 0);
  const paths = e.hostPaths ?? [];
  const setPath = (i: number, p: Partial<{ path: string; writable: boolean }>) => setEffect({ hostPaths: paths.map((h, j) => (j === i ? { ...h, ...p } : h)) });
  const save = () => {
    const out: AccessRule = { ...r, machines: splitList(machinesText) };
    if (!out.cluster) delete out.cluster;
    if (!out.machines?.length) delete out.machines;
    const eff: AccessEffect = out.effect.allow
      ? { ...out.effect, hostPaths: (out.effect.hostPaths ?? []).filter((h) => h.path.trim() !== "") }
      : { allow: false };
    onSave({ ...out, effect: eff });
  };
  return (
    <Drawer
      open
      onOpenChange={(o) => !o && onClose()}
      title={isDefault ? "默认规则" : "访问规则"}
      actions={
        <>
          <Button onClick={onClose}>取消</Button>
          <Button variant="primary" onClick={save}>
            确定
          </Button>
        </>
      }
    >
      <div className="space-y-3 text-xs">
        {!isDefault && (
          <>
            <Field label="集群">
              <Select aria-label="集群" value={r.cluster ?? ""} onChange={(ev) => setR({ ...r, cluster: ev.target.value || undefined })}>
                <option value="">任何集群</option>
                {clusters.map((c) => (
                  <option key={c.name} value={c.name}>
                    {c.displayName || c.name}
                  </option>
                ))}
              </Select>
            </Field>
            <Field label="机器" hint="名字（可用 * 通配）或标签 key=value，空格分开；留空 = 全部">
              <Input
                aria-label="机器"
                className="mono"
                value={machinesText}
                onChange={(ev) => setMachinesText(ev.target.value)}
              />
            </Field>
            {match.data && (
              <p className="text-muted">
                命中 {count} 台机器
                {e.allow && e.needsReservation && unreservable > 0 && (
                  <span className="text-warm">；其中 {unreservable} 台没标可预约，需预约的人将用不了它们</span>
                )}
              </p>
            )}
            <Field label="主体">
              <Select
                aria-label="主体"
                value={r.subject.kind}
                onChange={(ev) => setR({ ...r, subject: { kind: ev.target.value as AccessRule["subject"]["kind"], name: ev.target.value === "everyone" ? undefined : r.subject.name } })}
              >
                <option value="everyone">所有人</option>
                <option value="user">用户</option>
                <option value="group">组</option>
              </Select>
            </Field>
            {r.subject.kind === "user" && (
              <Input aria-label="用户名" placeholder="用户名" value={r.subject.name ?? ""} onChange={(ev) => setR({ ...r, subject: { kind: "user", name: ev.target.value.trim() } })} />
            )}
            {r.subject.kind === "group" && (
              <Select aria-label="组" value={r.subject.name ?? ""} onChange={(ev) => setR({ ...r, subject: { kind: "group", name: ev.target.value } })}>
                <option value="">选择组…</option>
                {groups.map((g) => (
                  <option key={g.name} value={g.name}>
                    {g.displayName ? `${g.name}（${g.displayName}）` : g.name}
                  </option>
                ))}
              </Select>
            )}
          </>
        )}
        <fieldset className="space-y-2">
          <legend className="mb-1 font-medium text-ink">效果</legend>
          <div className="flex gap-4">
            <label className="inline-flex items-center gap-1.5">
              <input type="radio" name="allow" checked={!e.allow} onChange={() => setEffect({ allow: false })} />
              拒绝
            </label>
            <label className="inline-flex items-center gap-1.5">
              <input type="radio" name="allow" checked={e.allow} onChange={() => setEffect({ allow: true })} />
              允许
            </label>
          </div>
          {e.allow && (
            <div className="space-y-2 pl-1">
              <label className="flex items-center gap-1.5" title="特权容器、额外的 capabilities、主机的 PID / 网络 / IPC">
                <input type="checkbox" checked={!!e.privileged} onChange={(ev) => setEffect({ privileged: ev.target.checked })} />
                特权和主机
              </label>
              <label className="flex items-center gap-1.5" title="只有在本人预约这台机器期间才能用">
                <input type="checkbox" checked={!!e.needsReservation} onChange={(ev) => setEffect({ needsReservation: ev.target.checked })} />
                需预约
              </label>
              <div className="space-y-1">
                <span className="text-muted">允许挂载的主机路径（home 不算）</span>
                {paths.map((h, i) => (
                  <div key={i} className="flex items-center gap-2">
                    <Input aria-label={`主机路径 ${i + 1}`} className="mono flex-1" placeholder="/data/models" value={h.path} onChange={(ev) => setPath(i, { path: ev.target.value })} />
                    <label className="inline-flex items-center gap-1">
                      <input type="checkbox" checked={!!h.writable} onChange={(ev) => setPath(i, { writable: ev.target.checked })} />
                      可写
                    </label>
                    <Button aria-label={`删除主机路径 ${i + 1}`} onClick={() => setEffect({ hostPaths: paths.filter((_, j) => j !== i) })}>
                      ✕
                    </Button>
                  </div>
                ))}
                <Button onClick={() => setEffect({ hostPaths: [...paths, { path: "" }] })}>添加主机路径</Button>
                {paths.some((h) => h.writable) && (
                  <Notice tone="warm">可写路径不是安全边界：用户可以在里面建指向 / 的符号链接，再挂载它拿到主机根目录。给出可写路径就等于信任这个人。</Notice>
                )}
              </div>
            </div>
          )}
        </fieldset>
      </div>
    </Drawer>
  );
}
