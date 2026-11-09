import { useEffect, useState } from "react";
import { getUserAuditPolicy, putUserAuditPolicy } from "../api";
import { formFromPolicy, policyFromForm, PolicyForm, Tri } from "../policy";
import { Button, Notice, cx } from "../ui";

const ROWS: [keyof PolicyForm, string][] = [
  ["download", "Download"],
  ["upload", "Upload"],
  ["forwardingLocal", "Local forwarding (VS Code)"],
  ["forwardingRemote", "Remote forwarding"],
  ["agentForwarding", "Agent forwarding"],
  ["x11Forwarding", "X11 forwarding"],
];

function Select<T extends string>({ value, options, onChange }: { value: T; options: T[]; onChange: (v: T) => void }) {
  return (
    <select
      value={value}
      onChange={(e) => onChange(e.target.value as T)}
      className="mono rounded-md border border-line-strong bg-surface px-1.5 py-0.5 text-xs"
    >
      {options.map((o) => (
        <option key={o} value={o}>
          {o}
        </option>
      ))}
    </select>
  );
}

// AuditPolicyEditor edits User.spec.auditPolicy; "inherit" = cluster default.
export function AuditPolicyEditor({ user, onDone }: { user: string; onDone: () => void }) {
  const [form, setForm] = useState<PolicyForm | null>(null);
  const [data, setData] = useState<Awaited<ReturnType<typeof getUserAuditPolicy>> | null>(null);
  const [err, setErr] = useState("");

  const load = () =>
    getUserAuditPolicy(user)
      .then((d) => {
        setData(d);
        setForm(formFromPolicy(d.user));
      })
      .catch((e) => setErr(e.message));
  useEffect(() => {
    void load();
  }, [user]); // eslint-disable-line react-hooks/exhaustive-deps

  if (!form) return err ? <Notice>{err}</Notice> : <p className="mono py-2 text-xs text-faint">loading…</p>;
  const set = <K extends keyof PolicyForm>(k: K, v: PolicyForm[K]) => setForm({ ...form, [k]: v });
  const save = async () => {
    setErr("");
    try {
      await putUserAuditPolicy(user, policyFromForm(form));
      await load();
      onDone();
    } catch (e) {
      setErr((e as Error).message);
    }
  };

  return (
    <div className="mono mt-3 rounded-lg border border-line bg-sunk/50 p-3 text-xs">
      <div className="grid gap-x-6 gap-y-1.5 sm:grid-cols-2">
        {ROWS.map(([k, label]) => (
          <label key={k} className="flex items-center justify-between gap-2">
            <span className="text-muted">{label}</span>
            <Select<Tri> value={form[k] as Tri} options={["inherit", "Allow", "Deny"]} onChange={(v) => set(k, v)} />
          </label>
        ))}
        <label className="flex items-center justify-between gap-2">
          <span className="text-muted">Record forwarded content</span>
          <Select value={form.recordContent} options={["inherit", "true", "false"]} onChange={(v) => set("recordContent", v)} />
        </label>
        <label className="flex items-center justify-between gap-2">
          <span className="text-muted">Fail mode</span>
          <Select value={form.failMode} options={["inherit", "Open", "Closed"]} onChange={(v) => set("failMode", v)} />
        </label>
        <label className="flex items-center justify-between gap-2">
          <span className="text-muted">Max file size</span>
          <input
            value={form.maxFileSize}
            onChange={(e) => set("maxFileSize", e.target.value)}
            placeholder="inherit (e.g. 100Mi)"
            className="w-36 rounded-md border border-line-strong bg-surface px-1.5 py-0.5"
          />
        </label>
      </div>
      {data && (
        <p className="mt-2 text-faint">
          effective:{" "}
          {Object.entries(data.effective)
            .filter(([, v]) => v)
            .map(([k, v]) => (v === true ? k : `${k}=${v}`))
            .join(", ") || "allow everything"}
        </p>
      )}
      {data?.warnings.map((w) => (
        <p key={w} className={cx("mt-1 text-warm")}>
          ⚠ {w}
        </p>
      ))}
      {err && (
        <div className="mt-2">
          <Notice>{err}</Notice>
        </div>
      )}
      <div className="mt-3 flex gap-2">
        <Button size="sm" variant="accent" onClick={save}>
          Save
        </Button>
        <Button size="sm" onClick={onDone}>
          Close
        </Button>
      </div>
    </div>
  );
}
