// The editor of one access rule — its condition, previewed as it is typed
// (the server reads it: the page has no parser of its own) — or of the
// default rule's effect.
import { useState } from "react";
import { Drawer } from "../../design/Drawer";
import { Button } from "../../design/Button";
import { Field, Input, Notice } from "../../design/Form";
import { ConditionField, useCondition } from "./ConditionField";
import { cleanPrivileges, PRIVILEGE_FLAGS, splitList, type AccessEffect, type AccessPrivileges, type AccessRule, type Group } from "./api";

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
  // the condition as typed; previewed once typing pauses
  const c = useCondition(rule.match, !isDefault);
  const bad = !isDefault && c.bad;

  const [e, setE] = useState<AccessEffect>(rule.effect);
  const setEffect = (p: Partial<AccessEffect>) => setE({ ...e, ...p });
  const priv: AccessPrivileges = e.privileges ?? {};
  const setPriv = (p: Partial<AccessPrivileges>) => setEffect({ privileges: { ...priv, ...p } });
  const [capsText, setCapsText] = useState((priv.capabilities ?? []).join(" "));
  const paths = e.hostPaths ?? [];
  const setPath = (i: number, p: Partial<{ path: string; writable: boolean }>) => setEffect({ hostPaths: paths.map((h, j) => (j === i ? { ...h, ...p } : h)) });

  const save = () => {
    const privileges = cleanPrivileges({ ...priv, capabilities: splitList(capsText) });
    const kept = paths.filter((h) => h.path.trim() !== "");
    const eff: AccessEffect = e.allow
      ? { allow: true, ...(privileges ? { privileges } : {}), ...(kept.length ? { hostPaths: kept } : {}), ...(e.needsReservation ? { needsReservation: true } : {}) }
      : { allow: false };
    onSave({ match: isDefault ? "" : c.text.trim(), effect: eff });
  };
  return (
    <Drawer
      open
      onOpenChange={(o) => !o && onClose()}
      title={isDefault ? "默认规则" : "访问规则"}
      actions={
        <>
          <Button onClick={onClose}>取消</Button>
          <Button variant="primary" disabled={bad} title={bad ? "条件有错" : undefined} onClick={save}>
            确定
          </Button>
        </>
      }
    >
      <div className="space-y-3 text-xs">
        {!isDefault && <ConditionField c={c} groups={groups} hint="留空 = 全部" warnUnreservable={e.allow && !!e.needsReservation} />}
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
              <div className="flex flex-wrap gap-x-4 gap-y-1">
                {PRIVILEGE_FLAGS.map(([k, label]) => (
                  <label key={k} className="inline-flex items-center gap-1.5">
                    <input type="checkbox" checked={!!priv[k]} onChange={(ev) => setPriv({ [k]: ev.target.checked })} />
                    {label}
                  </label>
                ))}
              </div>
              <Field label="可加的 capability" hint="空格分开，如 NET_ADMIN SYS_PTRACE；* = 任意">
                <Input aria-label="可加的 capability" className="mono" disabled={!!priv.privileged} value={capsText} onChange={(ev) => setCapsText(ev.target.value)} />
              </Field>
              {priv.privileged && <p className="text-[11px] text-muted">特权容器已含全部 capability</p>}
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
