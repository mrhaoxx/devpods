import { describe, expect, it } from "vitest";
import { activeRDMA, chartsFromLive, chartsFromSeries, mergeLive } from "./monitor";
import type { DevPodSample, MetricSeries } from "../../../data/api";

const series: MetricSeries = {
  step: 5,
  ts: [100, 105, 110],
  series: {
    cpu: [1, 2, null],
    cpu_limit: [8, 8, 8],
    mem_anon: [10, 20, 30],
    "gpu.GPU-abc.util": [50, 60, 70],
    "gpu.GPU-abc.temp": [40, 41, 42],
    "rdma.mlx5_0/1.rx": [1, 2, 3],
    "rdma.mlx5_0/1.tx": [4, 5, 6],
  },
};

const live = (ts: number, health: Record<string, number>): { ts: number; value: DevPodSample } => ({
  ts,
  value: {
    ts: "",
    container: { cpu: 1 } as DevPodSample["container"],
    gpus: [],
    rdma: [{ ts: "", ibdev: "mlx5_0", port: 1, rx_bps: 10, tx_bps: 20, tx_pps: 0, rx_pps: 0, scope: "vf_exclusive", health }],
  },
});

describe("monitor data", () => {
  it("chartsFromSeries maps cpu, mem and gpu.<uuid>.util keys", () => {
    const c = chartsFromSeries(series);
    expect(c.ts).toEqual([100_000, 105_000, 110_000]);
    expect(c.cpu.used).toEqual([1, 2, null]);
    expect(c.cpu.limit).toEqual([8, 8, 8]);
    expect(c.mem.anon).toEqual([10, 20, 30]);
    expect(c.gpu["GPU-abc"].util).toEqual([50, 60, 70]);
    expect(c.gpu["GPU-abc"].temp).toEqual([40, 41, 42]);
  });
  it("chartsFromSeries groups rdma.<dev>/<port>.rx|tx", () => {
    const c = chartsFromSeries(series);
    expect(Object.keys(c.rdma)).toEqual(["mlx5_0/1"]);
    expect(c.rdma["mlx5_0/1"].rx).toEqual([1, 2, 3]);
    expect(c.rdma["mlx5_0/1"].tx).toEqual([4, 5, 6]);
  });
  it("missing keys become all-null series of the right length", () => {
    const c = chartsFromSeries(series);
    expect(c.mem.file).toEqual([null, null, null]);
    expect(c.io.w).toEqual([null, null, null]);
    expect(c.gpu["GPU-abc"].power).toEqual([null, null, null]);
  });
  it("chartsFromLive derives cnp from health counters", () => {
    const c = chartsFromLive([live(0, { np_cnp_sent: 10, rp_cnp_handled: 0, packet_seq_err: 1 }), live(1000, { np_cnp_sent: 30, rp_cnp_handled: 5, packet_seq_err: 4 })]);
    expect(c.rdma["mlx5_0/1"].cnp).toEqual([null, 25]);
    expect(c.rdma["mlx5_0/1"].retrans).toEqual([null, 3]);
    expect(c.rdma["mlx5_0/1"].scope).toBe("VF");
    expect(c.cpu.used).toEqual([1, 1]);
  });
  it("activeRDMA: only ports with something to show for the metric looked at; the rest counted", () => {
    const rdma = {
      "mlx5_0/1": { rx: [0, 5, null], tx: [0, 0, 0], cnp: [0, 0, 0], retrans: [0, 0, 0] },
      "mlx5_bond_1/7": { rx: [0, 0, null], tx: [null, 0, 0], cnp: [0, 2, 0], retrans: [0, 0, 0] },
      "mlx5_bond_1/8": { rx: [null, null, null], tx: [0, 0, 0] },
    };
    const traffic = activeRDMA(rdma, false);
    expect(traffic.shown.map(([k]) => k)).toEqual(["mlx5_0/1"]);
    expect(traffic.idle).toBe(2);
    const health = activeRDMA(rdma, true);
    expect(health.shown.map(([k]) => k)).toEqual(["mlx5_bond_1/7"]);
    expect(health.idle).toBe(2);
  });

  it("mergeLive appends only samples past the history's last bucket and keeps the window", () => {
    const hist = chartsFromSeries({ step: 5, ts: [0, 5], series: { cpu: [1, 2] } });
    const lv = chartsFromLive([4, 9, 11].map((s, i) => ({ ...live(s * 1000, {}), value: { ...live(0, {}).value, container: { cpu: [9, 3, 4][i] } as DevPodSample["container"] } })));
    const m = mergeLive(hist, lv, 10_000, 11_000);
    expect(m.ts).toEqual([5_000, 11_000]);
    expect(m.cpu.used).toEqual([2, 4]);
    expect(m.mem.anon).toHaveLength(2);
    expect(Object.keys(m.rdma)).toEqual(["mlx5_0/1"]);
    expect(m.rdma["mlx5_0/1"].rx).toEqual([null, 10]);
  });
});
