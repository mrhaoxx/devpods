import { describe, expect, it } from "vitest";
import { castOffset, castPlayerOptions, mergeTimeline } from "./audit";
import { SessionDetail } from "../data/api";

const base = {
  session: {} as SessionDetail["session"],
  channels: [
    { channel_id: 1, type: "session", command: "", subsystem: "", target: "", pty: true, opened_at: "2026-11-09T10:00:00Z" },
  ],
  commands: [
    { event_id: "c1", ts: "2026-11-09T10:00:05Z", channel_id: 1, cmdline: "make", exit_code: 2 },
    { event_id: "c2", ts: "2026-11-09T10:00:01Z", channel_id: 1, cmdline: "ls", exit_code: 0 },
  ],
  files: [{ event_id: "f1", ts: "2026-11-09T10:00:05Z", channel_id: 2, protocol: "sftp", op: "upload", path: "/x", path2: "", blocked: true }],
  policy: [{ ts: "2026-11-09T10:00:03Z", rule: "download", action: "deny", detail: "blocked" }],
  alerts: null,
} as unknown as SessionDetail;

describe("mergeTimeline", () => {
  it("orders every kind by time", () => {
    const tl = mergeTimeline(base);
    expect(tl.map((i) => i.kind)).toEqual(["channel", "command", "policy", "command", "file"]);
    expect(tl[1].text).toContain("ls");
  });
  it("keeps the input order (channels, commands, files, policy, alerts) for equal timestamps", () => {
    const tl = mergeTimeline(base);
    // c1 and f1 share 10:00:05 — the command was listed before the file
    expect(tl.slice(-2).map((i) => i.kind)).toEqual(["command", "file"]);
  });
  it("tolerates null sections", () => {
    expect(mergeTimeline({ ...base, channels: null, commands: null, files: null, policy: null })).toEqual([]);
  });
});

describe("castOffset", () => {
  it("is the seconds since the channel opened", () => {
    expect(castOffset("2026-11-09T10:00:00Z", "2026-11-09T10:00:02.5Z")).toBe(2.5);
  });
  it("clamps to zero", () => {
    expect(castOffset("2026-11-09T10:00:10Z", "2026-11-09T10:00:00Z")).toBe(0);
  });
});

describe("castPlayerOptions", () => {
  it("keeps real time so timeline seeks (wall-clock castOffset) land on the event", () => {
    // asciinema-player's idleTimeLimit shifts every later event earlier,
    // and seek() uses that compressed clock.
    expect(castPlayerOptions).not.toHaveProperty("idleTimeLimit");
  });
});
