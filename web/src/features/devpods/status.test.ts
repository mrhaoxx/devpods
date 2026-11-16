import { describe, expect, it } from "vitest";
import { phaseSummary, reservationNotes } from "./status";
import type { DevPod, Reservation } from "../../data/api";

const now = new Date("2026-11-10T12:00:00Z");
const dp = (spec: Partial<DevPod["spec"]>, status: DevPod["status"]): DevPod => ({
  metadata: { name: "alice-x" },
  spec: { owner: "alice", running: true, ...spec },
  status,
});

describe("phaseSummary", () => {
  it("is null while running", () => {
    expect(phaseSummary(dp({}, { phase: "Running" }), undefined, now)).toBeNull();
  });
  it("Pending shows the controller's message, else 启动中", () => {
    expect(phaseSummary(dp({}, { phase: "Pending", message: "拉取镜像 pytorch:2.6（38%）" }), undefined, now)).toBe("拉取镜像 pytorch:2.6（38%）");
    expect(phaseSummary(dp({}, { phase: "Pending" }), undefined, now)).toBe("启动中");
  });
  it("Stopped shows hibernated age and PVC size", () => {
    const s = phaseSummary(dp({ running: false, persistence: { size: "50Gi" } }, { phase: "Stopped", hibernatedAt: "2026-11-07T12:00:00Z" }), undefined, now);
    expect(s).toBe("已休眠 3d · PVC 50Gi");
  });
  it("Stopped without persistence says just 已休眠", () => {
    expect(phaseSummary(dp({ running: false }, { phase: "Stopped" }), undefined, now)).toBe("已休眠");
  });
  it("Failed shows the reason and the retry countdown", () => {
    const s = phaseSummary(dp({}, { phase: "Failed", message: "ImagePullBackOff", retryCount: 2, lastFailureAt: "2026-11-10T11:59:50Z" }), undefined, now);
    expect(s).toBe("ImagePullBackOff · 10s 后重试");
  });
});

describe("reservationNotes", () => {
  const at = Date.parse("2026-11-12T02:10:00Z"); // 10:10 Shanghai
  const dp = (o: { running?: boolean; ann?: Record<string, string>; pin?: string; persistence?: boolean }) =>
    ({
      metadata: { name: "a", annotations: o.ann },
      spec: { owner: "u", running: o.running ?? true, persistence: o.persistence ? { size: "10Gi" } : undefined, pod: o.pin ? { spec: { nodeSelector: { "kubernetes.io/hostname": o.pin } } } : undefined },
    }) as DevPod;
  const rsv = (user: string, start: string, end: string, phase: Reservation["phase"] = "Active"): Reservation =>
    ({ id: user + start, segments: [], node: "g1", user, start, end, phase, mine: user === "u" }) as Reservation;
  const rs = (...items: Reservation[]) => ({ items, noticeMs: 30 * 60_000, now: at });
  const labels = (d: DevPod, where: { node?: string }, r?: ReturnType<typeof rs>) => reservationNotes(d, where, r).map((n) => n.label);

  it("warns a DevPod on a node someone reserves soon, with the loss when it keeps nothing", () => {
    const soon = rs(rsv("alice", "2026-11-12T02:30:00Z", "2026-11-12T04:00:00Z", "Upcoming"));
    const n = reservationNotes(dp({}), { node: "g1" }, soon);
    expect(n.map((x) => x.label)).toEqual(["即将清场"]);
    expect(n[0].message).toBe("g1 将于 10:30 被 alice 预约，届时自动休眠；休眠后容器内数据丢失");
    expect(labels(dp({ persistence: true }), { node: "g2" }, soon)).toEqual([]); // another node
    expect(labels(dp({}), { node: "g1" }, rs(rsv("alice", "2026-11-12T03:00:00Z", "2026-11-12T04:00:00Z", "Upcoming")))).toEqual([]); // not yet
  });
  it("says a stopped DevPod pinned to a held node cannot start there, and the holder's hold is running out", () => {
    expect(labels(dp({ running: false, pin: "g1" }), {}, rs(rsv("alice", "2026-11-12T02:00:00Z", "2026-11-12T04:00:00Z")))).toEqual(["节点被预约"]);
    expect(labels(dp({}), { node: "g1" }, rs(rsv("u", "2026-11-12T02:00:00Z", "2026-11-12T02:30:00Z")))).toEqual(["预约将到期"]);
    expect(labels(dp({}), { node: "g1" }, rs(rsv("u", "2026-11-12T02:00:00Z", "2026-11-12T04:00:00Z")))).toEqual([]);
    expect(labels(dp({ running: false, pin: "g1" }), {}, rs(rsv("alice", "2026-11-12T02:00:00Z", "2026-11-12T04:00:00Z", "Cancelled")))).toEqual([]);
  });
  it("says why one was hibernated, only while it is stopped", () => {
    expect(labels(dp({ running: false, ann: { "devpod.io/hibernated-by": "reservation/r1" } }), {})).toEqual(["被预约清场休眠"]);
    expect(labels(dp({ running: false, ann: { "devpod.io/hibernated-by": "reservation-end/r1" } }), {})).toEqual(["预约结束休眠"]);
    expect(labels(dp({ running: true, ann: { "devpod.io/hibernated-by": "reservation/r1" } }), {})).toEqual([]);
  });
});
