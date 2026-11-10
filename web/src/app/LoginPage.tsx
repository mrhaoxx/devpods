import { useEffect, useState, type FormEvent } from "react";
import { authConfig, passwordLogin, ApiFailure, type AuthConfig } from "../data/api";
import { Button } from "../design/Button";
import { Field, Input, Notice } from "../design/Form";

/** Login (UI spec §5.11): password, GitLab or both, per /api/auth/config. */
export function LoginPage() {
  const [cfg, setCfg] = useState<AuthConfig | null>(null);
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    authConfig()
      .then(setCfg)
      .catch(() => setCfg({ password: false, oauth: true }));
  }, []);

  const submit = async (e: FormEvent) => {
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
    <main className="grid min-h-dvh place-items-center bg-paper px-4">
      <div className="w-full max-w-xs">
        <div className="mb-6 flex items-center justify-center gap-2">
          <span className="text-lg text-accent" aria-hidden="true">
            ◈
          </span>
          <span className="mono text-lg font-semibold">devpod</span>
        </div>
        <div className="space-y-4 rounded-lg border border-line bg-surface p-5">
          {cfg?.password && (
            <form onSubmit={submit} className="space-y-3">
              <Field label="用户名">
                <Input value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="username" className="mono" required />
              </Field>
              <Field label="密码">
                <Input type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" required />
              </Field>
              {err && <Notice>{err}</Notice>}
              <Button type="submit" variant="primary" size="md" className="w-full" disabled={busy}>
                {busy ? "登录中…" : "登录"}
              </Button>
            </form>
          )}
          {cfg?.password && cfg?.oauth && <div className="text-center text-[11px] text-faint">或</div>}
          {cfg?.oauth && (
            <a
              href="/auth/login"
              className="flex h-8 w-full items-center justify-center rounded-md border border-line-strong text-sm font-medium hover:bg-sunk"
            >
              使用 GitLab 登录
            </a>
          )}
          {cfg && !cfg.password && !cfg.oauth && <Notice tone="warm">没有启用任何登录方式，请检查服务端配置。</Notice>}
        </div>
      </div>
    </main>
  );
}
