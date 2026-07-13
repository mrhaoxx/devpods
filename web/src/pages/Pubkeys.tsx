import { useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { me, getPubkeys, putPubkeys, ApiFailure } from "../api";
import { BackLink, Button, Card, Notice, Shell } from "../ui";

export default function Pubkeys() {
  const meQ = useQuery({ queryKey: ["me"], queryFn: me });
  const [text, setText] = useState("");
  const [msg, setMsg] = useState<string | null>(null);
  const [err, setErr] = useState(false);

  useEffect(() => {
    getPubkeys().then((r) => setText((r.pubkeys ?? []).join("\n")));
  }, []);

  const save = async () => {
    setMsg(null);
    try {
      const keys = text.split("\n").map((l) => l.trim()).filter(Boolean);
      await putPubkeys(keys);
      setErr(false);
      setMsg(`Saved ${keys.length} key${keys.length === 1 ? "" : "s"}.`);
    } catch (e) {
      setErr(true);
      setMsg(e instanceof ApiFailure ? e.body.message : String(e));
    }
  };

  return (
    <Shell>
      <BackLink />
      <h1 className="mono mb-1 mt-3 text-xl font-semibold tracking-tight">SSH keys</h1>
      <p className="mb-5 text-sm text-muted">
        One key per line. Connect with{" "}
        <code className="mono rounded bg-sunk px-1.5 py-0.5 text-xs text-ink">ssh {meQ.data?.user}+&lt;pod&gt;@&lt;gateway&gt;</code>
      </p>

      {meQ.data?.features?.pubkeySelfService === false ? (
        <Notice tone="idle">SSH keys are managed externally on this deployment — ask an administrator to update them.</Notice>
      ) : (
        <Card className="p-5">
          <textarea
            value={text}
            onChange={(e) => setText(e.target.value)}
            rows={8}
            spellCheck={false}
            className="mono w-full rounded-lg border border-line-strong bg-sunk p-3 text-xs text-ink focus:border-accent focus-visible:outline-none"
            placeholder="ssh-ed25519 AAAA… laptop"
          />
          <div className="mt-3 flex items-center gap-3">
            <Button variant="accent" size="sm" onClick={save}>
              Save keys
            </Button>
            {msg && <span className={err ? "text-sm text-fail" : "text-sm text-run"}>{msg}</span>}
          </div>
        </Card>
      )}
    </Shell>
  );
}
