// 网关 (admins): the gateways that serve several clusters. One is
// registered here, joins with the token shown once, and from then on asks
// the console who may log in where; the clusters' agents connect to it.
import { Fragment, useState } from "react";
import * as Dialog from "@radix-ui/react-dialog";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Badge } from "../../design/Badge";
import { Button } from "../../design/Button";
import { ConfirmDialog } from "../../design/ConfirmDialog";
import { CopyBlock } from "../../design/Copy";
import { Field, Input, Notice } from "../../design/Form";
import { fmtBytes } from "../../lib/format";
import { Rtt, Since, Traffic, ago } from "./links";
import {
  createGateway,
  deleteGateway,
  listAdminGateways,
  patchGateway,
  reissueGatewayToken,
  validClusterName,
  type AdminGateway,
  type GatewayFields,
  type GatewayJoining,
  type GatewayReplica,
  type LinkStats,
} from "./api";

export const gatewaysKey = ["admin", "gateways"] as const;
export const useAdminGateways = (every = 10_000) => useQuery({ queryKey: gatewaysKey, queryFn: listAdminGateways, refetchInterval: every || false, retry: false });

const errText = (e: unknown) => (e instanceof Error ? e.message : String(e));

/** a gateway that asked the console something within the last two minutes is there */
const online = (g: AdminGateway) => !!g.lastSeen && Date.now() - new Date(g.lastSeen).getTime() < 120_000;

function State({ g }: { g: AdminGateway }) {
  if (g.paused) return <Badge tone="idle">已暂停</Badge>;
  if (!g.joined) return <Badge tone="warm">等待加入</Badge>;
  return online(g) ? <Badge tone="run">在线</Badge> : <Badge tone="fail">离线</Badge>;
}

type Ask = { kind: "pause" | "resume" | "remove" | "reissue"; gateway: AdminGateway };

/** clusterLinks: each cluster's link to the gateway, from the replicas that said (the first that has it). */
function clusterLinks(g: AdminGateway): Map<string, LinkStats> {
  const out = new Map<string, LinkStats>();
  for (const r of g.replicas ?? []) for (const c of r.status?.clusters ?? []) if (!out.has(c.name)) out.set(c.name, c);
  return out;
}
const sessionCount = (g: AdminGateway) => (g.replicas ?? []).reduce((n, r) => n + (r.status?.sessions.length ?? 0), 0);

const th = "px-2 py-1 font-medium";
const td = "px-2 py-1";

/** Replica: one replica of a gateway — its link to this console, the clusters connected to it, the sessions it carries. */
function Replica({ r }: { r: GatewayReplica }) {
  const st = r.status;
  const sessions = st?.sessions ?? [];
  return (
    <div className="space-y-2 rounded-md border border-line bg-sunk/40 p-2">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        <span className="mono font-medium">{r.pod}</span>
        {st?.version && <span className="text-muted">{st.version}</span>}
        <span className="text-muted">
          到控制台 <Since l={r.link} /> · 延迟 <Rtt l={r.link} /> · 收 / 发 <Traffic l={r.link} />
        </span>
      </div>
      {!st ? (
        <p className="text-muted">这个副本没有给出它的状态{r.error ? `：${r.error}` : ""}（网关升级到 v0.9.23 以上才有）。</p>
      ) : (
        <>
          <div className="overflow-x-auto">
            <table className="w-full min-w-[28rem] text-left">
              <caption className="pb-1 text-left text-muted">已连上的集群（{st.clusters.length}）</caption>
              <thead className="text-muted">
                <tr>
                  {["集群", "连接", "延迟", "流量（收 / 发）", "流"].map((h) => (
                    <th key={h} className={th}>
                      {h}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {st.clusters.map((c) => (
                  <tr key={c.name} className="border-t border-line">
                    <td className={`mono ${td}`}>{c.name}</td>
                    <td className={`${td} whitespace-nowrap`}>
                      <Since l={c} />
                    </td>
                    <td className={td}>
                      <Rtt l={c} />
                    </td>
                    <td className={td}>
                      <Traffic l={c} />
                    </td>
                    <td className={`mono ${td}`}>{c.streams}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <div className="overflow-x-auto">
            <table className="w-full min-w-[40rem] text-left">
              <caption className="pb-1 text-left text-muted">当前会话（{sessions.length}）</caption>
              {sessions.length > 0 && (
                <thead className="text-muted">
                  <tr>
                    {["用户", "DevPod", "集群", "客户端", "公钥", "开始", "上行 / 下行"].map((h) => (
                      <th key={h} className={th}>
                        {h}
                      </th>
                    ))}
                  </tr>
                </thead>
              )}
              <tbody>
                {sessions.map((x) => (
                  <tr key={x.id} className="border-t border-line">
                    <td className={`mono ${td}`}>{x.user}</td>
                    <td className={`mono ${td}`}>{x.devpod}</td>
                    <td className={`mono ${td}`}>{x.cluster || "—"}</td>
                    <td className={`mono ${td}`} title={x.kind ? `认证 ${x.kind}` : undefined}>
                      {x.client === "console" ? "网页终端" : x.client}
                    </td>
                    <td className={`mono ${td} text-muted`} title={x.pubkeyFp}>
                      {x.pubkeyFp ? x.pubkeyFp.replace(/^SHA256:/, "").slice(0, 10) + "…" : "—"}
                    </td>
                    <td className={`${td} whitespace-nowrap`} title={new Date(x.since).toLocaleString("zh-CN")}>
                      {ago(x.since)}
                    </td>
                    <td className={`mono ${td} whitespace-nowrap`}>
                      {fmtBytes(x.up)} / {fmtBytes(x.down)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}
    </div>
  );
}

/** every: how often it refreshes — the 集群 page's 自动刷新 */
export function GatewaysPanel({ every = 10_000 }: { every?: number }) {
  const qc = useQueryClient();
  const q = useAdminGateways(every);
  const [creating, setCreating] = useState(false);
  const [joining, setJoining] = useState<GatewayJoining | null>(null);
  const [ask, setAsk] = useState<Ask | null>(null);
  const [error, setError] = useState("");
  const refresh = () => void qc.invalidateQueries({ queryKey: gatewaysKey });
  const act = useMutation({
    mutationFn: async (a: Ask): Promise<GatewayJoining | null> => {
      const n = a.gateway.name;
      if (a.kind === "pause") await patchGateway(n, { paused: true });
      if (a.kind === "resume") await patchGateway(n, { paused: false });
      if (a.kind === "remove") await deleteGateway(n);
      if (a.kind === "reissue") return reissueGatewayToken(n);
      return null;
    },
    onSuccess: (j) => {
      setError("");
      if (j) setJoining(j);
    },
    onError: (e) => setError(errText(e)),
    onSettled: refresh,
  });
  const items = q.data?.items ?? [];
  const [open, setOpen] = useState<ReadonlySet<string>>(new Set());
  const toggle = (n: string) =>
    setOpen((cur) => {
      const next = new Set(cur);
      if (!next.delete(n)) next.add(n);
      return next;
    });
  return (
    <section aria-label="网关" className="space-y-2">
      <header className="flex flex-wrap items-center gap-2">
        <h2 className="text-sm font-semibold">网关</h2>
        <p className="min-w-0 flex-1 text-xs text-muted">用户经网关 SSH 和访问端口链接；一个网关可以服务多个集群（各集群的 agent 主动连到它）。</p>
        <Button onClick={() => setCreating(true)}>新建网关</Button>
      </header>
      {error && <Notice>{error}</Notice>}
      {items.length > 0 && (
        <div className="overflow-x-auto rounded-md border border-line bg-surface">
          <table className="w-full text-left text-xs">
            <thead className="border-b border-line text-muted">
              <tr>
                {["名称", "SSH 地址", "端口链接域名", "状态", "已连上的集群", "会话", ""].map((h) => (
                  <th key={h} className="px-3 py-2 font-medium">
                    {h}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {items.map((g) => {
                const links = clusterLinks(g);
                const expandable = (g.replicas?.length ?? 0) > 0;
                return (
                <Fragment key={g.name}>
                <tr className="border-b border-line last:border-0">
                  <td className="px-3 py-2">
                    {expandable ? (
                      <button
                        type="button"
                        aria-expanded={open.has(g.name)}
                        onClick={() => toggle(g.name)}
                        className="inline-flex items-center gap-1 hover:text-accent"
                        title="展开：副本、已连上的集群、当前会话"
                      >
                        <span aria-hidden className="w-3 text-muted">{open.has(g.name) ? "▾" : "▸"}</span>
                        <span className="mono">{g.name}</span>
                      </button>
                    ) : (
                      <span className="mono">{g.name}</span>
                    )}
                    {g.displayName && <span className="ml-2 text-muted">{g.displayName}</span>}
                    {expandable && <span className="ml-2 text-faint">{g.replicas!.length} 个副本</span>}
                  </td>
                  <td className="mono px-3 py-2">{g.sshAddress || <span className="font-sans text-faint">—</span>}</td>
                  <td className="mono px-3 py-2">{g.httpBaseDomain || <span className="font-sans text-faint">—</span>}</td>
                  <td className="px-3 py-2" title={g.version ? `版本 ${g.version}` : undefined}>
                    <State g={g} />
                  </td>
                  <td className="px-3 py-2">
                    <span className="flex flex-wrap gap-1">
                      {(g.clusters ?? []).map((c) => {
                        const l = links.get(c);
                        return (
                          <Badge key={c} title={l ? `连到网关：收 ${fmtBytes(l.rxRate)}/s · 发 ${fmtBytes(l.txRate)}/s` : undefined}>
                            {c}
                            {l?.rttMs ? <span className="ml-1 text-muted">{l.rttMs < 10 ? l.rttMs.toFixed(1) : Math.round(l.rttMs)}ms</span> : null}
                          </Badge>
                        );
                      })}
                    </span>
                  </td>
                  <td className="mono px-3 py-2">{g.replicas?.some((r) => r.status) ? sessionCount(g) : <span className="font-sans text-faint">—</span>}</td>
                  <td className="px-3 py-2">
                    <span className="flex justify-end gap-1">
                      <Button variant="ghost" onClick={() => setAsk({ kind: g.paused ? "resume" : "pause", gateway: g })}>
                        {g.paused ? "恢复" : "暂停"}
                      </Button>
                      <Button variant="ghost" onClick={() => setAsk({ kind: "reissue", gateway: g })}>
                        重新签发
                      </Button>
                      <Button variant="danger" onClick={() => setAsk({ kind: "remove", gateway: g })}>
                        移除
                      </Button>
                    </span>
                  </td>
                </tr>
                {open.has(g.name) && (
                  <tr className="border-b border-line last:border-0">
                    <td colSpan={7} className="space-y-2 px-3 pb-3">
                      {(g.replicas ?? []).map((r) => (
                        <Replica key={r.pod} r={r} />
                      ))}
                    </td>
                  </tr>
                )}
                </Fragment>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
      <CreateGateway
        open={creating}
        onOpenChange={setCreating}
        onCreated={(j) => {
          setCreating(false);
          setJoining(j);
          refresh();
        }}
      />
      <Dialog.Root open={!!joining} onOpenChange={(o) => !o && setJoining(null)}>
        <Dialog.Portal>
          <Dialog.Overlay className="fixed inset-0 z-40 bg-black/30" />
          <Dialog.Content
            aria-describedby={undefined}
            className="fixed left-1/2 top-1/4 z-50 w-[min(94vw,640px)] -translate-x-1/2 space-y-3 rounded-lg border border-line bg-surface p-4 shadow-xl"
          >
            <Dialog.Title className="text-sm font-semibold">让网关 {joining?.gateway.name} 加入</Dialog.Title>
            <p className="text-xs text-muted">在安装网关的地方运行下面的命令。加入令牌 24 小时内有效、只能用一次，并且只显示这一次。</p>
            {joining && <CopyBlock value={joining.command} />}
            <div className="flex justify-end">
              <Button variant="primary" onClick={() => setJoining(null)}>
                我已保存
              </Button>
            </div>
          </Dialog.Content>
        </Dialog.Portal>
      </Dialog.Root>
      <ConfirmDialog
        open={ask?.kind === "pause"}
        onOpenChange={(o) => !o && setAsk(null)}
        title={`暂停网关 ${ask?.gateway.name ?? ""}`}
        confirmLabel="暂停"
        onConfirm={() => ask && (act.mutate(ask), setAsk(null))}
      >
        控制台不再回答它，各集群的 agent 也会离开它：经它的新登录和端口链接都会失败。
      </ConfirmDialog>
      <ConfirmDialog
        open={ask?.kind === "resume"}
        onOpenChange={(o) => !o && setAsk(null)}
        title={`恢复网关 ${ask?.gateway.name ?? ""}`}
        confirmLabel="恢复"
        onConfirm={() => ask && (act.mutate(ask), setAsk(null))}
      />
      <ConfirmDialog
        open={ask?.kind === "reissue"}
        onOpenChange={(o) => !o && setAsk(null)}
        title={`为网关 ${ask?.gateway.name ?? ""} 重新签发加入令牌`}
        confirmLabel="重新签发"
        danger
        onConfirm={() => ask && (act.mutate(ask), setAsk(null))}
      >
        它现有的密钥将不再被接受，直到用新令牌重新加入。
      </ConfirmDialog>
      <ConfirmDialog
        open={ask?.kind === "remove"}
        onOpenChange={(o) => !o && setAsk(null)}
        title={`移除网关 ${ask?.gateway.name ?? ""}`}
        confirmLabel="移除"
        requireText={ask?.gateway.name}
        danger
        onConfirm={() => ask && (act.mutate(ask), setAsk(null))}
      >
        指向它的集群将改用控制台自己的入口设置。
      </ConfirmDialog>
    </section>
  );
}

function CreateGateway({ open, onOpenChange, onCreated }: { open: boolean; onOpenChange: (o: boolean) => void; onCreated: (j: GatewayJoining) => void }) {
  const [name, setName] = useState("");
  const [f, setF] = useState({ displayName: "", sshAddress: "", httpBaseDomain: "", agentAddress: "" });
  const create = useMutation({
    mutationFn: () => {
      const fields: GatewayFields = {};
      for (const [k, v] of Object.entries(f)) if (v.trim()) (fields as Record<string, string>)[k] = v.trim();
      return createGateway(name, fields);
    },
    onSuccess: (j) => {
      setName("");
      setF({ displayName: "", sshAddress: "", httpBaseDomain: "", agentAddress: "" });
      onCreated(j);
    },
  });
  const bad = name !== "" && !validClusterName(name);
  const rows: [keyof typeof f, string, string][] = [
    ["displayName", "显示名", ""],
    ["sshAddress", "SSH 地址", "用户连接的地址，如 ssh.example.com:22"],
    ["httpBaseDomain", "端口链接域名", "如 apps.example.com；需要泛域名解析和证书"],
    ["agentAddress", "agent 连接地址", "各集群 agent 连到网关的地址，如 https://gw.example.com"],
  ];
  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/30" />
        <Dialog.Content
          aria-describedby={undefined}
          className="fixed left-1/2 top-[12%] z-50 w-[min(92vw,480px)] -translate-x-1/2 space-y-3 rounded-lg border border-line bg-surface p-4 shadow-xl"
        >
          <Dialog.Title className="text-sm font-semibold">新建网关</Dialog.Title>
          <Field label="名称" error={bad ? "名称不合法" : undefined}>
            <Input aria-label="名称" value={name} onChange={(e) => setName(e.target.value.trim())} autoFocus />
          </Field>
          {rows.map(([k, label, hint]) => (
            <Field key={k} label={label} hint={hint || undefined}>
              <Input aria-label={label} value={f[k]} onChange={(e) => setF({ ...f, [k]: e.target.value })} />
            </Field>
          ))}
          {create.isError && <Notice>{errText(create.error)}</Notice>}
          <div className="flex justify-end gap-2">
            <Dialog.Close asChild>
              <Button>取消</Button>
            </Dialog.Close>
            <Button variant="primary" disabled={name === "" || bad || create.isPending} onClick={() => create.mutate()}>
              创建
            </Button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
