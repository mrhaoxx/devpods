// 监控 (UI spec §5.2, feedback round 1): history from ClickHouse for 15m–7d,
// the live tail appended on the ranges up to 1h; opens on 1h.
import type { Ref } from "../../../data/cluster";
import { useMemo, useState } from "react";
import type { DevPodSample, MetricRange } from "../../../data/api";
import { ApiFailure } from "../../../data/api";
import { useMetrics } from "../../../data/queries";
import { TimeSeriesChart, type ChartSeries } from "../../../design/TimeSeriesChart";
import { Panel } from "../../../design/Panel";
import { EmptyState } from "../../../design/EmptyState";
import { Select } from "../../../design/Form";
import { useUrlState, type Schema } from "../../../lib/urlState";
import { fmtBytes, fmtCores, fmtRate } from "../../../lib/format";
import { activeRDMA, chartsFromLive, chartsFromSeries, lastLimit, mergeLive, type ChartSet } from "./monitor";

// Grafana-like presets; history from ClickHouse, the live tail appended
// for the short ones (UI feedback round 1).
const RANGE_MS: Record<string, number> = {
  "15m": 15 * 60e3,
  "1h": 3600e3,
  "3h": 3 * 3600e3,
  "6h": 6 * 3600e3,
  "12h": 12 * 3600e3,
  "24h": 24 * 3600e3,
  "7d": 7 * 24 * 3600e3,
};
const RANGES = Object.keys(RANGE_MS);
const schema = { range: { kind: "string", default: "" } } satisfies Schema;

// the range picked last, remembered in this browser: where it opens
// when the address does not say
const RANGE_KEY = "devpod.metricsRange";
function lastRange(): string {
  try {
    return localStorage.getItem(RANGE_KEY) ?? "";
  } catch {
    return "";
  }
}
function rememberRange(r: string) {
  try {
    localStorage.setItem(RANGE_KEY, r);
  } catch {
    // not kept: no matter
  }
}
// series colors in fixed order (the validated chart palette)
const palette = ["--chart-1", "--chart-2", "--chart-3", "--chart-4", "--chart-5", "--chart-6", "--chart-7", "--chart-8"];
type GpuMetric = "util" | "mem" | "power" | "temp";
const gpuMetric: Record<GpuMetric, [string, (v: number) => string]> = {
  util: ["利用率", (v) => `${Math.round(v)}%`],
  mem: ["显存", (v) => `${Math.round(v)} MiB`],
  power: ["功耗", (v) => `${Math.round(v)} W`],
  temp: ["温度", (v) => `${Math.round(v)}°C`],
};

function Charts({ c, live }: { c: ChartSet; live: boolean }) {
  const [gm, setGm] = useState<GpuMetric>("util");
  const [splitTcp, setSplitTcp] = useState(false);
  const [rdmaHealth, setRdmaHealth] = useState(false);
  const gpus = Object.entries(c.gpu);
  const rdmas = Object.entries(c.rdma);
  // ports with nothing to show are left out (and counted): a host has hundreds
  const { shown: rdmaShown, idle: rdmaIdle } = activeRDMA(c.rdma, rdmaHealth && live);
  const net: ChartSeries[] = [
    { label: "↓ 接收", values: c.net.rx, color: "--chart-1" },
    { label: "↑ 发送", values: c.net.tx, color: "--chart-2" },
    ...(splitTcp
      ? [
          { label: "↓ TCP", values: c.net.tcpRx, color: "--chart-3" },
          { label: "↑ TCP", values: c.net.tcpTx, color: "--chart-4" },
        ]
      : []),
  ];
  return (
    <div className="grid gap-3 lg:grid-cols-2">
      <Panel title="CPU（核）">
        <div className="p-2">
          <TimeSeriesChart
            ts={c.ts}
            series={[
              { label: "使用", values: c.cpu.used, color: "--chart-1", fill: true },
              { label: "throttled", values: c.cpu.throttled, color: "--chart-2" },
            ]}
            limit={lastLimit(c.cpu.limit)}
            yFormat={fmtCores}
          />
        </div>
      </Panel>
      <Panel title="内存">
        <div className="p-2">
          <TimeSeriesChart
            ts={c.ts}
            stacked
            series={[
              { label: "anon", values: c.mem.anon, color: "--chart-1", fill: true },
              { label: "file", values: c.mem.file, color: "--chart-2", fill: true },
            ]}
            limit={lastLimit(c.mem.limit)}
            yFormat={fmtBytes}
          />
        </div>
      </Panel>
      <Panel
        title="GPU"
        actions={
          gpus.length > 0 && (
            <Select aria-label="GPU 指标" value={gm} onChange={(e) => setGm(e.target.value as GpuMetric)} className="!h-6 !w-24 text-xs">
              {Object.entries(gpuMetric).map(([k, [l]]) => (
                <option key={k} value={k}>
                  {l}
                </option>
              ))}
            </Select>
          )
        }
      >
        {gpus.length ? (
          <div className="p-2">
            <TimeSeriesChart
              ts={c.ts}
              series={gpus.map(([uuid, g], i) => ({ label: g.name ? `${g.name} ${uuid.slice(-6)}` : uuid.slice(-12), values: g[gm], color: palette[i % palette.length] }))}
              yFormat={gpuMetric[gm][1]}
            />
          </div>
        ) : (
          <EmptyState title="没有 GPU" />
        )}
      </Panel>
      <Panel
        title="网络"
        actions={
          <label className="inline-flex items-center gap-1 text-[11px] text-muted">
            <input type="checkbox" checked={splitTcp} onChange={(e) => setSplitTcp(e.target.checked)} /> 拆分 TCP
          </label>
        }
      >
        <div className="p-2">
          <TimeSeriesChart ts={c.ts} series={net} yFormat={fmtRate} />
        </div>
      </Panel>
      <Panel
        title={`RDMA${rdmaShown.length ? ` · ${rdmaShown.map(([k, r]) => `${k}${r.scope ? `（${r.scope}）` : ""}`).join("，")}` : ""}`}
        actions={
          live &&
          rdmas.length > 0 && (
            <label className="inline-flex items-center gap-1 text-[11px] text-muted">
              <input type="checkbox" checked={rdmaHealth} onChange={(e) => setRdmaHealth(e.target.checked)} /> CNP / 重传（实时）
            </label>
          )
        }
      >
        {rdmaShown.length ? (
          <div className="p-2">
            <TimeSeriesChart
              ts={c.ts}
              series={rdmaShown.flatMap(([k, r], i) =>
                rdmaHealth && live
                  ? [
                      { label: `${k} CNP/s`, values: r.cnp ?? [], color: palette[(2 * i) % palette.length] },
                      { label: `${k} 重传/s`, values: r.retrans ?? [], color: palette[(2 * i + 1) % palette.length] },
                    ]
                  : [
                      { label: `${k} ↓`, values: r.rx, color: palette[(2 * i) % palette.length] },
                      { label: `${k} ↑`, values: r.tx, color: palette[(2 * i + 1) % palette.length] },
                    ],
              )}
              yFormat={rdmaHealth && live ? (v) => String(Math.round(v)) : fmtRate}
            />
            {rdmaIdle > 0 && <p className="mt-1 text-[11px] text-muted">另有 {rdmaIdle} 个端口无流量，未显示</p>}
          </div>
        ) : rdmas.length ? (
          <EmptyState title={`${rdmas.length} 个 RDMA 端口都没有流量`} />
        ) : (
          <EmptyState title="没有 RDMA 设备" />
        )}
      </Panel>
      <Panel title="磁盘 IO">
        <div className="p-2">
          <TimeSeriesChart
            ts={c.ts}
            series={[
              { label: "读", values: c.io.r, color: "--chart-1" },
              { label: "写", values: c.io.w, color: "--chart-2" },
            ]}
            yFormat={fmtRate}
          />
        </div>
      </Panel>
    </div>
  );
}

export function MonitorTab({ r, history }: { r: Ref; history?: ReadonlyArray<{ ts: number; value: DevPodSample }> }) {
  const [url, setUrl] = useUrlState(schema);
  const range = RANGE_MS[url.range] ? url.range : RANGE_MS[lastRange()] ? lastRange() : "1h";
  const rangeMs = RANGE_MS[range];
  const hist = useMetrics(r, range as MetricRange);
  // the live tail extends the short ranges second by second
  const live = rangeMs <= 3600e3 && !!history?.length;
  const charts = useMemo(() => {
    if (!hist.data) return null;
    const base = chartsFromSeries(hist.data);
    return live ? mergeLive(base, chartsFromLive(history!), rangeMs, Date.now()) : base;
  }, [hist.data, history, live, rangeMs]);
  return (
    <div className="space-y-3">
      <div role="radiogroup" aria-label="时间范围" className="inline-flex rounded-md border border-line bg-sunk p-0.5 text-xs">
        {RANGES.map((id) => (
          <button
            key={id}
            type="button"
            role="radio"
            aria-checked={range === id}
            onClick={() => {
              rememberRange(id);
              setUrl({ range: id });
            }}
            className={`rounded px-2.5 py-0.5 ${range === id ? "bg-surface font-medium shadow-sm" : "text-muted hover:text-ink"}`}
          >
            {id}
          </button>
        ))}
      </div>
      {hist.isError ? (
        <EmptyState
          tone="error"
          title="历史数据暂不可用"
          hint={hist.error instanceof ApiFailure ? hist.error.body.message : String(hist.error)}
        />
      ) : charts ? (
        <Charts c={charts} live={live} />
      ) : (
        <EmptyState title="加载中…" />
      )}
    </div>
  );
}
