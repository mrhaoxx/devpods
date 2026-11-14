// How often a list refreshes by itself, chosen on the page (自动刷新) and
// kept in this browser per page; 0 is off.
import { useCallback, useState } from "react";

export const REFRESH_CHOICES: [number, string][] = [
  [0, "关闭"],
  [5_000, "5 秒"],
  [10_000, "10 秒"],
  [30_000, "30 秒"],
  [60_000, "1 分钟"],
];

const key = (page: string) => `devpod.refresh.${page}`;

/** useAutoRefresh: the page's refresh interval in ms (0: off), dflt until one is chosen. */
export function useAutoRefresh(page: string, dflt: number): [number, (ms: number) => void] {
  const [every, set] = useState(() => {
    try {
      const v = localStorage.getItem(key(page));
      const n = v === null ? NaN : Number(v);
      return REFRESH_CHOICES.some(([ms]) => ms === n) ? n : dflt;
    } catch {
      return dflt;
    }
  });
  const choose = useCallback(
    (ms: number) => {
      set(ms);
      try {
        localStorage.setItem(key(page), String(ms));
      } catch {
        // not kept: no matter
      }
    },
    [page],
  );
  return [every, choose];
}
