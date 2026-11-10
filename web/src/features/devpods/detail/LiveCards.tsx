// Six live cards (UI spec §5.2): value + 60 s sparkline each, "—" without
// telemetry.
import type { ReactNode } from "react";
import type { DevPodSample, MetricSeries } from "../../../data/api";
import { sparkSeries } from "../../../lib/spark";
import type { LiveRow } from "../../../data/live/LiveHub";
import { Sparkline } from "../../../design/Sparkline";
import { NO_DATA_HINT } from "../../../design/MetricCell";
import { fmtBytes, fmtCores, fmtRate } from "../../../lib/format";

function Card({ id, label, value, spark, max, sub }: { id: string; label: string; value?: ReactNode; spark?: (number | null)[]; max?: number; sub?: ReactNode }) {
  return (
    <div data-testid={`card-${id}`} className="min-w-0 rounded-lg border border-line bg-surface px-3 py-2">
      <div className="text-[11px] text-muted">{label}</div>
      {value === undefined ? (
        <div className="mono text-base text-faint" title={NO_DATA_HINT} aria-label={NO_DATA_HINT}>
          —
        </div>
      ) : (
        <div className="mono truncate text-base">{value}</div>
      )}
      <div className="flex h-4 items-center justify-between gap-2">
        {spark && spark.some((v) => v != null) ? <Sparkline values={spark} max={max} width={120} height={16} /> : <span />}
        {sub && <span className="mono truncate text-[11px] text-faint">{sub}</span>}
      </div>
    </div>
  );
}

const sum = (xs: number[]) => xs.reduce((a, b) => a + b, 0);

/** combine sums (avg: averages) the history series picked by keep. */
function combine(s: MetricSeries | undefined, keep: (k: string) => boolean, avg = false): (number | null)[] | undefined {
  if (!s) return undefined;
  const cols = Object.entries(s.series).filter(([k]) => keep(k)).map(([, v]) => v);
  if (!cols.length) return undefined;
  return s.ts.map((_, i) => {
    const vs = cols.map((c) => c[i]).filter((v): v is number => v != null);
    return vs.length ? sum(vs) / (avg ? vs.length : 1) : null;
  });
}

/** The cards' sparklines: 15 min of history (ClickHouse) plus the live
 *  tail, so they are drawn as soon as the page opens. */
export function LiveCards({ row, hist }: { row?: LiveRow<DevPodSample>; hist?: MetricSeries }) {
  const s = row?.value;
  const c = s?.container;
  const gpus = s?.gpus ?? [];
  const rdma = s?.rdma ?? [];
  const gpuAvg = (x: DevPodSample) => ((x.gpus ?? []).length ? sum((x.gpus ?? []).map((g) => g.util)) / (x.gpus ?? []).length : null);
  const now = Date.now();
  const liveTail = row?.history ?? [];
  const spark = (keep: (k: string) => boolean, pick: (x: DevPodSample) => number | null | undefined, avg = false) => {
    const v = combine(hist, keep, avg);
    return sparkSeries(v && hist ? { ts: hist.ts, v } : undefined, liveTail.map((x) => ({ ts: x.ts, v: pick(x.value) })), now);
  };
  return (
    <div className="grid grid-cols-6 gap-2 max-[1100px]:grid-cols-3 max-[700px]:grid-cols-2">
      <Card
        id="cpu"
        label="CPU"
        value={c ? `${fmtCores(c.cpu)}/${c.cpu_limit ? fmtCores(c.cpu_limit) : "∞"}` : undefined}
        spark={spark((k) => k === "cpu", (x) => x.container?.cpu)}
        max={c?.cpu_limit || undefined}
        sub={c?.throttled_usec ? "限流中" : undefined}
      />
      <Card
        id="mem"
        label="内存"
        value={c ? `${fmtBytes(c.mem_ws)}/${c.mem_limit ? fmtBytes(c.mem_limit) : "∞"}` : undefined}
        spark={spark((k) => k === "mem_anon" || k === "mem_file", (x) => x.container?.mem_ws)}
        max={c?.mem_limit || undefined}
        sub={c?.oom_kills ? `OOM ${c.oom_kills}` : undefined}
      />
      <Card
        id="gpu"
        label="GPU"
        value={!s ? undefined : gpus.length ? `${Math.round(gpuAvg(s) ?? 0)}%` : "无"}
        spark={spark((k) => /^gpu\..+\.util$/.test(k), gpuAvg, true)}
        max={100}
        sub={gpus.length ? `${gpus.length} 卡` : undefined}
      />
      <Card
        id="net"
        label="网络 ↓/↑"
        value={c ? `↓${fmtRate(c.net_rx)} ↑${fmtRate(c.net_tx)}` : undefined}
        spark={spark((k) => k === "net_rx", (x) => x.container?.net_rx)}
        sub={c?.net_kind === "tcp-only" ? "TCP only" : undefined}
      />
      <Card
        id="rdma"
        label="RDMA ↓/↑"
        value={!s ? undefined : rdma.length ? `↓${fmtRate(sum(rdma.map((r) => r.rx_bps)))} ↑${fmtRate(sum(rdma.map((r) => r.tx_bps)))}` : "无"}
        spark={spark((k) => /^rdma\..+\.rx$/.test(k), (x) => ((x.rdma ?? []).length ? sum((x.rdma ?? []).map((r) => r.rx_bps)) : null))}
        sub={rdma.length ? (rdma[0].scope === "vf_exclusive" ? "VF" : "shared") : undefined}
      />
      <Card
        id="io"
        label="磁盘 IO 读/写"
        value={c ? `${fmtRate(c.io_r)} / ${fmtRate(c.io_w)}` : undefined}
        spark={spark((k) => k === "io_r" || k === "io_w", (x) => (x.container ? x.container.io_r + x.container.io_w : null))}
      />
    </div>
  );
}
