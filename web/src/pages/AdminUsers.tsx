import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  listUsers,
  createUser,
  resetUserPassword,
  deleteUser,
  setUserQuota,
  ApiFailure,
  AdminUser,
  UserQuota,
} from "../api";
import { BackLink, Button, Card, Field, Input, Notice, Shell, cx } from "../ui";

function QuotaEditor({
  user,
  defaults,
  onDone,
}: {
  user: AdminUser;
  defaults: UserQuota;
  onDone: () => void;
}) {
  const q = user.quota;
  const gpuKey = "nvidia.com/gpu";
  const [maxDevPods, setMax] = useState(q?.maxDevPods != null ? String(q.maxDevPods) : "");
  const [cpu, setCpu] = useState(q?.compute?.cpu ?? "");
  const [memory, setMemory] = useState(q?.compute?.memory ?? "");
  const [gpu, setGpu] = useState(q?.compute?.[gpuKey] ?? "");
  const [storage, setStorage] = useState(q?.storage ?? "");
  const [err, setErr] = useState<string | null>(null);

  const save = async () => {
    setErr(null);
    try {
      await setUserQuota(user.name, {
        maxDevPods: maxDevPods === "" ? undefined : Number(maxDevPods),
        cpu,
        memory,
        gpu,
        storage,
      });
      onDone();
    } catch (e) {
      setErr(e instanceof ApiFailure ? e.body.message : String(e));
    }
  };

  const ph = (v?: string) => (v ? `default ${v}` : "unlimited");
  return (
    <div className="mt-3 rounded-lg border border-line bg-sunk p-3">
      <p className="mb-2 text-xs text-muted">
        Empty = the global default. Clear every field to remove this user&rsquo;s override.
      </p>
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-5">
        <Field label="Max DevPods">
          <Input value={maxDevPods} onChange={(e) => setMax(e.target.value)} placeholder={ph(defaults.maxDevPods?.toString())} className="mono" inputMode="numeric" />
        </Field>
        <Field label="CPU">
          <Input value={cpu} onChange={(e) => setCpu(e.target.value)} placeholder={ph(defaults.compute?.cpu)} className="mono" />
        </Field>
        <Field label="Memory">
          <Input value={memory} onChange={(e) => setMemory(e.target.value)} placeholder={ph(defaults.compute?.memory)} className="mono" />
        </Field>
        <Field label="GPU">
          <Input value={gpu} onChange={(e) => setGpu(e.target.value)} placeholder={ph(defaults.compute?.[gpuKey])} className="mono" inputMode="numeric" />
        </Field>
        <Field label="Storage">
          <Input value={storage} onChange={(e) => setStorage(e.target.value)} placeholder={ph(defaults.storage)} className="mono" />
        </Field>
      </div>
      {err && <div className="mt-2"><Notice>{err}</Notice></div>}
      <div className="mt-3 flex gap-2">
        <Button variant="accent" size="sm" onClick={save}>Save quota</Button>
        <Button size="sm" onClick={onDone}>Cancel</Button>
      </div>
    </div>
  );
}

export default function AdminUsers() {
  const qc = useQueryClient();
  const usersQ = useQuery({ queryKey: ["admin-users"], queryFn: listUsers });
  const [username, setUsername] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [password, setPassword] = useState("");
  const [err, setErr] = useState<string | null>(null);
  const [editing, setEditing] = useState<string | null>(null);

  const refresh = () => qc.invalidateQueries({ queryKey: ["admin-users"] });

  const create = async (e: React.FormEvent) => {
    e.preventDefault();
    setErr(null);
    try {
      await createUser(username, displayName, password);
      setUsername("");
      setDisplayName("");
      setPassword("");
      refresh();
    } catch (e) {
      setErr(e instanceof ApiFailure ? e.body.message : String(e));
    }
  };

  const reset = async (u: AdminUser) => {
    const pw = prompt(`New password for ${u.name}:`);
    if (!pw) return;
    try {
      await resetUserPassword(u.name, pw);
      refresh();
    } catch (e) {
      alert(e instanceof ApiFailure ? e.body.message : String(e));
    }
  };

  const remove = async (u: AdminUser) => {
    if (!confirm(`Delete user ${u.name}? (blocked while they own DevPods)`)) return;
    try {
      await deleteUser(u.name);
      refresh();
    } catch (e) {
      alert(e instanceof ApiFailure ? e.body.message : String(e));
    }
  };

  const users = usersQ.data?.items ?? [];
  const defaults = usersQ.data?.defaultQuota ?? {};

  const quotaSummary = (u: AdminUser) => {
    const cpuLim = u.quota?.compute?.cpu ?? defaults.compute?.cpu;
    const gpuLim = u.quota?.compute?.["nvidia.com/gpu"] ?? defaults.compute?.["nvidia.com/gpu"];
    let s = `${u.usage.cpu ?? "0"}/${cpuLim ?? "∞"} cpu`;
    if (u.usage.gpu || gpuLim) s += ` · ${u.usage.gpu ?? "0"}/${gpuLim ?? "∞"} gpu`;
    s += ` · ${u.devpods}/${u.quota?.maxDevPods ?? defaults.maxDevPods ?? "∞"} pods`;
    return s;
  };

  return (
    <Shell>
      <BackLink />
      <h1 className="mono mb-1 mt-3 text-xl font-semibold tracking-tight">Users</h1>
      <p className="mb-5 text-sm text-muted">
        Admin rights are granted with kubectl (<code className="mono text-xs">User.spec.admin</code>), not here.
      </p>

      <Card className="mb-5 p-4">
        <p className="eyebrow mb-3">Create user</p>
        <form onSubmit={create} className="flex flex-wrap items-end gap-3">
          <div className="min-w-[8rem] flex-1">
            <Field label="Username"><Input value={username} onChange={(e) => setUsername(e.target.value)} className="mono" required /></Field>
          </div>
          <div className="min-w-[8rem] flex-1">
            <Field label="Display name"><Input value={displayName} onChange={(e) => setDisplayName(e.target.value)} /></Field>
          </div>
          <div className="min-w-[8rem] flex-1">
            <Field label="Initial password"><Input type="password" value={password} onChange={(e) => setPassword(e.target.value)} required /></Field>
          </div>
          <Button type="submit" variant="accent" size="sm">Create</Button>
        </form>
        {err && <div className="mt-3"><Notice>{err}</Notice></div>}
      </Card>

      <p className="eyebrow mb-2">{users.length} users</p>
      <ul className="overflow-hidden rounded-2xl border border-line bg-surface">
        {users.map((u, i) => (
          <li key={u.name} className={cx("px-4 py-3", i > 0 && "border-t border-line")}>
            <div className="flex items-center gap-3">
              <div className="min-w-0 flex-1">
                <div className="flex flex-wrap items-center gap-2">
                  <span className="mono truncate text-sm text-ink">{u.name}</span>
                  {u.admin && <span className="rounded bg-accent-soft px-1.5 py-0.5 text-[10px] font-medium text-accent">ADMIN</span>}
                  {!u.hasPassword && <span className="rounded bg-idle-soft px-1.5 py-0.5 text-[10px] text-muted">no password</span>}
                  {u.quota && <span className="rounded bg-warm-soft px-1.5 py-0.5 text-[10px] text-warm">custom quota</span>}
                </div>
                <div className="mono text-xs text-faint">{quotaSummary(u)}</div>
              </div>
              <div className="flex shrink-0 gap-2 text-xs">
                <button onClick={() => setEditing(editing === u.name ? null : u.name)} className="text-accent hover:underline">quota</button>
                <button onClick={() => reset(u)} className="text-accent hover:underline">reset</button>
                <button onClick={() => remove(u)} className="text-fail hover:underline">delete</button>
              </div>
            </div>
            {editing === u.name && (
              <QuotaEditor user={u} defaults={defaults} onDone={() => { setEditing(null); refresh(); }} />
            )}
          </li>
        ))}
      </ul>
    </Shell>
  );
}
