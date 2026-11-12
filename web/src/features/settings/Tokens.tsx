// 访问令牌 (设置): personal access tokens for the REST API. A token acts
// as its owner within the scopes picked here and, optionally, only on
// the DevPods named; the token itself is shown once.
import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { createToken, deleteToken, listTokens, type TokenScope, type TokenView } from "../../data/api";
import { useDevPods } from "../../data/queries";
import { useClusters } from "../clusters/ClusterSwitcher";
import { Badge } from "../../design/Badge";
import { Button } from "../../design/Button";
import { ConfirmDialog } from "../../design/ConfirmDialog";
import { CopyBlock } from "../../design/Copy";
import { Field, Input, Notice, Select } from "../../design/Form";

const EXPIRY = [
  [7, "7 天"],
  [30, "30 天"],
  [90, "90 天"],
  [365, "1 年"],
  [0, "永不过期"],
] as const;

function groups(scopes: TokenScope[]): [string, TokenScope[]][] {
  const m = new Map<string, TokenScope[]>();
  for (const s of scopes) m.set(s.group, [...(m.get(s.group) ?? []), s]);
  return [...m];
}

function CreateForm({ scopes, onDone }: { scopes: TokenScope[]; onDone: (token: string) => void }) {
  const qc = useQueryClient();
  const mine = useDevPods("mine").data?.items ?? [];
  const [name, setName] = useState("");
  const [picked, setPicked] = useState<Set<string>>(new Set(["devpods:read"]));
  const [limit, setLimit] = useState(false);
  const [devpods, setDevpods] = useState<Set<string>>(new Set());
  const clusterList = useClusters().data?.items ?? [];
  const [limitClusters, setLimitClusters] = useState(false);
  const [clusters, setClusters] = useState<Set<string>>(new Set());
  const [days, setDays] = useState(30);
  const toggle = (set: Set<string>, id: string) => {
    const n = new Set(set);
    if (!n.delete(id)) n.add(id);
    return n;
  };
  const create = useMutation({
    mutationFn: () => createToken({ name: name.trim(), scopes: [...picked], devpods: limit ? [...devpods] : [], expiresInDays: days, ...(limitClusters ? { clusters: [...clusters] } : {}) }),
    onSuccess: (r) => {
      void qc.invalidateQueries({ queryKey: ["tokens"] });
      onDone(r.token);
    },
  });
  const admin = [...picked].some((s) => s.startsWith("admin:"));
  const ok = name.trim() !== "" && picked.size > 0 && (!limit || (devpods.size > 0 && !admin)) && (!limitClusters || clusters.size > 0);
  return (
    <form
      className="space-y-3 rounded-md border border-line p-3"
      onSubmit={(e) => {
        e.preventDefault();
        if (ok) create.mutate();
      }}
    >
      <div className="grid gap-3 sm:grid-cols-[minmax(0,1fr)_10rem]">
        <Field label="名称" hint="用来辨认这个令牌，例如 ci 或 my-agent">
          <Input aria-label="名称" value={name} onChange={(e) => setName(e.target.value)} maxLength={64} required />
        </Field>
        <Field label="有效期">
          <Select aria-label="有效期" value={days} onChange={(e) => setDays(Number(e.target.value))}>
            {EXPIRY.map(([d, l]) => (
              <option key={d} value={d}>
                {l}
              </option>
            ))}
          </Select>
        </Field>
      </div>
      <fieldset className="space-y-2">
        <legend className="text-xs font-medium">权限</legend>
        <p className="text-[11px] text-muted">只勾选需要的；没勾选的操作一律拒绝。</p>
        <div className="grid gap-x-6 gap-y-2 sm:grid-cols-2">
          {groups(scopes).map(([g, items]) => (
            <div key={g}>
              <div className="mb-0.5 text-[11px] text-faint">{g}</div>
              {items.map((s) => (
                <label key={s.id} className="flex items-start gap-1.5 py-0.5 text-xs">
                  <input type="checkbox" className="mt-0.5" checked={picked.has(s.id)} onChange={() => setPicked(toggle(picked, s.id))} aria-label={s.id} />
                  <span>
                    {s.label} <code className="mono text-[11px] text-faint">{s.id}</code>
                  </span>
                </label>
              ))}
            </div>
          ))}
        </div>
      </fieldset>
      <fieldset className="space-y-1">
        <legend className="text-xs font-medium">范围</legend>
        <label className="flex items-center gap-1.5 text-xs">
          <input type="checkbox" checked={limit} onChange={(e) => setLimit(e.target.checked)} />
          只允许操作指定的 DevPod
        </label>
        {limit && (
          <div className="flex flex-wrap gap-x-4 gap-y-1 pl-5">
            {mine.map((d) => (
              <label key={d.metadata.name} className="mono flex items-center gap-1.5 text-xs">
                <input type="checkbox" checked={devpods.has(d.metadata.name)} onChange={() => setDevpods(toggle(devpods, d.metadata.name))} />
                {d.metadata.name}
              </label>
            ))}
            {mine.length === 0 && <span className="text-xs text-faint">你还没有 DevPod</span>}
          </div>
        )}
        {limit && admin && <p className="pl-5 text-[11px] text-fail">限定 DevPod 的令牌不能带管理员权限。</p>}
        {limit && <p className="pl-5 text-[11px] text-muted">限定后，这个令牌不能新建 DevPod，也不能访问审计、公钥等不属于某个 DevPod 的接口。</p>}
        {clusterList.length > 1 && (
          <>
            <label className="flex items-center gap-1.5 text-xs">
              <input type="checkbox" checked={limitClusters} onChange={(e) => setLimitClusters(e.target.checked)} />
              只允许访问指定的集群
            </label>
            {limitClusters && (
              <div className="flex flex-wrap gap-x-4 gap-y-1 pl-5">
                {clusterList.map((c) => (
                  <label key={c.name} className="flex items-center gap-1.5 text-xs">
                    <input type="checkbox" aria-label={`集群 ${c.name}`} checked={clusters.has(c.name)} onChange={() => setClusters(toggle(clusters, c.name))} />
                    {c.displayName || c.name}
                  </label>
                ))}
              </div>
            )}
          </>
        )}
      </fieldset>
      {create.isError && <Notice>{create.error instanceof Error ? create.error.message : String(create.error)}</Notice>}
      <div className="flex justify-end">
        <Button type="submit" variant="primary" disabled={!ok || create.isPending}>
          创建令牌
        </Button>
      </div>
    </form>
  );
}

const fmtDay = (iso: string) => new Date(iso).toLocaleDateString();

export function Tokens() {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: ["tokens"], queryFn: listTokens });
  const [creating, setCreating] = useState(false);
  const [fresh, setFresh] = useState<string | null>(null);
  const [revoke, setRevoke] = useState<TokenView | null>(null);
  const del = useMutation({ mutationFn: (id: string) => deleteToken(id), onSuccess: () => void qc.invalidateQueries({ queryKey: ["tokens"] }) });
  if (!q.data) return <p className="p-3 text-xs text-faint">{q.isError ? "加载失败" : "加载中…"}</p>;
  return (
    <div className="space-y-3 p-3 text-xs">
      <p className="text-muted">
        令牌让脚本和 agent 调用 REST API：<code className="mono text-ink">Authorization: Bearer dpt_…</code>。接口列表和每个接口需要的权限见{" "}
        <a href="/api/openapi.json" target="_blank" rel="noreferrer" className="text-accent hover:underline">
          /api/openapi.json
        </a>
        。
      </p>
      {fresh && (
        <div className="space-y-1.5 rounded-md border border-line bg-accent-soft p-3">
          <p className="font-medium">令牌已创建。它只显示这一次，请现在复制保存。</p>
          <CopyBlock value={fresh} />
          <Button onClick={() => setFresh(null)}>我已保存</Button>
        </div>
      )}
      {q.data.items.length > 0 && (
        <table className="w-full border-collapse">
          <thead>
            <tr className="h-6 border-b border-line text-left text-[11px] text-muted">
              <th className="font-normal">名称</th>
              <th className="font-normal">权限</th>
              <th className="font-normal">范围</th>
              <th className="font-normal">到期</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {q.data.items.map((t) => (
              <tr key={t.id} className="border-b border-line align-top last:border-0">
                <td className="py-1.5 pr-3">
                  <div className="font-medium">{t.name}</div>
                  <div className="mono text-[11px] text-faint">dpt_{t.id}_…</div>
                </td>
                <td className="py-1.5 pr-3">
                  <div className="flex flex-wrap gap-1">
                    {t.scopes.map((s) => (
                      <Badge key={s} tone={s.startsWith("admin:") ? "warm" : "neutral"}>
                        {s}
                      </Badge>
                    ))}
                  </div>
                </td>
                <td className="mono py-1.5 pr-3">
                  {t.devpods.length ? t.devpods.join(", ") : <span className="font-sans text-muted">全部</span>}
                  {!!t.clusters?.length && (
                    <div className="font-sans text-[11px] text-muted">
                      集群：<span className="mono">{t.clusters.join(", ")}</span>
                    </div>
                  )}
                </td>
                <td className="py-1.5 pr-3 whitespace-nowrap">{t.expired ? <Badge tone="fail">已过期</Badge> : t.expiresAt ? fmtDay(t.expiresAt) : "永不"}</td>
                <td className="py-1.5 text-right">
                  <button type="button" onClick={() => setRevoke(t)} className="text-muted hover:text-fail" aria-label={`撤销 ${t.name}`}>
                    撤销
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {creating ? (
        <CreateForm
          scopes={q.data.scopes}
          onDone={(token) => {
            setCreating(false);
            setFresh(token);
          }}
        />
      ) : (
        <Button onClick={() => setCreating(true)}>新建令牌</Button>
      )}
      <ConfirmDialog
        open={!!revoke}
        onOpenChange={(o) => !o && setRevoke(null)}
        title={`撤销令牌 ${revoke?.name ?? ""}？`}
        confirmLabel="撤销"
        danger
        onConfirm={() => {
          if (revoke) del.mutate(revoke.id);
          setRevoke(null);
        }}
      >
        使用这个令牌的脚本会立即失效。
      </ConfirmDialog>
    </div>
  );
}
