// 镜像仓库凭证 (设置, snapshot spec v2 §5): the user's own registry
// logins. They pull private images for the user's DevPods and push when
// an image is saved to that registry. A password goes in once and is
// never shown again.
import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiFailure, deleteRegistry, listRegistries, putRegistry, type RegistryView } from "../../data/api";
import { Badge } from "../../design/Badge";
import { Button } from "../../design/Button";
import { ConfirmDialog } from "../../design/ConfirmDialog";
import { Field, Input, Notice } from "../../design/Form";
import { cleanRegistryHost, REGISTRY_HOST_RE } from "../../lib/registry";

export const registriesKey = ["registries"] as const;
const PENDING_POLL_MS = 2000;

/** useRegistries: the saved registries; while one is still being written
 *  to the cluster (pending) the list is refetched every 2 s, then not. */
export function useRegistries(enabled = true) {
  return useQuery({
    queryKey: registriesKey,
    queryFn: listRegistries,
    enabled,
    retry: false,
    refetchInterval: (q) => (q.state.data?.items.some((r) => r.pending) ? PENDING_POLL_MS : false),
  });
}

const errText = (e: unknown) => (e instanceof ApiFailure ? e.body.message : e instanceof Error ? e.message : String(e));

export function Registries() {
  const qc = useQueryClient();
  const q = useRegistries();
  const [host, setHost] = useState("");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [remove, setRemove] = useState<RegistryView | null>(null);
  const [missed, setMissed] = useState<string[]>([]);
  const refresh = () => void qc.invalidateQueries({ queryKey: registriesKey });
  const save = useMutation({
    mutationFn: (v: { host: string; username: string; password: string }) => putRegistry(v.host, { username: v.username, password: v.password }),
    onSuccess: (answer) => {
      setHost("");
      setUsername("");
      setPassword(""); // sent once; not kept in the page
      // with several clusters the credential goes to those connected right now
      setMissed(Object.entries(answer?.clusters ?? {}).flatMap(([c, state]) => (state === "ok" ? [] : [c])));
      refresh();
    },
  });
  const del = useMutation({ mutationFn: (h: string) => deleteRegistry(h), onSettled: refresh });

  const items = q.data?.items ?? [];
  const clean = cleanRegistryHost(host);
  const hostOK = REGISTRY_HOST_RE.test(clean);
  const ok = hostOK && username.trim() !== "" && password !== "";
  const replaces = hostOK && items.some((r) => r.host === clean);

  return (
    <div className="space-y-3 p-3 text-xs">
      <p className="text-muted">
        用于拉取你的 DevPod 用到的私有镜像，也用于「保存为镜像」时推送到同一个仓库。密码保存在集群里，之后不会再显示；添加或删除只影响之后启动的 DevPod。
      </p>
      {q.isError && <Notice>加载失败：{errText(q.error)}</Notice>}
      {missed.length > 0 && (
        <Notice tone="warm">凭证已保存，但没有写入这些集群（当时离线或写入失败）：{missed.join("、")}。它们恢复后请在这里重新保存一次。</Notice>
      )}
      {items.length > 0 && (
        <table className="w-full border-collapse" aria-label="已保存的仓库">
          <thead>
            <tr className="h-6 border-b border-line text-left text-[11px] text-muted">
              <th className="font-normal">仓库地址</th>
              <th className="font-normal">用户名</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {items.map((r) => (
              <tr key={r.host} className="h-8 border-b border-line last:border-0">
                <td className="mono pr-3">
                  {r.host}
                  {r.pending && (
                    <span className="ml-2 font-sans">
                      <Badge tone="warm" title="凭证正在写入集群，稍后生效">
                        待生效
                      </Badge>
                    </span>
                  )}
                </td>
                <td className="mono pr-3">{r.username}</td>
                <td className="text-right">
                  <button type="button" onClick={() => setRemove(r)} className="text-muted hover:text-fail" aria-label={`删除 ${r.host}`}>
                    删除
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {q.data && items.length === 0 && <p className="text-faint">还没有保存任何仓库凭证。</p>}
      <form
        className="space-y-3 rounded-md border border-line p-3"
        aria-label="添加仓库凭证"
        onSubmit={(e) => {
          e.preventDefault();
          if (ok) save.mutate({ host: clean, username: username.trim(), password });
        }}
      >
        <div className="grid gap-3 sm:grid-cols-3">
          <Field label="仓库地址" error={host && !hostOK ? "形如 harbor.example.com 或 registry.example.com:5000" : undefined}>
            <Input aria-label="仓库地址" className="mono" value={host} onChange={(e) => setHost(e.target.value)} onBlur={() => setHost(clean)} placeholder="harbor.example.com" autoComplete="off" required />
          </Field>
          <Field label="用户名">
            <Input aria-label="仓库用户名" className="mono" value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="off" required />
          </Field>
          <Field label="密码">
            <Input aria-label="仓库密码" type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="new-password" required />
          </Field>
        </div>
        {replaces && <p className="text-[11px] text-muted">已经保存过 {clean} 的凭证；再次添加会替换它。</p>}
        {save.isError && <Notice>保存失败：{errText(save.error)}</Notice>}
        {del.isError && <Notice>删除失败：{errText(del.error)}</Notice>}
        <div className="flex justify-end">
          <Button type="submit" variant="primary" disabled={!ok || save.isPending}>
            {replaces ? "替换凭证" : "添加凭证"}
          </Button>
        </div>
      </form>
      <ConfirmDialog
        open={!!remove}
        onOpenChange={(o) => !o && setRemove(null)}
        title={`删除 ${remove?.host ?? ""} 的凭证？`}
        confirmLabel="删除"
        danger
        onConfirm={() => {
          if (remove) del.mutate(remove.host);
          setRemove(null);
        }}
      >
        之后启动的 DevPod 不能再从这个仓库拉取私有镜像，「保存为镜像」推送到它时需要重新填写用户名和密码。正在运行的 DevPod 不受影响。
      </ConfirmDialog>
    </div>
  );
}
