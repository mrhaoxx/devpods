import { describe, expect, it } from "vitest";
import type { Job } from "../../data/api";
import { fmtHours, sizeText, stateText } from "./jobs";

const now = Date.parse("2026-11-27T04:00:00Z"); // 12:00 in Shanghai
const job = (o: Partial<Job>): Job =>
  ({ id: "abc12", owner: "alice", phase: "Queued", nodes: 2, gpus: 8, cpus: 96, timeLimitSeconds: 86400, priority: 0, mine: true, canManage: true, submitted: "2026-11-27T03:00:00Z", ...o }) as Job;

describe("jobs", () => {
  it("says a job's state in words", () => {
    expect(stateText(job({ phase: "Running", startedAt: "2026-11-27T01:00:00Z", timeLimitSeconds: 43200 }), now)).toBe("● 运行中 · 已 3h，最多再 9h");
    expect(stateText(job({ phase: "Starting" }), now)).toBe("● 启动中");
    expect(stateText(job({ reason: "capacity", detail: { need: "32", free: "18" }, expectedStart: "2026-11-27T18:10:00Z" }), now)).toBe("○ 等待 · 空闲卡不够（要 32，剩 18）· 预计 明天 02:10 前后");
    expect(stateText(job({ reason: "capacity", detail: { need: "4", free: "0" }, expectedStart: "2026-11-27T06:00:00Z" }), now)).toBe("○ 等待 · 空闲卡不够（要 4，剩 0）· 预计 今天 14:00 前后");
    // how many machines have room for a rank: free cards spread thin are said
    expect(stateText(job({ nodes: 4, gpus: 8, reason: "capacity", detail: { need: "32", free: "18", fit: "1" } }), now)).toBe("○ 等待 · 空闲卡不够（4 台 × 8 卡，只有 1 台放得下，共空 18 卡）");
    expect(stateText(job({ nodes: 2, gpus: 0, cpus: 64, reason: "capacity", detail: { needCPU: "128", freeCPU: "100", fit: "0" } }), now)).toBe("○ 等待 · 空闲核不够（2 台 × 64 核，只有 0 台放得下，共空 100 核）");
    expect(stateText(job({ gpus: 0, reason: "capacity", detail: { needCPU: "64", freeCPU: "16" } }), now)).toBe("○ 等待 · 空闲核不够（要 64，剩 16）");
    expect(stateText(job({ reason: "behind", detail: { head: "xyz34" } }), now)).toBe("○ 等待 · 排在 xyz34 后");
    expect(stateText(job({ reason: "quota" }), now)).toBe("○ 等待 · 超出配额");
    expect(stateText(job({ reason: "preempting" }), now)).toBe("○ 等待 · 正在让出资源");
    expect(stateText(job({ reason: "preempted" }), now)).toBe("○ 等待 · 被抢占，重新排队");
    expect(stateText(job({ reason: "reserved" }), now)).toBe("○ 等待 · 刚有预约占了位置，重新排队");
    expect(stateText(job({ phase: "Succeeded" }), now)).toBe("已完成");
    expect(stateText(job({ phase: "Failed", detail: { exit: "3" } }), now)).toBe("失败（退出码 3）");
    expect(stateText(job({ phase: "Failed", reason: "rank-stopped" }), now)).toBe("失败（有一台被停止）");
    expect(stateText(job({ phase: "TimedOut" }), now)).toBe("超时");
    expect(stateText(job({ phase: "Cancelled" }), now)).toBe("已取消");
  });
  it("a held job says it is held, not where it waits", () => {
    expect(stateText(job({ phase: "Queued", reason: "held", held: true }), now)).toBe("⏸ 已暂停 · 恢复后回到原来的位置");
  });

  it("says sizes and spans", () => {
    expect(sizeText(job({}))).toBe("2 台 × 8 卡");
    expect(sizeText(job({ gpus: 0, cpus: 16 }))).toBe("2 台 × 16 核");
    expect(fmtHours(3 * 3600e3)).toBe("3h");
    expect(fmtHours(90 * 60e3)).toBe("1h30m");
    expect(fmtHours(40 * 60e3)).toBe("40m");
  });
});
