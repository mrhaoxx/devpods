import { ReactNode, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { cx } from "../../ui";

export const RANGES: Record<string, number> = { "24h": 1, "7d": 7, "30d": 30, "90d": 90 };

// useRange returns RFC 3339 from/to for the chosen window.
export function useRange(initial = "7d") {
  const [range, setRange] = useState(initial);
  const { from, to } = useMemo(() => {
    const now = new Date();
    return { to: now.toISOString(), from: new Date(now.getTime() - RANGES[range] * 86400_000).toISOString() };
  }, [range]);
  return { range, setRange, from, to };
}

export function RangeChips({ range, setRange }: { range: string; setRange: (r: string) => void }) {
  return (
    <span className="inline-flex gap-1">
      {Object.keys(RANGES).map((r) => (
        <button
          key={r}
          onClick={() => setRange(r)}
          className={cx(
            "rounded-full border px-2 py-0.5",
            range === r ? "border-accent/40 bg-accent-soft text-accent" : "border-line bg-surface text-muted hover:text-ink",
          )}
        >
          {r}
        </button>
      ))}
    </span>
  );
}

export function FilterInput({ value, onChange, placeholder }: { value: string; onChange: (v: string) => void; placeholder: string }) {
  return (
    <input
      value={value}
      onChange={(e) => onChange(e.target.value)}
      placeholder={placeholder}
      className="w-44 rounded-md border border-line-strong bg-surface px-2 py-1 text-ink placeholder:text-faint"
    />
  );
}

export function AuditHeader({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <header className="mb-4">
      <p className="eyebrow mb-1">Audit</p>
      <h1 className="mono text-xl font-semibold tracking-tight">{title}</h1>
      <nav className="mono mt-2 flex flex-wrap gap-3 text-xs text-muted">
        <Link to="/audit/sessions" className="hover:text-accent">sessions</Link>
        <Link to="/audit/commands" className="hover:text-accent">commands</Link>
        <Link to="/audit/files" className="hover:text-accent">files</Link>
        {children}
      </nav>
    </header>
  );
}

export const th = "whitespace-nowrap px-2 py-1.5 text-left font-medium";
export const td = "whitespace-nowrap px-2 py-1";

export function Table({ head, children, empty }: { head: string[]; children: ReactNode; empty?: boolean }) {
  return (
    <div className="mono overflow-x-auto rounded-lg border border-line bg-surface text-xs">
      <table className="w-full border-collapse">
        <thead>
          <tr className="border-b border-line bg-sunk text-[10.5px] uppercase tracking-wider text-faint">
            {head.map((h) => (
              <th key={h} className={th}>
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {children}
          {empty && (
            <tr>
              <td colSpan={head.length} className="px-4 py-8 text-center text-faint">
                Nothing in this period.
              </td>
            </tr>
          )}
        </tbody>
      </table>
    </div>
  );
}

export const fmtTS = (iso: string) => new Date(iso).toLocaleString();

export function LoadMore({ hasMore, onClick, busy }: { hasMore?: boolean; onClick: () => void; busy?: boolean }) {
  if (!hasMore) return null;
  return (
    <button onClick={onClick} disabled={busy} className="mono mt-3 text-xs text-accent hover:underline">
      {busy ? "loading…" : "load more"}
    </button>
  );
}
