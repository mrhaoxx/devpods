import type { ReactNode } from "react";

/** One cell of a KPI strip. */
export function Kpi({ label, value, sub }: { label: string; value: ReactNode; sub?: ReactNode }) {
  return (
    <div className="min-w-0 px-3 py-2">
      <div className="truncate text-[11px] text-muted">{label}</div>
      <div className="mono truncate text-lg leading-6">{value}</div>
      {sub !== undefined && <div className="mono truncate text-[11px] text-faint">{sub}</div>}
    </div>
  );
}
