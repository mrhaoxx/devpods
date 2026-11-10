// Usage against a limit: a bar plus "used/limit" text. ≥ 80 % turns
// yellow, over the limit red (UI spec §5.5); the level is also spelled
// out for screen readers, so color never carries it alone.
export type MeterLevel = "ok" | "warn" | "over";

export function meterLevel(used: number, limit: number): MeterLevel {
  if (!(limit > 0)) return "ok";
  const r = used / limit;
  if (r > 1) return "over";
  if (r >= 0.8) return "warn";
  return "ok";
}

const fill: Record<MeterLevel, string> = { ok: "bg-run", warn: "bg-warm", over: "bg-fail" };
const word: Record<MeterLevel, string> = { ok: "", warn: "接近上限", over: "超出上限" };

export function Meter({ used, limit, label, width = 64 }: { used: number; limit: number; label?: string; width?: number }) {
  const level = meterLevel(used, limit);
  const pct = limit > 0 ? Math.min(100, (used / limit) * 100) : 0;
  return (
    <span className="inline-flex items-center gap-1.5" title={word[level] || undefined}>
      <span
        className="relative inline-block h-1.5 overflow-hidden rounded-sm bg-sunk"
        style={{ width }}
        role="meter"
        aria-valuenow={used}
        aria-valuemin={0}
        aria-valuemax={limit || undefined}
        aria-label={word[level] ? `${label ?? ""} ${word[level]}` : label}
      >
        <span className={`absolute inset-y-0 left-0 ${fill[level]}`} style={{ width: `${pct}%` }} />
      </span>
      {label !== undefined && <span className={`mono text-xs ${level === "over" ? "text-fail" : ""}`}>{label}</span>}
    </span>
  );
}
