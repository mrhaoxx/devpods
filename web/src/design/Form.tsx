import type { InputHTMLAttributes, ReactNode, SelectHTMLAttributes, TextareaHTMLAttributes } from "react";

const control =
  "w-full rounded-md border border-line-strong bg-surface px-2.5 text-sm text-ink placeholder:text-faint focus:border-accent focus-visible:outline-none disabled:opacity-60";

export function Input({ className = "", ...rest }: InputHTMLAttributes<HTMLInputElement>) {
  return <input {...rest} className={`${control} h-8 ${className}`} />;
}

export function Textarea({ className = "", ...rest }: TextareaHTMLAttributes<HTMLTextAreaElement>) {
  return <textarea {...rest} className={`${control} py-2 ${className}`} />;
}

export function Select({ className = "", children, ...rest }: SelectHTMLAttributes<HTMLSelectElement>) {
  return (
    <select {...rest} className={`${control} h-8 ${className}`}>
      {children}
    </select>
  );
}

/** A labelled form row; error is shown under the control. */
export function Field({ label, hint, error, children }: { label: string; hint?: ReactNode; error?: string; children: ReactNode }) {
  return (
    <label className="block">
      <span className="mb-1 flex items-baseline justify-between gap-2">
        <span className="text-xs font-medium text-ink">{label}</span>
        {hint && <span className="text-[11px] text-faint">{hint}</span>}
      </span>
      {children}
      {error && (
        <span role="alert" className="mt-1 block text-[11px] text-fail">
          {error}
        </span>
      )}
    </label>
  );
}

type Tone = "fail" | "warm" | "run" | "idle";
const tones: Record<Tone, string> = {
  fail: "bg-fail-soft text-fail",
  warm: "bg-warm-soft text-warm",
  run: "bg-run-soft text-run",
  idle: "bg-idle-soft text-ink",
};

export function Notice({ tone = "fail", children }: { tone?: Tone; children: ReactNode }) {
  return (
    <p role={tone === "fail" ? "alert" : undefined} className={`rounded-md px-3 py-2 text-xs ${tones[tone]}`}>
      {children}
    </p>
  );
}
