// 设置 (UI spec §5.11): SSH keys (read-only when keys are managed
// elsewhere) and the password.
import { useEffect, useState } from "react";
import { changePassword, getPubkeys, putPubkeys, sshConfig, ApiFailure } from "../../data/api";
import { useMe } from "../../data/queries";
import { Button } from "../../design/Button";
import { CopyBlock } from "../../design/Copy";
import { Field, Input, Notice, Textarea } from "../../design/Form";
import { Panel } from "../../design/Panel";

function Keys({ editable }: { editable: boolean }) {
  const [text, setText] = useState("");
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  useEffect(() => {
    getPubkeys()
      .then((r) => setText((r.pubkeys ?? []).join("\n")))
      .catch(() => {});
  }, []);
  const save = async () => {
    setMsg(null);
    try {
      const keys = text
        .split("\n")
        .map((l) => l.trim())
        .filter(Boolean);
      await putPubkeys(keys);
      setMsg({ ok: true, text: `已保存 ${keys.length} 个公钥` });
    } catch (e) {
      setMsg({ ok: false, text: e instanceof ApiFailure ? e.body.message : String(e) });
    }
  };
  return (
    <div className="space-y-2 p-3">
      {!editable && <Notice tone="idle">公钥由外部系统管理（如 LDAP），这里只读；需要修改请联系管理员。</Notice>}
      <Textarea
        aria-label="SSH 公钥"
        value={text}
        onChange={(e) => setText(e.target.value)}
        readOnly={!editable}
        rows={6}
        spellCheck={false}
        className="mono text-xs"
        placeholder="ssh-ed25519 AAAA… laptop（每行一个）"
      />
      {editable && (
        <div className="flex items-center gap-3">
          <Button variant="primary" onClick={save}>
            保存公钥
          </Button>
          {msg && <span className={`text-xs ${msg.ok ? "text-run" : "text-fail"}`}>{msg.text}</span>}
        </div>
      )}
    </div>
  );
}

function Password() {
  const [oldPw, setOld] = useState("");
  const [newPw, setNew] = useState("");
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null);
  const submit = async () => {
    setMsg(null);
    try {
      await changePassword(oldPw, newPw);
      setOld("");
      setNew("");
      setMsg({ ok: true, text: "密码已修改" });
    } catch (e) {
      setMsg({ ok: false, text: e instanceof ApiFailure ? e.body.message : String(e) });
    }
  };
  return (
    <div className="max-w-sm space-y-3 p-3">
      <Field label="当前密码">
        <Input type="password" value={oldPw} onChange={(e) => setOld(e.target.value)} autoComplete="current-password" />
      </Field>
      <Field label="新密码">
        <Input type="password" value={newPw} onChange={(e) => setNew(e.target.value)} autoComplete="new-password" />
      </Field>
      <div className="flex items-center gap-3">
        <Button variant="primary" onClick={submit} disabled={!oldPw || !newPw}>
          修改密码
        </Button>
        {msg && <span className={`text-xs ${msg.ok ? "text-run" : "text-fail"}`}>{msg.text}</span>}
      </div>
    </div>
  );
}

export function SettingsPage() {
  const me = useMe().data;
  if (!me) return null;
  return (
    <div className="max-w-3xl space-y-4 p-4">
      <Panel title="SSH 公钥">
        <Keys editable={me.features.pubkeySelfService} />
        <div className="space-y-1 border-t border-line p-3">
          <div className="text-xs text-muted">
            ~/.ssh/config 示例（每个 DevPod 的详情页有现成的片段）：
          </div>
          <CopyBlock value={sshConfig(me, me.user, "pod")} />
        </div>
      </Panel>
      <Panel title="密码">
        {!me.features.passwordAuth ? (
          <div className="p-3">
            <Notice tone="idle">本部署未启用密码登录。</Notice>
          </div>
        ) : me.hasPassword ? (
          <Password />
        ) : (
          <div className="p-3">
            <Notice tone="idle">你通过 GitLab 登录，没有可修改的密码。</Notice>
          </div>
        )}
      </Panel>
    </div>
  );
}
