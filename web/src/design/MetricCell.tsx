// One metric in a table cell: sparkline / meter plus text, or "—" with
// the degradation hint when there is no telemetry (UI spec §6.3).
import { Meter } from "./Meter";
import { Sparkline } from "./Sparkline";

export const NO_DATA_HINT = "监控在 Pod 下次重建后生效";

export function MetricCell({
  value,
  text,
  spark,
  sparkMax,
  meter,
  hint = NO_DATA_HINT,
}: {
  value?: number | null;
  text?: string;
  spark?: (number | null)[];
  sparkMax?: number;
  meter?: { used: number; limit: number };
  hint?: string;
}) {
  if (value == null) {
    const hasCurve = spark?.some((v) => v != null);
    return (
      <span className="inline-flex min-w-0 items-center gap-1.5">
        {hasCurve && <Sparkline values={spark!} max={sparkMax} width={48} height={14} />}
        <span className="text-faint" title={hasCurve ? undefined : hint} aria-label={hasCurve ? undefined : hint}>
          —
        </span>
      </span>
    );
  }
  return (
    <span className="inline-flex min-w-0 items-center gap-1.5">
      {spark && <Sparkline values={spark} max={sparkMax} width={48} height={14} />}
      {meter && <Meter used={meter.used} limit={meter.limit} width={40} />}
      {text && <span className="mono truncate">{text}</span>}
    </span>
  );
}
