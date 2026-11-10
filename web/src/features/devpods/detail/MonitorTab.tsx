// 监控 (UI spec §5.2): 实时 1s from the live ring, 15m–7d from history
// (the server switches to minute data beyond its 1 s retention).
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
import { chartsFromLive, chartsFromSeries, lastLimit, type ChartSet } from "./monitor";

const RANGES: [string, string][] = [
  ["live", "实时 1s"],
  ["15m", "15m"],
  ["1h", "1h"],
  ["24h", "24h"],
  ["7d", "7d"],
];
const schema = { range: { kind: "string", default: "live" } } satisfies Schema;
const palette = ["--color-accent", "--color-run", "--color-warm", "--color-fail", "--color-idle"];
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
  const net: ChartSeries[] = [
    { label: "↓ 接收", values: c.net.rx, color: "--color-accent" },
    { label: "↑ 发送", values: c.net.tx, color: "--color-run" },
    ...(splitTcp
      ? [
          { label: "↓ TCP", values: c.net.tcpRx, color: "--color-warm", dash: true },
          { label: "↑ TCP", values: c.net.tcpTx, color: "--color-idle", dash: true },
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
              { label: "使用", values: c.cpu.used, color: "--color-accent", fill: true },
              { label: "throttled", values: c.cpu.throttled, color: "--color-warm" },
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
              { label: "anon", values: c.mem.anon, color: "--color-accent", fill: true },
              { label: "file", values: c.mem.file, color: "--color-run", fill: true },
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
        title={`RDMA${rdmas.length ? ` · ${rdmas.map(([k, r]) => `${k}${r.scope ? `（${r.scope}）` : ""}`).join("，")}` : ""}`}
        actions={
          live &&
          rdmas.length > 0 && (
            <label className="inline-flex items-center gap-1 text-[11px] text-muted">
              <input type="checkbox" checked={rdmaHealth} onChange={(e) => setRdmaHealth(e.target.checked)} /> CNP / 重传
            </label>
          )
        }
      >
        {rdmas.length ? (
          <div className="p-2">
            <TimeSeriesChart
              ts={c.ts}
              series={rdmas.flatMap(([k, r], i) =>
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
          </div>
        ) : (
          <EmptyState title="没有 RDMA 设备" />
        )}
      </Panel>
      <Panel title="磁盘 IO">
        <div className="p-2">
          <TimeSeriesChart
            ts={c.ts}
            series={[
              { label: "读", values: c.io.r, color: "--color-accent" },
              { label: "写", values: c.io.w, color: "--color-warm" },
            ]}
            yFormat={fmtRate}
          />
        </div>
      </Panel>
    </div>
  );
}

export function MonitorTab({ name, history }: { name: string; history?: ReadonlyArray<{ ts: number; value: DevPodSample }> }) {
  const [url, setUrl] = useUrlState(schema);
  const live = url.range === "live";
  const hist = useMetrics(name, live ? undefined : (url.range as MetricRange));
  const charts = useMemo(() => {
    if (live) return history && history.length ? chartsFromLive(history) : null;
    return hist.data ? chartsFromSeries(hist.data) : null;
  }, [live, history, hist.data]);
  return (
    <div className="space-y-3">
      <div role="radiogroup" aria-label="时间范围" className="inline-flex rounded-md border border-line bg-sunk p-0.5 text-xs">
        {RANGES.map(([id, label]) => (
          <button
            key={id}
            type="button"
            role="radio"
            aria-checked={url.range === id}
            onClick={() => setUrl({ range: id })}
            className={`rounded px-2.5 py-0.5 ${url.range === id ? "bg-surface font-medium shadow-sm" : "text-muted hover:text-ink"}`}
          >
            {label}
          </button>
        ))}
      </div>
      {!live && hist.isError ? (
        <EmptyState
          tone="error"
          title="历史数据暂不可用"
          hint={hist.error instanceof ApiFailure ? hist.error.body.message : String(hist.error)}
        />
      ) : charts ? (
        <Charts c={charts} live={live} />
      ) : (
        <EmptyState title={live ? "等待实时数据…" : "加载中…"} hint={live && !history ? "监控在 Pod 下次重建后生效" : undefined} />
      )}
    </div>
  );
}
