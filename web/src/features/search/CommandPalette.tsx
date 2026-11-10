// ⌘K global search (UI spec §5.9): users, DevPods, sessions, commands
// (≤ 5 each, already filtered by the server for non-admins); Enter goes
// to the result; DevPod rows offer 复制 SSH / 休眠·启动 / 打开监控.
import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Command } from "cmdk";
import { patchDevPod, search, sshCommand } from "../../data/api";
import { useMe } from "../../data/queries";
import { copyText } from "../../design/Copy";
import { StatusDot } from "../../design/StatusDot";

function useDebounced<T>(v: T, ms: number): T {
  const [d, setD] = useState(v);
  useEffect(() => {
    const t = setTimeout(() => setD(v), ms);
    return () => clearTimeout(t);
  }, [v, ms]);
  return d;
}

const item = "flex cursor-pointer items-center gap-2 rounded px-2 py-1.5 text-xs aria-selected:bg-accent-soft aria-selected:text-accent";
const group = "px-1 py-1 [&_[cmdk-group-heading]]:px-2 [&_[cmdk-group-heading]]:py-1 [&_[cmdk-group-heading]]:text-[11px] [&_[cmdk-group-heading]]:text-faint";

export function CommandPalette() {
  const [open, setOpen] = useState(false);
  const [q, setQ] = useState("");
  const query = useDebounced(q.trim(), 150);
  const nav = useNavigate();
  const qc = useQueryClient();
  const me = useMe().data;
  const res = useQuery({ queryKey: ["search", query], queryFn: () => search(query), enabled: open && query.length > 0, retry: false, staleTime: 5000 });

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setOpen((o) => !o);
      }
    };
    const onOpen = () => setOpen(true);
    window.addEventListener("keydown", onKey);
    window.addEventListener("devpod:palette", onOpen);
    return () => {
      window.removeEventListener("keydown", onKey);
      window.removeEventListener("devpod:palette", onOpen);
    };
  }, []);

  const go = (to: string) => {
    setOpen(false);
    setQ("");
    nav(to);
  };
  const r = query && res.data?.users ? res.data : undefined;
  const none = r && !r.users.length && !r.devpods.length && !r.sessions.length && !r.commands.length;

  return (
    <Command.Dialog
      open={open}
      onOpenChange={setOpen}
      label="全局搜索"
      shouldFilter={false}
      overlayClassName="fixed inset-0 z-40 bg-black/30"
      contentClassName="fixed left-1/2 top-[12%] z-50 w-[min(94vw,560px)] -translate-x-1/2 overflow-hidden rounded-lg border border-line bg-surface shadow-2xl"
    >
      <Command.Input
        value={q}
        onValueChange={setQ}
        placeholder="搜索用户、DevPod、会话、命令…"
        className="h-10 w-full border-b border-line bg-transparent px-3 text-sm outline-none"
      />
      <Command.List className="max-h-[60vh] overflow-y-auto p-1">
        {!query && <div className="px-3 py-6 text-center text-xs text-faint">输入关键字开始搜索</div>}
        {query && res.isFetching && !r && <Command.Loading>
          <div className="px-3 py-4 text-xs text-faint">搜索中…</div>
        </Command.Loading>}
        {res.isError && <div className="px-3 py-4 text-xs text-fail">搜索失败：{String(res.error)}</div>}
        {none && !r?.auditError && <div className="px-3 py-6 text-center text-xs text-faint">没有结果</div>}
        {r && r.users.length > 0 && (
          <Command.Group heading="用户" className={group}>
            {r.users.map((u) => (
              <Command.Item key={u.name} value={`user:${u.name}`} onSelect={() => go(`/admin/users?user=${encodeURIComponent(u.name)}`)} className={item}>
                <span className="mono">{u.name}</span>
                {u.displayName && <span className="text-muted">{u.displayName}</span>}
              </Command.Item>
            ))}
          </Command.Group>
        )}
        {r && r.devpods.length > 0 && (
          <Command.Group heading="DevPod" className={group}>
            {r.devpods.map((d) => {
              const running = d.phase === "Running";
              const act = (e: React.MouseEvent, f: () => void) => {
                e.stopPropagation();
                f();
              };
              return (
                <Command.Item key={d.name} value={`devpod:${d.name}`} onSelect={() => go(`/devpods/${d.name}`)} className={item}>
                  <StatusDot phase={d.phase} showLabel={false} />
                  <span className="mono min-w-0 flex-1 truncate">{d.name}</span>
                  {me?.admin && <span className="mono text-muted">{d.owner}</span>}
                  <button type="button" aria-label={`${d.name} 复制 SSH`} className="rounded px-1 text-[11px] text-muted hover:bg-surface hover:text-ink" onClick={(e) => act(e, () => void copyText(sshCommand(me, d.owner, d.name)))}>
                    复制 SSH
                  </button>
                  <button
                    type="button"
                    aria-label={`${d.name} ${running ? "休眠" : "启动"}`}
                    className="rounded px-1 text-[11px] text-muted hover:bg-surface hover:text-ink"
                    onClick={(e) => act(e, () => void patchDevPod(d.name, !running).finally(() => void qc.invalidateQueries({ queryKey: ["search"] })))}
                  >
                    {running ? "休眠" : "启动"}
                  </button>
                  <button type="button" aria-label={`${d.name} 打开监控`} className="rounded px-1 text-[11px] text-muted hover:bg-surface hover:text-ink" onClick={(e) => act(e, () => go(`/devpods/${d.name}?tab=monitor`))}>
                    打开监控
                  </button>
                </Command.Item>
              );
            })}
          </Command.Group>
        )}
        {r?.auditError && <div className="px-3 py-2 text-[11px] text-warm">审计搜索暂不可用，会话和命令结果暂时缺失。</div>}
        {r && r.sessions.length > 0 && (
          <Command.Group heading="会话" className={group}>
            {r.sessions.map((s) => (
              <Command.Item key={s.id} value={`session:${s.id}`} onSelect={() => go(`/sessions/${s.id}`)} className={item}>
                <span className="mono">{s.user}</span>
                <span className="text-muted">→</span>
                <span className="mono flex-1 truncate">{s.devpod}</span>
                <span className="text-faint">{new Date(s.openedAt).toLocaleString()}</span>
              </Command.Item>
            ))}
          </Command.Group>
        )}
        {r && r.commands.length > 0 && (
          <Command.Group heading="命令" className={group}>
            {r.commands.map((c, i) => (
              <Command.Item key={`${c.sessionId}:${i}`} value={`cmd:${c.sessionId}:${i}`} onSelect={() => go(`/sessions/${c.sessionId}`)} className={item}>
                <span className="mono min-w-0 flex-1 truncate">{c.cmd}</span>
                <span className="mono text-muted">{c.devpod}</span>
              </Command.Item>
            ))}
          </Command.Group>
        )}
      </Command.List>
    </Command.Dialog>
  );
}
