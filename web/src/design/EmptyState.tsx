import type { ReactNode } from "react";

export function EmptyState({ title, hint, tone = "neutral" }: { title: ReactNode; hint?: ReactNode; tone?: "neutral" | "error" }) {
  return (
    <div role={tone === "error" ? "alert" : undefined} className="px-4 py-10 text-center">
      <div className={`text-sm ${tone === "error" ? "text-fail" : "text-muted"}`}>{title}</div>
      {hint && <div className="mt-1 text-xs text-faint">{hint}</div>}
    </div>
  );
}
