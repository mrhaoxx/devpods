// Per-user audit policy override (User.spec.auditPolicy); "inherit" =
// the cluster default. PolicyFields is shared with the batch dialog.
import { useEffect, useState } from "react";
import { getUserAuditPolicy, putUserAuditPolicy, ApiFailure } from "../../data/api";
import { formFromPolicy, policyFromForm, type PolicyForm, type Tri } from "../../lib/policy";
import { Button } from "../../design/Button";
import { Notice } from "../../design/Form";

const ROWS: [keyof PolicyForm, string][] = [
  ["download", "下载文件"],
  ["upload", "上传文件"],
  ["forwardingLocal", "本地端口转发（VS Code）"],
  ["forwardingRemote", "远程端口转发"],
  ["agentForwarding", "Agent 转发"],
  ["x11Forwarding", "X11 转发"],
];
const TRI: [Tri, string][] = [
  ["inherit", "默认"],
  ["Allow", "允许"],
  ["Deny", "拒绝"],
];

const sel = "mono h-6 rounded border border-line-strong bg-surface px-1 text-xs";

export function PolicyFields({ form, onChange }: { form: PolicyForm; onChange: (f: PolicyForm) => void }) {
  const set = <K extends keyof PolicyForm>(k: K, v: PolicyForm[K]) => onChange({ ...form, [k]: v });
  return (
    <div className="grid gap-x-6 gap-y-1.5 text-xs sm:grid-cols-2">
      {ROWS.map(([k, label]) => (
        <label key={k} className="flex items-center justify-between gap-2">
          <span className="text-muted">{label}</span>
          <select aria-label={label} className={sel} value={form[k] as Tri} onChange={(e) => set(k, e.target.value as Tri)}>
            {TRI.map(([v, l]) => (
              <option key={v} value={v}>
                {l}
              </option>
            ))}
          </select>
        </label>
      ))}
      <label className="flex items-center justify-between gap-2">
        <span className="text-muted">记录转发内容</span>
        <select aria-label="记录转发内容" className={sel} value={form.recordContent} onChange={(e) => set("recordContent", e.target.value as PolicyForm["recordContent"])}>
          <option value="inherit">默认</option>
          <option value="true">是</option>
          <option value="false">否</option>
        </select>
      </label>
      <label className="flex items-center justify-between gap-2">
        <span className="text-muted">审计失败时</span>
        <select aria-label="审计失败时" className={sel} value={form.failMode} onChange={(e) => set("failMode", e.target.value as PolicyForm["failMode"])}>
          <option value="inherit">默认</option>
          <option value="Open">放行</option>
          <option value="Closed">拒绝</option>
        </select>
      </label>
      <label className="flex items-center justify-between gap-2">
        <span className="text-muted">单文件上限</span>
        <input
          aria-label="单文件上限"
          value={form.maxFileSize}
          onChange={(e) => set("maxFileSize", e.target.value)}
          placeholder="默认（如 100Mi）"
          className="mono h-6 w-32 rounded border border-line-strong bg-surface px-1.5 text-xs"
        />
      </label>
    </div>
  );
}

export function AuditPolicyEditor({ user }: { user: string }) {
  const [form, setForm] = useState<PolicyForm | null>(null);
  const [data, setData] = useState<Awaited<ReturnType<typeof getUserAuditPolicy>> | null>(null);
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const load = () =>
    getUserAuditPolicy(user)
      .then((d) => {
        setData(d);
        setForm(formFromPolicy(d.user));
      })
      .catch((e) => setMsg({ ok: false, text: e instanceof ApiFailure ? e.body.message : String(e) }));
  useEffect(() => {
    void load();
  }, [user]); // eslint-disable-line react-hooks/exhaustive-deps

  if (!form) return msg ? <Notice>{msg.text}</Notice> : <p className="text-xs text-faint">加载中…</p>;
  const save = async () => {
    setMsg(null);
    try {
      await putUserAuditPolicy(user, policyFromForm(form));
      await load();
      setMsg({ ok: true, text: "已保存" });
    } catch (e) {
      setMsg({ ok: false, text: e instanceof ApiFailure ? e.body.message : String(e) });
    }
  };
  return (
    <div className="space-y-2">
      <PolicyFields form={form} onChange={setForm} />
      {data && (
        <p className="mono text-[11px] text-faint">
          生效：
          {Object.entries(data.effective)
            .filter(([, v]) => v)
            .map(([k, v]) => (v === true ? k : `${k}=${v}`))
            .join(", ") || "全部允许"}
        </p>
      )}
      {data?.warnings.map((w) => (
        <Notice key={w} tone="warm">
          {w}
        </Notice>
      ))}
      <div className="flex items-center gap-2">
        <Button variant="primary" onClick={save}>
          保存策略
        </Button>
        {msg && <span className={`text-xs ${msg.ok ? "text-run" : "text-fail"}`}>{msg.text}</span>}
      </div>
    </div>
  );
}
