// Up a level (Cmd+↑ on a Mac, Alt+↑ elsewhere — Finder's and Explorer's
// keys): from a page to the one it belongs under — a DevPod's page to the
// list, a config to the configs, an admin page to the overview. Not while
// typing (the key moves the caret there), nor under a dialog or a menu.
import { useEffect } from "react";
import { useLocation, useNavigate } from "react-router-dom";

/** parentOf: the page a path belongs under (null: a top page). */
export function parentOf(path: string): string | null {
  const p = path.replace(/\/+$/, "") || "/";
  if (/^\/devpods\/[^/]+\/[^/]+$/.test(p)) return "/";
  if (/^\/configs\/[^/]+$/.test(p)) return "/configs";
  if (/^\/sessions\/[^/]+$/.test(p)) return "/sessions";
  if (/^\/admin\/[^/]+$/.test(p)) return "/admin";
  return null;
}

// a page's own say (its parent depends on what it shows), while it is up
let pageTarget: string | null = null;

/** useUpTarget: this page's parent, when it is not the path's (null: the path's). */
export function useUpTarget(target: string | null) {
  useEffect(() => {
    pageTarget = target;
    return () => {
      if (pageTarget === target) pageTarget = null;
    };
  }, [target]);
}

const isMac = () => typeof navigator !== "undefined" && /Mac|iPhone|iPad/.test(navigator.platform);

function typing(t: EventTarget | null): boolean {
  if (!(t instanceof HTMLElement)) return false;
  if (t.closest("textarea,select,[contenteditable='true'],[contenteditable=''],.xterm")) return true;
  return t instanceof HTMLInputElement && !["checkbox", "radio", "button", "submit", "reset"].includes(t.type);
}

/** useUpLevel: listens for Cmd+↑ (Alt+↑) and goes to the page's parent. */
export function useUpLevel() {
  const nav = useNavigate();
  const { pathname } = useLocation();
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "ArrowUp" || e.shiftKey || e.defaultPrevented) return;
      const mod = isMac() ? e.metaKey && !e.altKey && !e.ctrlKey : e.altKey && !e.metaKey && !e.ctrlKey;
      if (!mod || typing(e.target) || document.querySelector('[role="dialog"],[role="menu"],[role="alertdialog"]')) return;
      const to = pageTarget ?? parentOf(pathname);
      if (!to) return;
      e.preventDefault();
      nav(to);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [nav, pathname]);
}
