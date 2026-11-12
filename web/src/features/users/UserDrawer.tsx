// One user (UI spec §5.5): quota with current usage beside each field
// (blank = default), DevPods, recent sessions, policy override, SSH keys,
// 设为管理员 / 删除用户 behind confirmation.
import { useClusters } from "../clusters/ClusterSwitcher";
import { useState } from "react";
import { Link } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { deleteUser, listDevPods, setUserAdmin, setUserClusterQuota, setUserQuota, ApiFailure, type AdminUserRow, type UserQuota } from "../../data/api";
import { qk, useAuditSessions, useMe } from "../../data/queries";
import { Drawer } from "../../design/Drawer";
import { Panel } from "../../design/Panel";
import { Button } from "../../design/Button";
import { Badge } from "../../design/Badge";
import { Field, Input, Notice, Select } from "../../design/Form";
import { ConfirmDialog } from "../../design/ConfirmDialog";
import { EmptyState } from "../../design/EmptyState";
import { StatusDot } from "../../design/StatusDot";
import { AuditPolicyEditor } from "../audit/AuditPolicyEditor";

const sourceLabel = { oidc: "GitLab", password: "密码", ldap: "LDAP" } as const;

/** Quota: the user's own, for every cluster — or, in a console of several
 *  clusters, their quota in one of them (which goes before their own). */
function Quota({ u, def }: { u: AdminUserRow; def: UserQuota }) {
  const clusters = useClusters().data?.items ?? [];
  const [where, setWhere] = useState("");
  return (
    <>
      {clusters.length > 1 && (
        <div className="flex items-center gap-2 px-3 pt-3 text-xs">
          <span className="text-muted">作用于</span>
          <Select aria-label="配额作用于" value={where} onChange={(e) => setWhere(e.target.value)} className="!w-auto">
            <option value="">所有集群</option>
            {clusters.map((c) => (
              <option key={c.name} value={c.name}>
                {c.displayName || c.name}
              </option>
            ))}
          </Select>
          {where && <span className="text-[11px] text-faint">这里填的项优先于「所有集群」里的同一项</span>}
        </div>
      )}
      <QuotaEditor key={where} u={u} def={def} cluster={where} />
    </>
  );
}

function QuotaEditor({ u, def, cluster }: { u: AdminUserRow; def: UserQuota; cluster: string }) {
  const qc = useQueryClient();
  const q = cluster ? u.quotas?.[cluster] : u.quota;
  const [f, setF] = useState({
    maxDevPods: q?.maxDevPods?.toString() ?? "",
    cpu: q?.compute?.cpu ?? "",
    memory: q?.compute?.memory ?? "",
    gpu: q?.compute?.["nvidia.com/gpu"] ?? "",
    storage: q?.storage ?? "",
  });
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const rows: [keyof typeof f, string, string | undefined, string | undefined][] = [
    ["maxDevPods", "DevPod 数", String(u.devpods), def.maxDevPods?.toString()],
    ["cpu", "CPU", u.usage.cpu ?? "0", def.compute?.cpu],
    ["memory", "内存", u.usage.memory ?? "0", def.compute?.memory],
    ["gpu", "GPU", u.usage.gpu ?? "0", def.compute?.["nvidia.com/gpu"]],
    ["storage", "存储", u.usage.storage ?? "0", def.storage],
  ];
  const save = async () => {
    setMsg(null);
    try {
      const patch = {
        maxDevPods: f.maxDevPods ? Number(f.maxDevPods) : null,
        cpu: f.cpu || undefined,
        memory: f.memory || undefined,
        gpu: f.gpu || undefined,
        storage: f.storage || undefined,
      };
      if (cluster) await setUserClusterQuota(u.name, cluster, patch);
      else await setUserQuota(u.name, patch);
      setMsg({ ok: true, text: "已保存" });
      void qc.invalidateQueries({ queryKey: qk.users });
    } catch (e) {
      setMsg({ ok: false, text: e instanceof ApiFailure ? e.body.message : String(e) });
    }
  };
  return (
    <div className="space-y-2 p-3">
      <div className="grid grid-cols-2 gap-2 max-[700px]:grid-cols-1">
        {rows.map(([k, label, used, d]) => (
          <Field key={k} label={label} hint={`已用 ${used}`}>
            <Input aria-label={`配额 ${label}`} value={f[k]} onChange={(e) => setF({ ...f, [k]: e.target.value })} placeholder={d ? `默认 ${d}` : "默认（不限）"} className="mono" />
          </Field>
        ))}
      </div>
      <div className="flex items-center gap-2">
        <Button variant="primary" onClick={() => void save()}>
          保存配额
        </Button>
        <span className="text-[11px] text-faint">留空 = 使用全局默认</span>
        {msg && <span className={`text-xs ${msg.ok ? "text-run" : "text-fail"}`}>{msg.text}</span>}
      </div>
    </div>
  );
}

export function UserDrawer({ u, def, onClose }: { u?: AdminUserRow; def: UserQuota; onClose: () => void }) {
  const me = useMe().data;
  const qc = useQueryClient();
  const audit = !!me?.features.audit;
  const name = u?.name ?? "";
  const pods = useQuery({ queryKey: qk.devpods("all"), queryFn: () => listDevPods("all"), enabled: !!u });
  const sessions = useAuditSessions({ user: name, limit: "10" }, { enabled: audit && !!u });
  const [confirm, setConfirm] = useState<"admin" | "delete" | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const mine = (pods.data?.items ?? []).filter((d) => d.spec.owner === name);

  const act = async (fn: () => Promise<unknown>, after?: () => void) => {
    setErr(null);
    try {
      await fn();
      void qc.invalidateQueries({ queryKey: qk.users });
      after?.();
    } catch (e) {
      setErr(e instanceof ApiFailure ? e.body.message : String(e));
    }
  };

  return (
    <Drawer
      open
      onOpenChange={(o) => !o && onClose()}
      title={
        <span className="inline-flex items-center gap-2">
          <span className="mono">{name}</span>
          {u?.admin && <Badge tone="accent">管理员</Badge>}
        </span>
      }
      width={640}
    >
      {!u ? (
        <EmptyState title="没有这个用户" hint="它可能只存在于 LDAP（没有 User 对象），或已被删除。" />
      ) : (
        <div className="space-y-3">
          <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs">
            {u.displayName && <span>{u.displayName}</span>}
            <span className="text-muted">来源 {sourceLabel[u.source]}</span>
            <span className="text-muted">SSH 公钥 {u.pubkeys} 个</span>
            <span className="text-muted">{u.lastSession ? `最近会话 ${new Date(u.lastSession).toLocaleString()}` : "从未登录"}</span>
          </div>
          {err && <Notice>{err}</Notice>}
          <Panel title="配额">
            <Quota u={u} def={def} />
          </Panel>
          <Panel title={`DevPod（${mine.length}）`}>
            {mine.length === 0 ? (
              <EmptyState title="没有 DevPod" />
            ) : (
              <ul className="divide-y divide-line text-xs">
                {mine.map((d) => (
                  <li key={d.metadata.name} className="flex items-center gap-2 px-3 py-1">
                    <StatusDot phase={d.status?.phase} />
                    <span className="mono min-w-0 flex-1 truncate">{d.metadata.name}</span>
                    <Link to={`/devpods/${d.metadata.name}`} className="text-accent hover:underline">
                      打开
                    </Link>
                  </li>
                ))}
              </ul>
            )}
          </Panel>
          {audit && (
            <Panel title="最近会话">
              {sessions.isError ? (
                <EmptyState tone="error" title="审计数据暂不可用" />
              ) : (sessions.data?.items ?? []).length === 0 ? (
                <EmptyState title="没有会话" />
              ) : (
                <ul className="divide-y divide-line text-xs">
                  {sessions.data!.items.map((s) => (
                    <li key={s.session_id} className="flex items-center gap-2 px-3 py-1">
                      <span className="mono">{s.devpod}</span>
                      <span className="text-muted">{new Date(s.opened_at).toLocaleString()}</span>
                      <Link to={`/sessions/${s.session_id}`} className="ml-auto text-accent hover:underline">
                        {s.closed_at ? "回放" : "直播"}
                      </Link>
                    </li>
                  ))}
                </ul>
              )}
            </Panel>
          )}
          {audit && (
            <Panel title="审计策略覆盖">
              <div className="p-3">
                <AuditPolicyEditor user={name} />
              </div>
            </Panel>
          )}
          <div className="flex flex-wrap gap-2">
            <Button onClick={() => setConfirm("admin")} disabled={name === me?.user && u.admin}>
              {u.admin ? "取消管理员…" : "设为管理员…"}
            </Button>
            <Button variant="danger" onClick={() => setConfirm("delete")}>
              删除用户…
            </Button>
          </div>
        </div>
      )}
      <ConfirmDialog
        open={confirm === "admin"}
        onOpenChange={(o) => !o && setConfirm(null)}
        title={u?.admin ? `取消 ${name} 的管理员权限？` : `把 ${name} 设为管理员？`}
        confirmLabel="确认"
        onConfirm={() => {
          setConfirm(null);
          void act(() => setUserAdmin(name, !u?.admin));
        }}
      >
        管理员可以查看和操作所有用户的 DevPod、会话与配额。
      </ConfirmDialog>
      <ConfirmDialog
        open={confirm === "delete"}
        onOpenChange={(o) => !o && setConfirm(null)}
        title={`删除用户 ${name}？`}
        confirmLabel="确认删除"
        requireText={name}
        danger
        onConfirm={() => {
          setConfirm(null);
          void act(() => deleteUser(name), onClose);
        }}
      >
        用户名下还有 DevPod 时删除会被拒绝。
      </ConfirmDialog>
    </Drawer>
  );
}
