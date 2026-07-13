import { useState } from "react";
import { Link } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { listUsers, createUser, resetUserPassword, deleteUser, ApiFailure, AdminUser } from "../api";

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

  return (
    <main className="mx-auto max-w-3xl p-8">
      <Link to="/" className="text-sm text-blue-600 hover:underline">
        ← My DevPods
      </Link>
      <h1 className="mb-1 mt-2 text-xl font-semibold">Users</h1>
      <p className="mb-6 text-xs text-slate-500">
        Admin rights are granted with kubectl (<code>User.spec.admin</code>), not here.
      </p>

      <form onSubmit={create} className="mb-6 flex flex-wrap items-end gap-2 rounded-xl border bg-white p-4">
        <label className="text-xs">
          Username
          <input value={username} onChange={(e) => setUsername(e.target.value)} className="mt-1 block rounded border px-2 py-1 text-sm" required />
        </label>
        <label className="text-xs">
          Display name
          <input value={displayName} onChange={(e) => setDisplayName(e.target.value)} className="mt-1 block rounded border px-2 py-1 text-sm" />
        </label>
        <label className="text-xs">
          Initial password
          <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} className="mt-1 block rounded border px-2 py-1 text-sm" required />
        </label>
        <button className="rounded bg-blue-600 px-3 py-1.5 text-sm text-white" type="submit">
          Create
        </button>
        {err && <p className="w-full text-sm text-red-700">{err}</p>}
      </form>

      <table className="w-full rounded-xl border bg-white text-sm">
        <thead className="text-left text-xs text-slate-500">
          <tr className="border-b">
            <th className="p-3">Name</th>
            <th className="p-3">Display</th>
            <th className="p-3">Admin</th>
            <th className="p-3">Password</th>
            <th className="p-3">DevPods</th>
            <th className="p-3"></th>
          </tr>
        </thead>
        <tbody>
          {(usersQ.data?.items ?? []).map((u) => (
            <tr key={u.name} className="border-b last:border-0">
              <td className="p-3 font-mono">{u.name}</td>
              <td className="p-3">{u.displayName || "—"}</td>
              <td className="p-3">{u.admin ? "✓" : ""}</td>
              <td className="p-3">{u.hasPassword ? "set" : "—"}</td>
              <td className="p-3">{u.devpods}</td>
              <td className="p-3 text-right">
                <button className="mr-3 text-blue-600 hover:underline" onClick={() => reset(u)}>
                  reset pw
                </button>
                <button className="text-red-600 hover:underline" onClick={() => remove(u)}>
                  delete
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </main>
  );
}
