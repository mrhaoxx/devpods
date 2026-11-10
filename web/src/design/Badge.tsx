import type { ReactNode } from "react";

type Tone = "neutral" | "accent" | "run" | "warm" | "fail" | "idle";
const tones: Record<Tone, string> = {
  neutral: "bg-sunk text-muted",
  accent: "bg-accent-soft text-accent",
  run: "bg-run-soft text-run",
  warm: "bg-warm-soft text-warm",
  fail: "bg-fail-soft text-fail",
  idle: "bg-idle-soft text-idle",
};

export function Badge({ tone = "neutral", children, title }: { tone?: Tone; children: ReactNode; title?: string }) {
  return (
    <span title={title} className={`inline-flex h-[18px] items-center rounded px-1.5 text-[11px] font-medium whitespace-nowrap ${tones[tone]}`}>
      {children}
    </span>
  );
}
