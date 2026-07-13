import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { me, changePassword, ApiFailure } from "../api";
import { BackLink, Button, Card, Field, Input, Notice, Shell } from "../ui";

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
    <Shell>
      <BackLink />
      <h1 className="mono mb-5 mt-3 text-xl font-semibold tracking-tight">Password</h1>

      {meQ.data && !meQ.data.hasPassword ? (
        <Notice tone="idle">You sign in via GitLab — there is no password to change.</Notice>
      ) : (
        <Card className="max-w-sm p-5">
          <form onSubmit={save} className="space-y-4">
            <Field label="Current password">
              <Input type="password" value={oldPassword} onChange={(e) => setOld(e.target.value)} autoComplete="current-password" required />
            </Field>
            <Field label="New password">
              <Input type="password" value={newPassword} onChange={(e) => setNew(e.target.value)} autoComplete="new-password" required />
            </Field>
            {msg && <Notice tone={err ? "fail" : "run"}>{msg}</Notice>}
            <Button type="submit" variant="accent" size="sm">
              Change password
            </Button>
          </form>
        </Card>
      )}
    </Shell>
  );
}
