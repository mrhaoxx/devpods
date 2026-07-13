import { useEffect, useState } from "react";
import { authConfig, passwordLogin, ApiFailure, AuthConfig } from "../api";
import { Button, CenterPage, Field, Input, Notice } from "../ui";

export default function Login() {
  const [cfg, setCfg] = useState<AuthConfig | null>(null);
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    authConfig().then(setCfg).catch(() => setCfg({ password: false, oauth: true }));
  }, []);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setErr(null);
    setBusy(true);
    try {
      await passwordLogin(username, password);
      window.location.href = "/";
    } catch (e) {
      setErr(e instanceof ApiFailure ? e.body.message : String(e));
      setBusy(false);
    }
  };

  return (
    <CenterPage>
      <div className="w-full max-w-sm">
        <div className="mb-8 text-center">
          <div className="mb-3 flex items-center justify-center gap-2">
            <span className="text-lg text-accent" aria-hidden>◈</span>
            <span className="mono text-lg font-semibold tracking-tight">devpod</span>
          </div>
          <p className="eyebrow">Remote development environments</p>
        </div>

        <div className="rounded-2xl border border-line bg-surface p-6 shadow-sm shadow-black/[0.03] sm:p-7">
          {cfg?.password && (
            <form onSubmit={submit} className="space-y-4">
              <Field label="Username">
                <Input value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="username" className="mono" required />
              </Field>
              <Field label="Password">
                <Input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" required />
              </Field>
              {err && <Notice>{err}</Notice>}
              <Button type="submit" variant="accent" className="w-full" disabled={busy}>
                {busy ? "Signing in…" : "Sign in"}
              </Button>
            </form>
          )}

          {cfg?.password && cfg?.oauth && (
            <div className="my-5 flex items-center gap-3">
              <span className="h-px flex-1 bg-line" />
              <span className="eyebrow">or</span>
              <span className="h-px flex-1 bg-line" />
            </div>
          )}

          {cfg?.oauth && (
            <a
              href="/auth/login"
              className="flex w-full items-center justify-center rounded-lg border border-line-strong bg-surface px-6 py-2 text-sm font-medium text-ink transition-colors hover:border-ink/30"
            >
              Sign in with GitLab
            </a>
          )}

          {cfg && !cfg.password && !cfg.oauth && (
            <Notice tone="warm">No login method is enabled — check the server configuration.</Notice>
          )}
        </div>
      </div>
    </CenterPage>
  );
}
