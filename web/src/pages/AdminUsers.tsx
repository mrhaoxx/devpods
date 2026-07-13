import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { listUsers, createUser, resetUserPassword, deleteUser, ApiFailure, AdminUser } from "../api";
import { BackLink, Button, Card, Field, Input, Notice, Shell } from "../ui";

export default function AdminUsers() {
  const qc = useQueryClient();
  const usersQ = useQuery({ queryKey: ["admin-users"], queryFn: listUsers });
  const [username, setUsername] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [password, setPassword] = useState("");
  const [err, setErr] = useState<string | null>(null);

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
            <Field label="Username">
              <Input value={username} onChange={(e) => setUsername(e.target.value)} className="mono" required />
            </Field>
          </div>
          <div className="min-w-[8rem] flex-1">
            <Field label="Display name">
              <Input value={displayName} onChange={(e) => setDisplayName(e.target.value)} />
            </Field>
          </div>
          <div className="min-w-[8rem] flex-1">
            <Field label="Initial password">
              <Input type="password" value={password} onChange={(e) => setPassword(e.target.value)} required />
            </Field>
          </div>
          <Button type="submit" variant="accent" size="sm">Create</Button>
        </form>
        {err && <div className="mt-3"><Notice>{err}</Notice></div>}
      </Card>

      <div className="mb-2 flex items-center justify-between">
        <p className="eyebrow">{users.length} users</p>
      </div>
      <ul className="overflow-hidden rounded-2xl border border-line bg-surface">
        {users.map((u, i) => (
          <li key={u.name} className={"flex items-center gap-3 px-4 py-3" + (i > 0 ? " border-t border-line" : "")}>
            <div className="min-w-0 flex-1">
              <div className="flex flex-wrap items-center gap-2">
                <span className="mono truncate text-sm text-ink">{u.name}</span>
                {u.admin && <span className="rounded bg-accent-soft px-1.5 py-0.5 text-[10px] font-medium text-accent">ADMIN</span>}
                {!u.hasPassword && <span className="rounded bg-idle-soft px-1.5 py-0.5 text-[10px] text-muted">no password</span>}
              </div>
              {u.displayName && <div className="text-xs text-muted">{u.displayName}</div>}
            </div>
            <span className="mono shrink-0 text-xs text-faint">{u.devpods} dp</span>
            <div className="flex shrink-0 gap-2 text-xs">
              <button onClick={() => reset(u)} className="text-accent hover:underline">reset</button>
              <button onClick={() => remove(u)} className="text-fail hover:underline">delete</button>
            </div>
          </li>
        ))}
      </ul>
    </Shell>
  );
}
