// Phase as a colored dot AND its name (color never stands alone).
const labels: Record<string, string> = { Running: "运行中", Pending: "启动中", Stopped: "已休眠", Failed: "失败" };
const tones: Record<string, string> = { Running: "bg-run", Pending: "bg-warm", Stopped: "bg-idle", Failed: "bg-fail" };

export function phaseLabel(phase?: string): string {
  if (!phase) return "未知";
  return labels[phase] ?? phase;
}

export function StatusDot({ phase, showLabel = true }: { phase?: string; showLabel?: boolean }) {
  const tone = tones[phase ?? ""] ?? "bg-idle";
  return (
    <span className="inline-flex items-center gap-1.5 whitespace-nowrap">
      <span aria-hidden="true" className={`inline-block h-2 w-2 shrink-0 rounded-full ${tone} ${phase === "Pending" ? "pulse" : ""}`} />
      {showLabel ? <span>{phaseLabel(phase)}</span> : <span className="sr-only">{phaseLabel(phase)}</span>}
    </span>
  );
}
