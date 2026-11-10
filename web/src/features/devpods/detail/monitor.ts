// Turns history (GET /metrics) and the live ring (1 s range) into the
// monitor tab's chart series (UI spec §5.2).
import type { DevPodSample, MetricSeries } from "../../../data/api";

type S = (number | null)[];
export type ChartSet = {
  ts: number[]; // ms
  cpu: { used: S; limit: S; throttled: S };
  mem: { anon: S; file: S; limit: S };
  gpu: Record<string, { name?: string; util: S; mem: S; power: S; temp: S }>;
  net: { rx: S; tx: S; tcpRx: S; tcpTx: S };
  rdma: Record<string, { scope?: string; rx: S; tx: S; cnp?: S; retrans?: S }>;
  io: { r: S; w: S };
};

const nulls = (n: number): S => new Array(n).fill(null);

export function chartsFromSeries(s: MetricSeries): ChartSet {
  const n = s.ts.length;
  const get = (k: string): S => s.series[k] ?? nulls(n);
  const gpu: ChartSet["gpu"] = {};
  const rdma: ChartSet["rdma"] = {};
  for (const key of Object.keys(s.series)) {
    const g = /^gpu\.(.+)\.(util|mem|power|temp)$/.exec(key);
    if (g) gpu[g[1]] ??= { util: get(`gpu.${g[1]}.util`), mem: get(`gpu.${g[1]}.mem`), power: get(`gpu.${g[1]}.power`), temp: get(`gpu.${g[1]}.temp`) };
    const r = /^rdma\.(.+)\.(rx|tx)$/.exec(key);
    if (r) rdma[r[1]] ??= { rx: get(`rdma.${r[1]}.rx`), tx: get(`rdma.${r[1]}.tx`) };
  }
  return {
    ts: s.ts.map((t) => t * 1000),
    cpu: { used: get("cpu"), limit: get("cpu_limit"), throttled: get("cpu_throttled") },
    mem: { anon: get("mem_anon"), file: get("mem_file"), limit: get("mem_limit") },
    gpu,
    net: { rx: get("net_rx"), tx: get("net_tx"), tcpRx: get("net_tcp_rx"), tcpTx: get("net_tcp_tx") },
    rdma,
    io: { r: get("io_r"), w: get("io_w") },
  };
}

const RETRANS = ["packet_seq_err", "out_of_sequence", "local_ack_timeout_err", "implied_nak_seq_err", "duplicate_request"];
const cnpOf = (h: Record<string, number> = {}) => Object.entries(h).reduce((a, [k, v]) => (k.includes("cnp") ? a + v : a), 0);
const retransOf = (h: Record<string, number> = {}) => RETRANS.reduce((a, k) => a + (h[k] ?? 0), 0);

/** chartsFromLive: the 60 s ring. CNP and retransmissions are rates of
 *  the cumulative hw_counters (live only: history keeps rx/tx). */
export function chartsFromLive(history: ReadonlyArray<{ ts: number; value: DevPodSample }>): ChartSet {
  const n = history.length;
  const at = <T>(f: (x: DevPodSample, i: number) => T | null | undefined): (T | null)[] => history.map((h, i) => f(h.value, i) ?? null);
  const c = (k: keyof DevPodSample["container"]) => at((x) => (x.container ? (x.container[k] as number) : null));
  const dt = (i: number) => (i > 0 ? (history[i].ts - history[i - 1].ts) / 1000 : 0);
  const throttled = at((x, i) => {
    if (i === 0 || !x.container || !history[i - 1].value.container || dt(i) <= 0) return null;
    return Math.max(0, x.container.throttled_usec - history[i - 1].value.container.throttled_usec) / 1e6 / dt(i);
  });
  const gpu: ChartSet["gpu"] = {};
  const rdma: ChartSet["rdma"] = {};
  history.forEach((h, i) => {
    for (const g of h.value.gpus ?? []) {
      const e = (gpu[g.uuid] ??= { name: g.name, util: nulls(n), mem: nulls(n), power: nulls(n), temp: nulls(n) });
      e.util[i] = g.util;
      e.mem[i] = g.mem_used;
      e.power[i] = g.power;
      e.temp[i] = g.temp;
    }
    for (const r of h.value.rdma ?? []) {
      const key = `${r.ibdev}/${r.port}`;
      const e = (rdma[key] ??= { scope: r.scope === "vf_exclusive" ? "VF" : "shared", rx: nulls(n), tx: nulls(n), cnp: nulls(n), retrans: nulls(n) });
      e.rx[i] = r.rx_bps;
      e.tx[i] = r.tx_bps;
      const prev = i > 0 ? (history[i - 1].value.rdma ?? []).find((p) => p.ibdev === r.ibdev && p.port === r.port) : undefined;
      if (prev && dt(i) > 0) {
        e.cnp![i] = Math.max(0, cnpOf(r.health) - cnpOf(prev.health)) / dt(i);
        e.retrans![i] = Math.max(0, retransOf(r.health) - retransOf(prev.health)) / dt(i);
      }
    }
  });
  return {
    ts: history.map((h) => h.ts),
    cpu: { used: c("cpu"), limit: c("cpu_limit"), throttled },
    mem: { anon: c("mem_anon"), file: c("mem_file"), limit: c("mem_limit") },
    gpu,
    net: { rx: c("net_rx"), tx: c("net_tx"), tcpRx: c("net_tcp_rx"), tcpTx: c("net_tcp_tx") },
    rdma,
    io: { r: c("io_r"), w: c("io_w") },
  };
}

/** The limit line: the latest non-null, non-zero value (0 = unlimited). */
export function lastLimit(s: S): number | null {
  for (let i = s.length - 1; i >= 0; i--) if (s[i] != null && s[i]! > 0) return s[i];
  return null;
}
