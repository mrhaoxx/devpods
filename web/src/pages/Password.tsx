import { useState } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { me, changePassword, ApiFailure } from "../api";

export default function Password() {
  const meQ = useQuery({ queryKey: ["me"], queryFn: me });
  const [oldPassword, setOld] = useState("");
  const [newPassword, setNew] = useState("");
  const [msg, setMsg] = useState<string | null>(null);
  const [err, setErr] = useState(false);

  const save = async (e: React.FormEvent) => {
    e.preventDefault();
    setMsg(null);
    try {
      await changePassword(oldPassword, newPassword);
      setErr(false);
      setMsg("Password changed.");
      setOld("");
      setNew("");
    } catch (e) {
      setErr(true);
      setMsg(e instanceof ApiFailure ? e.body.message : String(e));
    }
  };

  return (
    <main className="mx-auto max-w-md p-8">
      <Link to="/" className="text-sm text-blue-600 hover:underline">
        ← My DevPods
      </Link>
      <h1 className="mb-4 mt-2 text-xl font-semibold">Change password</h1>
      {meQ.data && !meQ.data.hasPassword ? (
        <p className="rounded bg-slate-50 p-4 text-sm text-slate-600">
          Your account signs in via GitLab — there is no password to change.
        </p>
      ) : (
        <form onSubmit={save} className="space-y-3">
          <input
            type="password"
            value={oldPassword}
            onChange={(e) => setOld(e.target.value)}
            placeholder="Current password"
            autoComplete="current-password"
            className="w-full rounded border px-3 py-2 text-sm"
            required
          />
          <input
            type="password"
            value={newPassword}
            onChange={(e) => setNew(e.target.value)}
            placeholder="New password"
            autoComplete="new-password"
            className="w-full rounded border px-3 py-2 text-sm"
            required
          />
          <div className="flex items-center gap-3">
            <button className="rounded bg-blue-600 px-4 py-2 text-sm text-white" type="submit">
              Save
            </button>
            {msg && <span className={`text-sm ${err ? "text-red-700" : "text-slate-600"}`}>{msg}</span>}
          </div>
        </form>
      )}
    </main>
  );
}
