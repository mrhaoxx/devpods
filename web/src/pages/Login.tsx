import { useEffect, useState } from "react";
import { authConfig, passwordLogin, ApiFailure, AuthConfig } from "../api";

export default function Login() {
  const [cfg, setCfg] = useState<AuthConfig | null>(null);
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    authConfig().then(setCfg).catch(() => setCfg({ password: false, oauth: true }));
  }, []);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setErr(null);
    try {
      await passwordLogin(username, password);
      window.location.href = "/";
    } catch (e) {
      setErr(e instanceof ApiFailure ? e.body.message : String(e));
    }
  };

  return (
    <main className="flex min-h-screen items-center justify-center bg-slate-50">
      <div className="w-80 rounded-xl border bg-white p-8 shadow-sm">
        <h1 className="mb-1 text-center text-2xl font-semibold">DevPod</h1>
        <p className="mb-6 text-center text-sm text-slate-500">Remote development environments</p>

        {cfg?.password && (
          <form onSubmit={submit} className="space-y-3">
            <input
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              placeholder="Username"
              autoComplete="username"
              className="w-full rounded border px-3 py-2 text-sm"
              required
            />
            <input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder="Password"
              autoComplete="current-password"
              className="w-full rounded border px-3 py-2 text-sm"
              required
            />
            {err && <p className="rounded bg-red-50 p-2 text-xs text-red-700">{err}</p>}
            <button className="w-full rounded-lg bg-blue-600 px-4 py-2 font-medium text-white hover:bg-blue-700" type="submit">
              Sign in
            </button>
          </form>
        )}

        {cfg?.password && cfg?.oauth && (
          <div className="my-4 flex items-center gap-3 text-xs text-slate-400">
            <span className="h-px flex-1 bg-slate-200" />
            or
            <span className="h-px flex-1 bg-slate-200" />
          </div>
        )}

        {cfg?.oauth && (
          <a
            href="/auth/login"
            className="block rounded-lg bg-orange-600 px-6 py-2 text-center font-medium text-white hover:bg-orange-700"
          >
            Sign in with GitLab
          </a>
        )}

        {cfg && !cfg.password && !cfg.oauth && (
          <p className="text-center text-sm text-red-600">No login method is enabled.</p>
        )}
      </div>
    </main>
  );
}
