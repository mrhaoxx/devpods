import type { ReactNode } from "react";

/** A toggleable filter chip with an optional count. */
export function Chip({
  active,
  count,
  onClick,
  children,
}: {
  active?: boolean;
  count?: number;
  onClick?: () => void;
  children: ReactNode;
}) {
  return (
    <button
      type="button"
      aria-pressed={!!active}
      onClick={onClick}
      className={`inline-flex h-6 items-center gap-1 rounded-full border px-2.5 text-xs whitespace-nowrap ${
        active ? "border-accent bg-accent-soft text-accent" : "border-line text-muted hover:text-ink"
      }`}
    >
      {children}
      {count !== undefined && <span className="mono opacity-80">{count}</span>}
    </button>
  );
}
