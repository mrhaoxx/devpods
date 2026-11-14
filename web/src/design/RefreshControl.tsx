// 自动刷新: how often a list refreshes by itself, and a refresh now.
import { REFRESH_CHOICES } from "../lib/autoRefresh";
import { Button } from "./Button";
import { Select } from "./Form";

export function RefreshControl({ every, onEvery, onRefresh, busy }: { every: number; onEvery(ms: number): void; onRefresh(): void; busy?: boolean }) {
  return (
    <span className="inline-flex items-center gap-1 text-xs">
      <Button variant="ghost" onClick={onRefresh} disabled={busy} title="立即刷新">
        <span aria-hidden className={busy ? "inline-block animate-spin" : undefined}>
          ↻
        </span>
        <span className="ml-1">刷新</span>
      </Button>
      <Select aria-label="自动刷新" title="自动刷新" value={every} onChange={(e) => onEvery(Number(e.target.value))} className="!h-7 !w-auto text-xs">
        {REFRESH_CHOICES.map(([ms, label]) => (
          <option key={ms} value={ms}>
            {ms ? `每 ${label}` : "不自动刷新"}
          </option>
        ))}
      </Select>
    </span>
  );
}
