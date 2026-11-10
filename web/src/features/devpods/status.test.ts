import { describe, expect, it } from "vitest";
import { phaseSummary, reservationNotes } from "./status";
import type { DevPod } from "../../data/api";

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
  it("Stopped without persistence says 无持久化", () => {
    expect(phaseSummary(dp({ running: false }, { phase: "Stopped" }), undefined, now)).toBe("已休眠 · 无持久化");
  });
  it("Failed shows the reason and the retry countdown", () => {
    const s = phaseSummary(dp({}, { phase: "Failed", message: "ImagePullBackOff", retryCount: 2, lastFailureAt: "2026-11-10T11:59:50Z" }), undefined, now);
    expect(s).toBe("ImagePullBackOff · 10s 后重试");
  });
});

describe("reservationNotes", () => {
  const dp = (o: { running?: boolean; ann?: Record<string, string>; conditions?: { type: string; status: string; message?: string }[] }) =>
    ({ metadata: { name: "a", annotations: o.ann }, spec: { owner: "u", running: o.running ?? true }, status: { conditions: o.conditions } }) as DevPod;
  it("shows the controller's conditions, and says a NodeReserved DevPod starts by itself", () => {
    const notes = reservationNotes(dp({ conditions: [{ type: "NodeReserved", status: "True", message: "g206 被 alice 预约至 17:00" }, { type: "ReservationEnding", status: "False" }] }));
    expect(notes.map((n) => n.label)).toEqual(["节点被预约"]);
    expect(notes[0].message).toBe("g206 被 alice 预约至 17:00。节点空出来后会自动启动，不需要操作。");
  });
  it("被预约清场休眠 only while the DevPod is stopped", () => {
    const ann = { "devpod.io/hibernated-by": "reservation/r1" };
    expect(reservationNotes(dp({ running: false, ann })).map((n) => n.label)).toEqual(["被预约清场休眠"]);
    expect(reservationNotes(dp({ running: true, ann }))).toEqual([]);
  });
});
