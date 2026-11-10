// Light / dark theme (UI spec §3): follows prefers-color-scheme unless the
// user picked one; the choice lives in localStorage, and every storage
// access is guarded (private windows and blocked site data throw).
import { useCallback, useEffect, useState } from "react";

export type ThemePref = "system" | "light" | "dark";
export const THEME_KEY = "devpod.theme";

export function readPref(storage?: Pick<Storage, "getItem">): ThemePref {
  try {
    const v = storage?.getItem(THEME_KEY);
    return v === "light" || v === "dark" || v === "system" ? v : "system";
  } catch {
    return "system";
  }
}

export function writePref(storage: Pick<Storage, "setItem"> | undefined, p: ThemePref): void {
  try {
    storage?.setItem(THEME_KEY, p);
  } catch {
    /* not persisted: the choice lasts for this page */
  }
}

export function resolveTheme(p: ThemePref, prefersDark: boolean): "light" | "dark" {
  return p === "system" ? (prefersDark ? "dark" : "light") : p;
}

function storage(): Storage | undefined {
  try {
    return window.localStorage;
  } catch {
    return undefined;
  }
}

const darkQuery = () => window.matchMedia?.("(prefers-color-scheme: dark)");

function apply(t: "light" | "dark") {
  document.documentElement.dataset.theme = t;
  document.documentElement.style.colorScheme = t;
}

/** initTheme sets the theme before the first render (no flash). */
export function initTheme(): void {
  apply(resolveTheme(readPref(storage()), !!darkQuery()?.matches));
}

export function useTheme(): [ThemePref, (p: ThemePref) => void] {
  const [pref, setPref] = useState<ThemePref>(() => readPref(storage()));
  const [prefersDark, setPrefersDark] = useState(() => !!darkQuery()?.matches);
  useEffect(() => {
    const q = darkQuery();
    if (!q) return;
    const on = (e: MediaQueryListEvent) => setPrefersDark(e.matches);
    q.addEventListener("change", on);
    return () => q.removeEventListener("change", on);
  }, []);
  useEffect(() => apply(resolveTheme(pref, prefersDark)), [pref, prefersDark]);
  const set = useCallback((p: ThemePref) => {
    writePref(storage(), p);
    setPref(p);
  }, []);
  return [pref, set];
}
