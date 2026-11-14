// 集群 (admins): the clusters this console manages. A cluster is
// registered here, joins with the token shown once, and from then on its
// agent keeps the connection.
import { useAutoRefresh } from "../../lib/autoRefresh";
import { RefreshControl } from "../../design/RefreshControl";
import { useState } from "react";
import * as Dialog from "@radix-ui/react-dialog";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Badge } from "../../design/Badge";
import { Button } from "../../design/Button";
import { ConfirmDialog } from "../../design/ConfirmDialog";
import { CopyBlock } from "../../design/Copy";
import { EmptyState } from "../../design/EmptyState";
import { Field, Input, Notice, Select } from "../../design/Form";
import { GatewaysPanel, useAdminGateways } from "./GatewaysPanel";
import { Rtt, Since, Traffic, ago } from "./links";
import {
  createCluster,
  deleteCluster,
  listAdminClusters,
  patchCluster,
  reissueClusterToken,
  setClusterGateway,
  validClusterName,
  type AdminCluster,
  type Joining,
} from "./api";

/** Connection: since when its agent's link is up, or when it was last seen. */
function Connection({ c }: { c: AdminCluster }) {
  if (c.link) return <Since l={c.link} />;
  if (c.connected && !c.paused) return <span className="text-muted" title="这次回答的控制台副本没有它的连接；刷新可能换到另一个副本">在另一个控制台副本上</span>;
  return <span className="text-muted">{c.lastSeen ? `最后在线 ${ago(c.lastSeen)}` : "—"}</span>;
}

function State({ c }: { c: AdminCluster }) {
  if (c.paused) return <Badge tone="idle">已暂停</Badge>;
  if (!c.joined) return <Badge tone="warm" title={c.tokenExpires ? `加入令牌 ${new Date(c.tokenExpires).toLocaleString("zh-CN")} 前有效` : undefined}>等待加入</Badge>;
  if (c.connected) return <Badge tone="run">已连接</Badge>;
  return <Badge tone="fail">离线</Badge>;
}

const errText = (e: unknown) => (e instanceof Error ? e.message : String(e));

type Ask = { kind: "pause" | "resume" | "remove" | "reissue"; cluster: AdminCluster };

export function ClustersPage() {
  const qc = useQueryClient();
  const [every, setEvery] = useAutoRefresh("clusters", 10_000);
  const q = useQuery({ queryKey: ["admin", "clusters"], queryFn: listAdminClusters, refetchInterval: every || false });
  const [creating, setCreating] = useState(false);
  const [joining, setJoining] = useState<Joining | null>(null);
  const [ask, setAsk] = useState<Ask | null>(null);
  const [error, setError] = useState("");
  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ["admin", "clusters"] });
    void qc.invalidateQueries({ queryKey: ["clusters"] });
  };
  const act = useMutation({
    mutationFn: async (a: Ask): Promise<Joining | null> => {
      const n = a.cluster.name;
      if (a.kind === "pause") await patchCluster(n, { paused: true });
      if (a.kind === "resume") await patchCluster(n, { paused: false });
      if (a.kind === "remove") await deleteCluster(n);
      if (a.kind === "reissue") return reissueClusterToken(n);
      return null;
    },
    onSuccess: (j) => {
      setError("");
      if (j) setJoining(j);
    },
    onError: (e) => setError(errText(e)),
    onSettled: refresh,
  });

  const gw = useAdminGateways(every); // the gateways below refresh with the clusters
  const gateways = gw.data?.items ?? [];
  const gateway = useMutation({
    mutationFn: (v: { cluster: string; gateway: string }) => setClusterGateway(v.cluster, v.gateway),
    onError: (e) => setError(errText(e)),
    onSettled: refresh,
  });
  if (q.isError) return <EmptyState tone="error" title="无法加载集群" hint={errText(q.error)} />;
  const items = q.data?.items ?? [];
  return (
    <div className="space-y-3 p-3">
      <header className="flex flex-wrap items-center gap-2">
        <p className="min-w-0 flex-1 text-xs text-muted">各集群里的 agent 主动连到本控制台；这里登记集群、发放加入令牌。</p>
        <RefreshControl
          every={every}
          onEvery={setEvery}
          onRefresh={() => {
            void q.refetch();
            void gw.refetch();
          }}
          busy={q.isFetching || gw.isFetching}
        />
        <Button variant="primary" onClick={() => setCreating(true)}>
          新建集群
        </Button>
      </header>
      {error && <Notice>{error}</Notice>}
      {q.data && items.length === 0 ? (
        <EmptyState title="还没有集群" hint="新建一个集群，然后在该集群里用给出的命令安装 agent。" />
      ) : (
        <div className="overflow-x-auto rounded-md border border-line bg-surface">
          <table className="w-full text-left text-xs">
            <thead className="border-b border-line text-muted">
              <tr>
                {["名称", "显示名", "状态", "连接", "延迟", "流量（收 / 发）", "版本", "能力", "网关", ""].map((h) => (
                  <th key={h} className="px-3 py-2 font-medium">
                    {h}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {items.map((c) => (
                <tr key={c.name} className="border-b border-line last:border-0">
                  <td className="mono px-3 py-2">{c.name}</td>
                  <td className="px-3 py-2">{c.displayName || <span className="text-faint">—</span>}</td>
                  <td className="px-3 py-2">
                    <State c={c} />
                  </td>
                  <td className="px-3 py-2 whitespace-nowrap">
                    <Connection c={c} />
                  </td>
                  <td className="px-3 py-2">
                    <Rtt l={c.link} />
                  </td>
                  <td className="px-3 py-2">
                    <Traffic l={c.link} />
                  </td>
                  <td className="px-3 py-2 text-muted">
                    {c.agentVersion ? (
                      <span title={`controller ${c.controllerVersion || "?"} · Kubernetes ${c.kubernetesVersion || "?"}`}>agent {c.agentVersion}</span>
                    ) : (
                      "—"
                    )}
                  </td>
                  <td className="px-3 py-2">
                    <span className="flex flex-wrap gap-1">
                      {(c.capabilities ?? []).map((cap) => (
                        <Badge key={cap}>{cap}</Badge>
                      ))}
                    </span>
                  </td>
                  <td className="px-3 py-2">
                    <Select
                      aria-label={`${c.name} 的网关`}
                      value={c.gateway ?? ""}
                      onChange={(e) => gateway.mutate({ cluster: c.name, gateway: e.target.value })}
                      className="!w-auto"
                    >
                      <option value="">{gateways.length === 1 ? `默认（${gateways[0].name}）` : "默认"}</option>
                      {gateways.map((g) => (
                        <option key={g.name} value={g.name}>
                          {g.displayName || g.name}
                        </option>
                      ))}
                    </Select>
                  </td>
                  <td className="px-3 py-2">
                    <span className="flex justify-end gap-1">
                      <Button variant="ghost" onClick={() => setAsk({ kind: c.paused ? "resume" : "pause", cluster: c })}>
                        {c.paused ? "恢复" : "暂停"}
                      </Button>
                      <Button variant="ghost" onClick={() => setAsk({ kind: "reissue", cluster: c })}>
                        重新签发
                      </Button>
                      <Button variant="danger" onClick={() => setAsk({ kind: "remove", cluster: c })}>
                        移除
                      </Button>
                    </span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <GatewaysPanel every={every} />

      <CreateDialog
        open={creating}
        onOpenChange={setCreating}
        onCreated={(j) => {
          setCreating(false);
          setJoining(j);
          refresh();
        }}
      />
      <JoinDialog joining={joining} onClose={() => setJoining(null)} />

      <ConfirmDialog
        open={ask?.kind === "pause"}
        onOpenChange={(o) => !o && setAsk(null)}
        title={`暂停集群 ${ask?.cluster.name ?? ""}`}
        confirmLabel="暂停"
        onConfirm={() => ask && (act.mutate(ask), setAsk(null))}
      >
        它的 agent 会被断开并拒绝重连；集群里的 DevPod 照常运行，但在控制台里只能看到最后的状态，不能修改。
      </ConfirmDialog>
      <ConfirmDialog
        open={ask?.kind === "resume"}
        onOpenChange={(o) => !o && setAsk(null)}
        title={`恢复集群 ${ask?.cluster.name ?? ""}`}
        confirmLabel="恢复"
        onConfirm={() => ask && (act.mutate(ask), setAsk(null))}
      >
        重新接受它的 agent 连接。
      </ConfirmDialog>
      <ConfirmDialog
        open={ask?.kind === "reissue"}
        onOpenChange={(o) => !o && setAsk(null)}
        title={`为 ${ask?.cluster.name ?? ""} 重新签发加入令牌`}
        confirmLabel="重新签发"
        danger
        onConfirm={() => ask && (act.mutate(ask), setAsk(null))}
      >
        现有 agent 将不再被接受，直到用新令牌重新加入。用于更换 agent 或其密钥丢失时。
      </ConfirmDialog>
      <ConfirmDialog
        open={ask?.kind === "remove"}
        onOpenChange={(o) => !o && setAsk(null)}
        title={`移除集群 ${ask?.cluster.name ?? ""}`}
        confirmLabel="移除"
        requireText={ask?.cluster.name}
        danger
        onConfirm={() => ask && (act.mutate(ask), setAsk(null))}
      >
        控制台不再管理这个集群；集群里的 DevPod 不会被删除。
      </ConfirmDialog>
    </div>
  );
}

function CreateDialog({ open, onOpenChange, onCreated }: { open: boolean; onOpenChange: (o: boolean) => void; onCreated: (j: Joining) => void }) {
  const [name, setName] = useState("");
  const [displayName, setDisplayName] = useState("");
  const create = useMutation({
    mutationFn: () => createCluster(name, displayName),
    onSuccess: (j) => {
      setName("");
      setDisplayName("");
      onCreated(j);
    },
  });
  const bad = name !== "" && !validClusterName(name);
  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/30" />
        <Dialog.Content
          aria-describedby={undefined}
          className="fixed left-1/2 top-1/4 z-50 w-[min(92vw,440px)] -translate-x-1/2 space-y-3 rounded-lg border border-line bg-surface p-4 shadow-xl"
        >
          <Dialog.Title className="text-sm font-semibold">新建集群</Dialog.Title>
          <Field label="名称" hint="小写字母、数字和连字符，最多 32 个字符；两个集群有同名 DevPod 时，SSH 登录名里用它区分。" error={bad ? "名称不合法（不能是 devpod）" : undefined}>
            <Input aria-label="名称" value={name} onChange={(e) => setName(e.target.value.trim())} autoFocus />
          </Field>
          <Field label="显示名">
            <Input aria-label="显示名" value={displayName} onChange={(e) => setDisplayName(e.target.value)} />
          </Field>
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

function JoinDialog({ joining, onClose }: { joining: Joining | null; onClose: () => void }) {
  return (
    <Dialog.Root open={!!joining} onOpenChange={(o) => !o && onClose()}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-40 bg-black/30" />
        <Dialog.Content
          aria-describedby={undefined}
          className="fixed left-1/2 top-1/4 z-50 w-[min(94vw,640px)] -translate-x-1/2 space-y-3 rounded-lg border border-line bg-surface p-4 shadow-xl"
        >
          <Dialog.Title className="text-sm font-semibold">让 {joining?.cluster.name} 加入</Dialog.Title>
          <p className="text-xs text-muted">在该集群里运行下面的命令。加入令牌 24 小时内有效、只能用一次，并且只显示这一次。</p>
          {joining && <CopyBlock value={joining.command} />}
          <div className="flex justify-end">
            <Button variant="primary" onClick={onClose}>
              我已保存
            </Button>
          </div>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
