import type { ButtonHTMLAttributes } from "react";

type Variant = "primary" | "ghost" | "danger" | "default";
const variants: Record<Variant, string> = {
  primary: "bg-accent text-white hover:bg-accent-hi border-transparent",
  default: "bg-surface text-ink hover:bg-sunk border-line-strong",
  ghost: "bg-transparent text-muted hover:bg-sunk hover:text-ink border-transparent",
  danger: "bg-surface text-fail hover:bg-fail-soft border-line-strong",
};

export function Button({
  variant = "default",
  size = "sm",
  className = "",
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: Variant; size?: "sm" | "md" }) {
  const sz = size === "sm" ? "h-7 px-2.5 text-xs" : "h-8 px-3 text-sm";
  return (
    <button
      type="button"
      {...props}
      className={`inline-flex items-center justify-center gap-1.5 rounded-md border font-medium whitespace-nowrap transition-colors disabled:cursor-not-allowed disabled:opacity-50 ${sz} ${variants[variant]} ${className}`}
    />
  );
}
