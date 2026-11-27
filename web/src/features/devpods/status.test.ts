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
  it("in the queue's line: its place and why it waits", () => {
    const q = (state: object) => ({ ...dp({ running: false }, { phase: "Stopped" }), metadata: { name: "alice-x", annotations: { "devpod.io/queued-at": "2026-11-10T11:00:00Z", "devpod.io/queue-state": JSON.stringify(state) } } });
    expect(phaseSummary(q({ position: 3, reason: "capacity" }), undefined, now)).toBe("排队中 #3 · 等空闲资源");
    expect(phaseSummary(q({ position: 2, reason: "behind", detail: { head: "abc12" } }), undefined, now)).toBe("排队中 #2 · 排在 abc12 后");
    expect(phaseSummary(q({}), undefined, now)).toBe("排队中");
    expect(phaseSummary(q({ position: 4, reason: "never" }), undefined, now)).toBe("排队中 #4 · 没有机器放得下（看看要的资源，或绑定的机器）");
  });
  it("in the queue's line with a promise: when it is expected to start", () => {
    const q = { ...dp({ running: false }, { phase: "Stopped" }), metadata: { name: "alice-x", annotations: { "devpod.io/queued-at": "2026-11-10T11:00:00Z", "devpod.io/queue-state": JSON.stringify({ position: 3, reason: "capacity", expected: "2026-11-10T13:30:00Z" }) } } };
    expect(phaseSummary(q, undefined, now)).toBe("排队中 #3 · 等空闲资源 · 预计 今天 21:30 前后");
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
    ({ id: user + start, node: "g1", user, start, end, phase, mine: user === "u" }) as Reservation;
  const rs = (...items: Reservation[]) => ({ items, noticeMs: 30 * 60_000, now: at });
  const labels = (d: DevPod, where: { node?: string }, r?: ReturnType<typeof rs>) => reservationNotes(d, where, r).map((n) => n.label);

  it("tells the owner of a DevPod in a starting reservation's way when it goes; not once it is stopped", () => {
    const ann = { "devpod.io/clear-at": "2026-11-12T02:40:00Z", "devpod.io/clear-for": "r1" };
    const n = reservationNotes(dp({ ann, pin: "g1" }), { node: "g1" });
    expect(n.map((x) => x.label)).toEqual(["将被清场"]);
    expect(n[0].message).toMatch(/g1 的预约 10:40 起需要这台机器上的资源，届时这个 DevPod 会被自动休眠；休眠后容器内数据丢失/);
    expect(labels(dp({ ann, running: false, pin: "g1" }), { node: "g1" })).not.toContain("将被清场");
  });

  it("warns the owner that their reservation of the machine is running out, with the loss when it keeps nothing", () => {
    const n = reservationNotes(dp({}), { node: "g1" }, rs(rsv("u", "2026-11-12T02:00:00Z", "2026-11-12T02:30:00Z")));
    expect(n.map((x) => x.label)).toEqual(["预约将到期"]);
    expect(n[0].message).toBe("你对 g1 的预约将于 10:30 结束；如果这个 DevPod 只能靠预约在这台机器上运行，届时会被自动休眠；休眠后容器内数据丢失");
    expect(labels(dp({}), { node: "g1" }, rs(rsv("u", "2026-11-12T02:00:00Z", "2026-11-12T04:00:00Z")))).toEqual([]); // not yet
    expect(labels(dp({ running: false, pin: "g1" }), {}, rs(rsv("u", "2026-11-12T02:00:00Z", "2026-11-12T02:30:00Z")))).toEqual([]); // not running
  });
  it("says nothing of other people's reservations: they do not keep anyone off", () => {
    expect(labels(dp({}), { node: "g1" }, rs(rsv("alice", "2026-11-12T02:30:00Z", "2026-11-12T04:00:00Z", "Upcoming")))).toEqual([]);
    expect(labels(dp({ running: false, pin: "g1" }), {}, rs(rsv("alice", "2026-11-12T02:00:00Z", "2026-11-12T04:00:00Z")))).toEqual([]);
  });
  it("says why one was hibernated, only while it is stopped", () => {
    expect(labels(dp({ running: false, ann: { "devpod.io/hibernated-by": "reservation/r1" } }), {})).toEqual(["被预约清场休眠"]);
    expect(labels(dp({ running: false, ann: { "devpod.io/hibernated-by": "reservation-end/r1" } }), {})).toEqual(["预约结束休眠"]);
    expect(labels(dp({ running: true, ann: { "devpod.io/hibernated-by": "reservation/r1" } }), {})).toEqual([]);
    expect(labels(dp({ running: false, ann: { "devpod.io/hibernated-by": "machine-closed" } }), {})).toEqual(["机器关闭预约"]);
  });
  it("says when a reservation will clear it, where the queue put it", () => {
    expect(labels(dp({ running: true, pin: "g1", ann: { "devpod.io/expect-clear": "2026-11-12T08:00:00Z" } }), {})).toEqual(["预计被清场"]);
    expect(labels(dp({ running: false, pin: "g1", ann: { "devpod.io/expect-clear": "2026-11-12T08:00:00Z" } }), {})).toEqual([]);
  });
});
