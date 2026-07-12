import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { me, getPubkeys, putPubkeys, ApiFailure } from "../api";

export default function Pubkeys() {
  const meQ = useQuery({ queryKey: ["me"], queryFn: me });
  const [text, setText] = useState("");
  const [msg, setMsg] = useState<string | null>(null);

  useEffect(() => {
    getPubkeys().then((r) => setText((r.pubkeys ?? []).join("\n")));
  }, []);

  const save = async () => {
    setMsg(null);
    try {
      const keys = text
        .split("\n")
        .map((l) => l.trim())
        .filter(Boolean);
      await putPubkeys(keys);
      setMsg(`Saved ${keys.length} key(s).`);
    } catch (e) {
      setMsg(e instanceof ApiFailure ? e.body.message : String(e));
    }
  };

  return (
    <main className="mx-auto max-w-2xl p-8">
      <Link to="/" className="text-sm text-blue-600 hover:underline">
        ← My DevPods
      </Link>
      <h1 className="mb-2 mt-2 text-xl font-semibold">SSH public keys</h1>
      <p className="mb-4 text-sm text-slate-500">
        One key per line. Then connect with{" "}
        <code className="rounded bg-slate-100 px-1">ssh {meQ.data?.user}+&lt;pod&gt;@&lt;gateway&gt;</code>
      </p>
      <textarea
        value={text}
        onChange={(e) => setText(e.target.value)}
        rows={8}
        className="w-full rounded border p-2 font-mono text-xs"
        placeholder="ssh-ed25519 AAAA… laptop"
      />
      <div className="mt-3 flex items-center gap-3">
        <button onClick={save} className="rounded bg-blue-600 px-4 py-2 text-sm text-white">
          Save
        </button>
        {msg && <span className="text-sm text-slate-600">{msg}</span>}
      </div>
    </main>
  );
}
