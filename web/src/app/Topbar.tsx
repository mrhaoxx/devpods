import { useLocation } from "react-router-dom";
import { logout, type Me } from "../data/api";
import { useTheme, type ThemePref } from "./theme";

const titles: [RegExp, string][] = [
  [/^\/$/, "我的 DevPod"],
  [/^\/devpods\/new/, "新建 DevPod"],
  [/^\/devpods\//, "DevPod"],
  [/^\/sessions\/./, "会话详情"],
  [/^\/sessions/, "我的会话"],
  [/^\/settings/, "设置"],
  [/^\/admin\/devpods/, "全部 DevPod"],
  [/^\/admin\/nodes/, "节点"],
  [/^\/admin\/users/, "用户与配额"],
  [/^\/admin/, "集群总览"],
  [/^\/audit\/sessions/, "会话 / 回放"],
  [/^\/audit\/commands/, "命令"],
  [/^\/audit\/files/, "文件"],
  [/^\/audit\/alerts/, "告警"],
  [/^\/audit\/access-log/, "访问记录"],
  [/^\/audit\/policy/, "审计策略"],
];

export function titleFor(pathname: string): string {
  return titles.find(([re]) => re.test(pathname))?.[1] ?? "devpod";
}

/** openPalette asks the shell to open the ⌘K palette. */
export const openPalette = () => window.dispatchEvent(new Event("devpod:palette"));

export function Topbar({ me, onMenu }: { me: Me; onMenu: () => void }) {
  const { pathname } = useLocation();
  const [pref, setPref] = useTheme();
  return (
    <header className="flex h-11 shrink-0 items-center gap-2 border-b border-line bg-surface px-3">
      <button type="button" onClick={onMenu} className="hidden rounded px-1.5 text-muted hover:bg-sunk max-[700px]:block" aria-label="打开导航">
        ☰
      </button>
      <h1 className="min-w-0 flex-1 truncate text-sm font-semibold">{titleFor(pathname)}</h1>
      <button
        type="button"
        onClick={openPalette}
        className="flex h-7 w-56 items-center gap-2 rounded-md border border-line bg-sunk px-2 text-xs text-faint hover:text-muted max-[700px]:w-8 max-[700px]:justify-center"
        aria-label="搜索"
      >
        <span aria-hidden="true">⌕</span>
        <span className="flex-1 text-left max-[700px]:hidden">搜索…</span>
        <kbd className="mono rounded border border-line px-1 text-[10px] max-[700px]:hidden">⌘K</kbd>
      </button>
      <select
        aria-label="配色"
        value={pref}
        onChange={(e) => setPref(e.target.value as ThemePref)}
        className="h-7 rounded-md border border-line bg-surface px-1 text-xs text-muted"
      >
        <option value="system">跟随系统</option>
        <option value="light">浅色</option>
        <option value="dark">深色</option>
      </select>
      <button
        type="button"
        onClick={async () => {
          await logout().catch(() => {});
          window.location.href = "/login";
        }}
        className="rounded px-1.5 text-xs text-muted hover:text-ink"
        title={`以 ${me.user} 登录`}
      >
        退出
      </button>
    </header>
  );
}
