// Spinner: something is being done (a save, a start…) and is not done yet.
export function Spinner({ className = "" }: { className?: string }) {
  return <span aria-hidden className={`inline-block size-2.5 shrink-0 animate-spin rounded-full border border-current border-t-transparent ${className}`} />;
}
