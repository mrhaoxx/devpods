import { useEffect, useRef, useState, ReactNode, ButtonHTMLAttributes, InputHTMLAttributes } from "react";
import { Link, useLocation } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { me as fetchMe, logout, Me } from "./api";

export function cx(...parts: (string | false | null | undefined)[]) {
  return parts.filter(Boolean).join(" ");
}

/* ─── Phase → tone ─────────────────────────────────────────────────── */
export type Tone = "run" | "warm" | "idle" | "fail";
export function phaseTone(phase?: string): Tone {
  switch (phase) {
    case "Running":
      return "run";
    case "Pending":
      return "warm";
    case "Failed":
      return "fail";
    default:
      return "idle";
  }
}
const toneText: Record<Tone, string> = { run: "text-run", warm: "text-warm", idle: "text-idle", fail: "text-fail" };
const toneBg: Record<Tone, string> = { run: "bg-run", warm: "bg-warm", idle: "bg-idle", fail: "bg-fail" };

/* Status cell — a single core-square. Running breathes. */
export function StatusCell({ phase, className }: { phase?: string; className?: string }) {
  const tone = phaseTone(phase);
  return (
    <span
      className={cx("inline-block size-2.5 rounded-[3px]", toneBg[tone], phase === "Running" && "pulse", className)}
      aria-hidden
    />
  );
}

export function PhaseLabel({ phase }: { phase?: string }) {
  const tone = phaseTone(phase);
  return (
    <span className={cx("inline-flex items-center gap-1.5", toneText[tone])}>
      <StatusCell phase={phase} />
      <span className="text-xs font-medium">{phase ?? "Pending"}</span>
    </span>
  );
}

/* ─── Signature: discrete core cells ───────────────────────────────── */
export function CoreMeter({ used, limit }: { used: number; limit?: number }) {
  const cap = 32;
  const total = limit && limit > 0 ? limit : Math.max(used, 1);
  const cells = Math.min(total, cap);
  const scale = total > cap ? total / cap : 1;
  return (
    <span className="inline-flex flex-wrap items-center gap-[3px] align-middle">
      {Array.from({ length: cells }).map((_, i) => {
        const filled = (i + 1) * scale <= used;
        const over = filled && limit != null && (i + 1) * scale > limit;
        return (
          <span
            key={i}
            className={cx("size-[7px] rounded-[2px]", over ? "bg-fail" : filled ? "bg-accent" : "bg-line-strong")}
          />
        );
      })}
    </span>
  );
}

/* Thin proportional bar for continuous resources (memory, storage). */
export function Bar({ used, limit }: { used: number; limit?: number }) {
  const pct = limit && limit > 0 ? Math.min(100, (used / limit) * 100) : 0;
  const over = limit != null && used > limit;
  return (
    <span className="inline-block h-1 w-16 overflow-hidden rounded-full bg-line align-middle">
      <span className={cx("block h-full rounded-full", over ? "bg-fail" : "bg-accent")} style={{ width: `${pct}%` }} />
    </span>
  );
}

/* ─── Buttons ──────────────────────────────────────────────────────── */
type BtnProps = ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: "accent" | "ghost" | "danger";
  size?: "sm" | "md";
};
export function Button({ variant = "ghost", size = "md", className, ...rest }: BtnProps) {
  const base =
    "inline-flex items-center justify-center gap-1.5 rounded-lg font-medium transition-colors disabled:opacity-40 disabled:pointer-events-none";
  const sizes = { sm: "px-3 py-1.5 text-sm", md: "px-4 py-2 text-sm" };
  const variants = {
    accent: "bg-accent text-white hover:bg-accent-hi",
    ghost: "border border-line-strong bg-surface text-ink hover:border-ink/30",
    danger: "border border-fail/40 text-fail hover:bg-fail-soft",
  };
  return <button className={cx(base, sizes[size], variants[variant], className)} {...rest} />;
}

/* ─── Inputs ───────────────────────────────────────────────────────── */
export function Input({ className, ...rest }: InputHTMLAttributes<HTMLInputElement>) {
  return (
    <input
      className={cx(
        "w-full rounded-lg border border-line-strong bg-surface px-3 py-2 text-sm text-ink placeholder:text-faint",
        "focus:border-accent focus-visible:outline-none",
        className,
      )}
      {...rest}
    />
  );
}

export function Field({ label, hint, children }: { label: string; hint?: ReactNode; children: ReactNode }) {
  return (
    <label className="block">
      <span className="mb-1.5 flex items-baseline justify-between">
        <span className="text-sm font-medium text-ink">{label}</span>
        {hint && <span className="text-xs text-faint">{hint}</span>}
      </span>
      {children}
    </label>
  );
}

/* ─── Surfaces ─────────────────────────────────────────────────────── */
export function Card({ className, children }: { className?: string; children: ReactNode }) {
  return <section className={cx("rounded-2xl border border-line bg-surface", className)}>{children}</section>;
}

export function Notice({ tone = "fail", children }: { tone?: Tone; children: ReactNode }) {
  const bg: Record<Tone, string> = { run: "bg-run-soft text-run", warm: "bg-warm-soft text-warm", idle: "bg-idle-soft text-ink", fail: "bg-fail-soft text-fail" };
  return <p className={cx("rounded-lg px-3 py-2 text-sm", bg[tone])}>{children}</p>;
}

/* Copyable mono value (SSH command, endpoint…). */
export function CopyRow({ value }: { value: string }) {
  const [done, setDone] = useState(false);
  return (
    <button
      onClick={() => {
        navigator.clipboard?.writeText(value).then(() => {
          setDone(true);
          setTimeout(() => setDone(false), 1200);
        });
      }}
      className="group inline-flex max-w-full items-center gap-2 rounded-lg border border-line bg-sunk px-2.5 py-1.5 text-left transition-colors hover:border-line-strong"
      title="Copy"
    >
      <code className="mono truncate text-xs text-ink">{value}</code>
      <span className="mono shrink-0 text-[10px] uppercase tracking-wider text-faint group-hover:text-accent">
        {done ? "copied" : "copy"}
      </span>
    </button>
  );
}

/* Multi-line copyable block (ssh config, manifests…). */
export function CopyBlock({ value }: { value: string }) {
  const [done, setDone] = useState(false);
  return (
    <div className="relative overflow-hidden rounded-lg border border-line bg-sunk">
      <button
        onClick={() => {
          navigator.clipboard?.writeText(value).then(() => {
            setDone(true);
            setTimeout(() => setDone(false), 1200);
          });
        }}
        className="absolute right-2 top-2 rounded-md border border-line bg-surface px-2 py-1 text-[10px] uppercase tracking-wider text-faint transition-colors hover:text-accent"
        title="Copy"
      >
        {done ? "copied" : "copy"}
      </button>
      <pre className="mono overflow-x-auto p-3 pr-16 text-xs leading-relaxed text-ink">{value}</pre>
    </div>
  );
}

/* ─── App shell: top command bar + identity menu ───────────────────── */
function IdentityMenu({ me }: { me?: Me }) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const h = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", h);
    return () => document.removeEventListener("mousedown", h);
  }, [open]);

  const cpu = me?.usage.compute.cpu;
  const cpuLimit = me?.quota.compute?.cpu;

  return (
    <div className="relative" ref={ref}>
      <button
        onClick={() => setOpen((v) => !v)}
        className="flex items-center gap-2 rounded-lg border border-line bg-surface px-2.5 py-1.5 text-sm hover:border-line-strong"
      >
        <span className="grid size-6 place-items-center rounded-md bg-accent-soft text-xs font-semibold text-accent">
          {(me?.user ?? "?").slice(0, 1).toUpperCase()}
        </span>
        <span className="mono hidden max-w-[10rem] truncate text-ink sm:inline">{me?.user ?? "…"}</span>
        <svg width="10" height="10" viewBox="0 0 10 10" className="text-faint" aria-hidden>
          <path d="M2 3.5L5 6.5L8 3.5" stroke="currentColor" strokeWidth="1.3" fill="none" strokeLinecap="round" />
        </svg>
      </button>

      {open && (
        <div className="absolute right-0 z-20 mt-2 w-64 overflow-hidden rounded-xl border border-line bg-surface shadow-lg shadow-black/5">
          <div className="border-b border-line px-4 py-3">
            <div className="mono truncate text-sm text-ink">{me?.user}</div>
            <div className="mt-0.5 flex items-center gap-1.5 text-xs text-muted">
              {me?.admin && <span className="rounded bg-accent-soft px-1.5 py-0.5 text-[10px] font-medium text-accent">ADMIN</span>}
              <span>
                {me?.usage.devpods ?? 0} devpod{me?.usage.devpods === 1 ? "" : "s"} · {me?.usage.running ?? 0} running
              </span>
            </div>
            {cpu && (
              <div className="mt-2.5">
                <div className="mb-1 flex items-baseline justify-between">
                  <span className="eyebrow">compute</span>
                  <span className="mono text-xs text-muted">
                    {cpu}<span className="text-faint">/{cpuLimit ?? "∞"}</span> cpu
                  </span>
                </div>
                <CoreMeter used={Number(cpu) || 0} limit={cpuLimit ? Number(cpuLimit) : undefined} />
              </div>
            )}
          </div>
          <nav className="py-1 text-sm">
            {me?.features?.pubkeySelfService !== false && <MenuLink to="/settings/pubkeys" label="SSH keys" onNav={() => setOpen(false)} />}
            {me?.hasPassword && <MenuLink to="/settings/password" label="Password" onNav={() => setOpen(false)} />}
            {me?.admin && <MenuLink to="/admin/users" label="Users" onNav={() => setOpen(false)} />}
            <button
              onClick={() => logout().finally(() => (window.location.href = "/login"))}
              className="block w-full px-4 py-2 text-left text-ink hover:bg-sunk"
            >
              Log out
            </button>
          </nav>
        </div>
      )}
    </div>
  );
}

function MenuLink({ to, label, onNav }: { to: string; label: string; onNav: () => void }) {
  return (
    <Link to={to} onClick={onNav} className="block px-4 py-2 text-ink hover:bg-sunk">
      {label}
    </Link>
  );
}

export function Shell({ children }: { children: ReactNode }) {
  const meQ = useQuery({ queryKey: ["me"], queryFn: fetchMe });
  const loc = useLocation();
  const onNew = loc.pathname === "/devpods/new";
  return (
    <div className="min-h-screen">
      <header className="sticky top-0 z-10 border-b border-line bg-paper/85 backdrop-blur">
        <div className="mx-auto flex h-14 max-w-3xl items-center justify-between gap-3 px-4 sm:px-6">
          <Link to="/" className="flex items-center gap-2">
            <span className="text-accent" aria-hidden>◈</span>
            <span className="mono text-sm font-semibold tracking-tight text-ink">devpod</span>
          </Link>
          <div className="flex items-center gap-2">
            {!onNew && (
              <Link to="/devpods/new" className="inline-flex items-center gap-1 rounded-lg bg-accent px-3 py-1.5 text-sm font-medium text-white transition-colors hover:bg-accent-hi">
                <span className="hidden sm:inline">New</span>
                <span aria-hidden>+</span>
              </Link>
            )}
            <IdentityMenu me={meQ.data} />
          </div>
        </div>
      </header>
      <main className="mx-auto w-full max-w-3xl px-4 pb-16 pt-6 sm:px-6 sm:pt-10">{children}</main>
    </div>
  );
}

/* Bare page for pre-auth (login). */
export function CenterPage({ children }: { children: ReactNode }) {
  return <main className="grid min-h-screen place-items-center px-4 py-10">{children}</main>;
}

export function BackLink({ to = "/", label = "Environments" }: { to?: string; label?: string }) {
  return (
    <Link to={to} className="mono inline-flex items-center gap-1 text-xs text-muted hover:text-accent">
      <span aria-hidden>←</span> {label}
    </Link>
  );
}
