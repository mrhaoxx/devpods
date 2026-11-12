// 端口: HTTP services of the DevPod published through the gateway as
// https://{id}-{port}… — the address shows a random id, never the DevPod
// or its owner. Each port has one of three access levels: private (owner,
// collaborators, admins), signed-in users (anyone logged in to the
// console) or public (anyone with the link).
import type { Ref } from "../../../data/cluster";
import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { getHttp, putHttp, type HttpView } from "../../../data/api";
import { useInfo } from "../../../data/queries";
import { Badge } from "../../../design/Badge";
import { Button } from "../../../design/Button";
import { ConfirmDialog } from "../../../design/ConfirmDialog";
import { useCopied } from "../../../design/Copy";
import { Input, Select } from "../../../design/Form";
import { Panel } from "../../../design/Panel";

type Listen = { port: number; proto: string; addr?: string; pid?: number };
const loopback = (addr?: string) => !!addr && (addr.startsWith("127.") || addr === "::1" || addr.startsWith("[::1]"));

export type PortAccess = "private" | "users" | "public";
const ACCESS: [PortAccess, string][] = [
  ["private", "仅本人和协作者"],
  ["users", "登录用户"],
  ["public", "公开（任何拿到链接的人）"],
];

/** accessOf: a port's level; public wins if it is listed twice. */
export function accessOf(v: { publicPorts: number[]; userPorts: number[] }, port: number): PortAccess {
  return v.publicPorts.includes(port) ? "public" : v.userPorts.includes(port) ? "users" : "private";
}

/** withAccess: the two port lists after setting one port's level. */
export function withAccess(v: { publicPorts: number[]; userPorts: number[] }, port: number, access: PortAccess): { publicPorts: number[]; userPorts: number[] } {
  const without = (l: number[]) => l.filter((p) => p !== port);
  return {
    publicPorts: access === "public" ? [...without(v.publicPorts), port] : without(v.publicPorts),
    userPorts: access === "users" ? [...without(v.userPorts), port] : without(v.userPorts),
  };
}

/** portRows: the TCP listeners (one row per port; the container's own
 *  sshd port is never proxied and is left out) plus opened ports nothing
 *  listens on right now. */
export function portRows(listen: Listen[], openedPorts: number[], sshPort = 22): { port: number; local: boolean; listening: boolean }[] {
  const rows = new Map<number, { port: number; local: boolean; listening: boolean }>();
  for (const l of listen) {
    if (!l.proto.startsWith("tcp") || l.port === sshPort) continue;
    const cur = rows.get(l.port);
    // reachable if any listener on the port is not loopback-only
    rows.set(l.port, { port: l.port, listening: true, local: (cur?.local ?? true) && loopback(l.addr) });
  }
  for (const p of openedPorts) if (p !== sshPort && !rows.has(p)) rows.set(p, { port: p, local: false, listening: false });
  return [...rows.values()].sort((a, b) => a.port - b.port);
}

function CopyLink({ url }: { url: string }) {
  const [done, copy] = useCopied();
  return (
    <button type="button" onClick={() => copy(url)} className="text-muted hover:text-accent">
      {done ? "已复制" : "复制链接"}
    </button>
  );
}

export function PortsPanel({ r, sshPort }: { r: Ref; /** the container's sshd port (from the DevPod's endpoint) */ sshPort?: number }) {
  const qc = useQueryClient();
  const key = ["http", r.cluster ?? "", r.name];
  const http = useQuery({ queryKey: key, queryFn: () => getHttp(r), retry: false });
  const info = useInfo(r);
  const [other, setOther] = useState("");
  const [reset, setReset] = useState(false);
  const save = useMutation({
    mutationFn: (body: { publicPorts?: number[]; userPorts?: number[]; reset?: boolean }) => putHttp(r, body),
    onSuccess: (v: HttpView) => qc.setQueryData(key, v),
  });
  if (!http.data) return null;
  const v = http.data;
  const url = (port: number) => v.urlTemplate.replace("{port}", String(port));
  const lists = { publicPorts: v.publicPorts, userPorts: v.userPorts ?? [] };
  const rows = portRows(info.data?.snapshot?.listen ?? [], [...lists.publicPorts, ...lists.userPorts], sshPort);
  const setAccess = (port: number, access: PortAccess) => save.mutate(withAccess(lists, port, access));
  const otherPort = Number(other);
  const otherOK = Number.isInteger(otherPort) && otherPort >= 1 && otherPort <= 65535;

  return (
    <Panel
      title="端口"
      actions={
        <button type="button" onClick={() => setReset(true)} className="text-[11px] text-muted hover:text-fail">
          重置链接…
        </button>
      }
    >
      <div className="space-y-2 p-3 text-xs">
        <p className="text-muted">
          容器里的 HTTP 服务可以通过 <code className="mono text-ink">{url(0).replace("-0", "-{端口}")}</code> 访问。每个端口有三种访问方式：默认只有你、协作者和管理员能打开；「登录用户」是任何登录了本控制台的人；「公开」是任何拿到链接的人，不需要登录。
        </p>
        {rows.length > 0 && (
          <table className="w-full border-collapse">
            <thead>
              <tr className="h-6 border-b border-line text-left text-[11px] text-muted">
                <th className="font-normal">端口</th>
                <th className="font-normal">访问</th>
                <th className="font-normal">链接</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((r) => {
                const access = accessOf(lists, r.port);
                return (
                  <tr key={r.port} className="h-8 border-b border-line last:border-0">
                    <td className="mono w-24">
                      {r.port}
                      {!r.listening && <span className="ml-1 text-faint">未监听</span>}
                    </td>
                    <td className="w-72">
                      <span className="inline-flex items-center gap-2">
                        <Select aria-label={`端口 ${r.port} 的访问方式`} value={access} disabled={save.isPending} onChange={(e) => setAccess(r.port, e.target.value as PortAccess)} className="!h-6 !w-52 text-xs">
                          {ACCESS.map(([id, label]) => (
                            <option key={id} value={id}>
                              {label}
                            </option>
                          ))}
                        </Select>
                        {access === "public" && <Badge tone="warm">公开</Badge>}
                      </span>
                    </td>
                    <td>
                      {r.local ? (
                        <span className="text-faint" title="程序只监听了 127.0.0.1，代理连不上；让它监听 0.0.0.0">
                          只监听本机地址，无法代理
                        </span>
                      ) : (
                        <span className="inline-flex items-center gap-3">
                          <a href={url(r.port)} target="_blank" rel="noreferrer" className="text-accent hover:underline">
                            打开 ↗
                          </a>
                          <CopyLink url={url(r.port)} />
                        </span>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
        {rows.length === 0 && <p className="text-faint">{info.data ? "容器里目前没有程序在监听 TCP 端口。" : "还没有监听端口的数据；可以直接输入端口号打开。"}</p>}
        <form
          className="flex items-center gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            if (otherOK) window.open(url(otherPort), "_blank", "noreferrer");
          }}
        >
          <Input aria-label="其他端口" value={other} onChange={(e) => setOther(e.target.value.replace(/\D/g, ""))} placeholder="其他端口，如 8888" className="mono !w-40" inputMode="numeric" />
          <Button type="submit" disabled={!otherOK}>
            打开
          </Button>
        </form>
        {save.isError && <p className="text-fail">保存失败：{String(save.error instanceof Error ? save.error.message : save.error)}</p>}
      </div>
      <ConfirmDialog
        open={reset}
        onOpenChange={setReset}
        title="重置链接？"
        confirmLabel="重置"
        danger
        onConfirm={() => {
          setReset(false);
          save.mutate({ reset: true });
        }}
      >
        会生成一个新的随机地址。之前发出去的所有链接（包括公开端口的）立即失效。
      </ConfirmDialog>
    </Panel>
  );
}
