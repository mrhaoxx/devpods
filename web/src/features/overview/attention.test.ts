import { describe, expect, it } from "vitest";
import { attentionAction } from "./attention";

const base = { since: "2026-11-10T00:00:00Z", devpod: "alice-x", owner: "alice", uid: "u1" };

describe("attentionAction", () => {
  it("maps each kind", () => {
    expect(attentionAction({ ...base, kind: "idle_gpu" })).toEqual({ label: "休眠", op: "hibernate" });
    expect(attentionAction({ ...base, kind: "pending_long" })).toEqual({ label: "事件", href: "/devpods/alice-x?tab=events" });
    expect(attentionAction({ ...base, kind: "alert" })).toEqual({ label: "回放", href: "/audit/alerts?devpod=alice-x" });
    expect(attentionAction({ ...base, kind: "quota_high" })).toEqual({ label: "调整", href: "/admin/users?user=alice" });
  });
  it("unknown kinds have no action", () => {
    expect(attentionAction({ ...base, kind: "reservation_clearing" })).toBeNull();
  });
});
