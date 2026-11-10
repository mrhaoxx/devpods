import type { ReactNode } from "react";

export function Panel({ title, actions, children, className = "" }: { title?: ReactNode; actions?: ReactNode; children: ReactNode; className?: string }) {
  return (
    <section className={`rounded-lg border border-line bg-surface ${className}`}>
      {(title || actions) && (
        <header className="flex items-center gap-2 border-b border-line px-3 py-2">
          <h2 className="min-w-0 flex-1 truncate text-xs font-semibold text-muted">{title}</h2>
          {actions}
        </header>
      )}
      <div>{children}</div>
    </section>
  );
}
