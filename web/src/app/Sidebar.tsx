// Permanent sidebar (UI spec §4.1). ≤ 1100 px it collapses to an icon
// rail that expands on hover; < 700 px the shell shows it in a drawer.
import { NavLink } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { listAlerts, listDevPods, listNodes, listAdminUsers, meIn, type Me } from "../data/api";
import { useClusterFilter } from "../data/clusterFilter";
import { qk } from "../data/queries";

type Item = { to: string; label: string; icon: string; count?: string; countTone?: "fail"; end?: boolean; hint?: string };

function NavItem({ it, onNavigate }: { it: Item; onNavigate?: () => void }) {
  return (
    <NavLink
      to={it.to}
      end={it.end}
      onClick={onNavigate}
      title={it.hint ?? it.label}
      className={({ isActive }) =>
        `flex h-7 items-center gap-2 rounded-md px-2 text-[13px] whitespace-nowrap ${
          isActive ? "bg-accent-soft text-accent" : "text-muted hover:bg-sunk hover:text-ink"
        }`
      }
    >
      <span aria-hidden="true" className="w-4 shrink-0 text-center">
        {it.icon}
      </span>
      <span className="side-label min-w-0 flex-1 truncate">{it.label}</span>
      {it.count !== undefined && (
        <span className={`side-label mono text-[11px] ${it.countTone === "fail" ? "text-fail" : "text-faint"}`}>{it.count}</span>
      )}
    </NavLink>
  );
}

function Group({ title, items, onNavigate }: { title?: string; items: Item[]; onNavigate?: () => void }) {
  return (
    <div className="space-y-0.5">
      {title && <div className="side-label px-2 pb-0.5 pt-3 text-[11px] text-faint">── {title}</div>}
      {items.map((it) => (
        <NavItem key={it.to} it={it} onNavigate={onNavigate} />
      ))}
    </div>
  );
}

const dayAgo = () => new Date(Date.now() - 24 * 3600 * 1000).toISOString();

export function Sidebar({ me, onNavigate }: { me: Me; onNavigate?: () => void }) {
  const admin = me.admin;
  const audit = !!me.features.audit;
  const { filter } = useClusterFilter();
  // counts share the pages' query keys; the pages' own watches keep them fresh
  const mine = useQuery({ queryKey: qk.devpods("mine"), queryFn: () => listDevPods("mine"), staleTime: 30_000 });
  const all = useQuery({ queryKey: qk.devpods("all"), queryFn: () => listDevPods("all"), staleTime: 30_000, enabled: admin });
  const nodes = useQuery({ queryKey: qk.nodes, queryFn: listNodes, staleTime: 60_000, enabled: admin });
  const users = useQuery({ queryKey: qk.users, queryFn: listAdminUsers, staleTime: 60_000, enabled: admin });
  const alerts = useQuery({
    queryKey: ["audit", "alerts24h"],
    queryFn: () => listAlerts({ from: dayAgo(), limit: "100" }),
    enabled: admin && audit,
    refetchInterval: 60_000,
    retry: false,
  });
  const allItems = all.data?.items ?? [];
  const running = allItems.filter((d) => d.status?.phase === "Running").length;
  const alertCount = alerts.data ? `${alerts.data.items.length}${alerts.data.next ? "+" : ""}` : undefined;

  const userItems: Item[] = [
    { to: "/", label: "我的 DevPod", icon: "▦", count: mine.data ? String(mine.data.items.length) : undefined, end: true },
    ...(me.features.snapshots ? [{ to: "/images", label: "我的镜像", icon: "◫" }] : []),
    { to: "/configs", label: "配置", icon: "▧" },
    ...(me.features.reservations ? [{ to: "/reservations", label: "节点预约", icon: "▥" }] : []),
    ...(audit ? [{ to: "/sessions", label: "我的会话", icon: "◷" }] : []),
  ];
  const settings: Item[] = [{ to: "/settings", label: "密钥与凭证", icon: "⚙", hint: "SSH 公钥、访问令牌、镜像仓库凭证" }];
  const cluster: Item[] = [
    { to: "/admin", label: "总览", icon: "◎", end: true },
    { to: "/admin/devpods", label: "全部 DevPod", icon: "▤", count: all.data ? `${running}/${allItems.length}` : undefined },
    { to: "/admin/nodes", label: "节点", icon: "⬡", count: nodes.data ? String(nodes.data.items.length) : undefined },
    { to: "/admin/users", label: "用户与配额", icon: "☺", count: users.data ? String(users.data.items.length) : undefined },
    ...(me.features.clusters ? [{ to: "/admin/clusters", label: "集群", icon: "◈" }] : []),
  ];
  const auditItems: Item[] = [
    { to: "/audit/sessions", label: "会话 / 回放", icon: "▶" },
    { to: "/audit/commands", label: "命令 · 文件", icon: "⌘" },
    { to: "/audit/alerts", label: "告警", icon: "⚠", count: alertCount && alertCount !== "0" ? alertCount : undefined, countTone: "fail" },
    { to: "/audit/access-log", label: "访问记录 · 策略", icon: "☰" },
  ];
  // quotas are counted per cluster: the filtered one's, or the console's
  const quota = meIn(me, filter)!;
  const quotaMax = quota.quota.maxDevPods;
  return (
    <nav aria-label="主导航" className="side-inner flex h-full flex-col gap-1 bg-surface px-2 py-3">
      <div className="flex items-center gap-2 px-2 pb-2">
        <span className="text-accent" aria-hidden="true">
          ◈
        </span>
        <span className="side-label mono text-sm font-semibold">devpod</span>
      </div>
      <Group items={userItems} onNavigate={onNavigate} />
      <Group title="设置" items={settings} onNavigate={onNavigate} />
      {admin && <Group title="集群" items={cluster} onNavigate={onNavigate} />}
      {admin && audit && <Group title="审计" items={auditItems} onNavigate={onNavigate} />}
      <div className="pt-3">
        <NavLink
          to="/?new=1"
          onClick={onNavigate}
          className="flex h-7 items-center justify-center gap-1 rounded-md bg-accent px-2 text-[13px] font-medium text-white hover:bg-accent-hi"
        >
          <span aria-hidden="true">+</span>
          <span className="side-label">新建 DevPod</span>
        </NavLink>
      </div>
      <div className="side-label mt-auto space-y-0.5 px-2 pt-3 text-[11px] text-muted">
        <div className="mono truncate text-ink">{me.user}</div>
        <div className="mono">
          配额 {quota.usage.devpods}/{quotaMax ?? "∞"}
        </div>
      </div>
    </nav>
  );
}
