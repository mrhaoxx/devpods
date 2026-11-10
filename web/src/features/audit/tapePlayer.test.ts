import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { TapePlayer, type Sink } from "./tapePlayer";

function rig(initial: "live" | "paused") {
  const calls: string[] = [];
  const sink: Sink = {
    reset: (s) => calls.push(`reset ${s.cols}x${s.rows}`),
    output: (d) => calls.push(`o ${d}`),
    resize: (s) => calls.push(`r ${s.cols}x${s.rows}`),
  };
  const p = new TapePlayer({ initial, idleLimitMs: 2000 });
  p.attach(sink);
  const take = () => calls.splice(0);
  return { p, take };
}
const header = (now?: number) => ({ cols: 80, rows: 24, start: 0, now });

describe("TapePlayer", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(0);
  });
  afterEach(() => vi.useRealTimers());

  it("live: draws the history once at the hand-over, then follows", () => {
    const { p, take } = rig("live");
    p.header(header(10_000));
    p.push({ t: 1000, o: "a" });
    p.push({ t: 2000, o: "b" });
    expect(take()).toEqual([]); // history is not drawn piece by piece
    p.live();
    expect(take()).toEqual(["reset 80x24", "o ab"]);
    p.push({ t: 11_000, o: "c" });
    p.push({ t: 11_500, size: { cols: 100, rows: 30 } });
    expect(take()).toEqual(["o c", "r 100x30"]);
    expect(p.getView().mode).toBe("live");
    vi.advanceTimersByTime(3000);
    expect(p.position()).toBe(13_000); // the live edge follows the channel clock
    expect(p.timelineEnd()).toBe(13_000);
  });

  it("seeking back replays from that moment and becomes live again on catching up", () => {
    const { p, take } = rig("live");
    p.header(header(10_000));
    p.push({ t: 1000, o: "a" });
    p.push({ t: 2000, o: "b" });
    p.push({ t: 9000, o: "c" });
    p.live();
    take();
    p.seek(1500);
    expect(take()).toEqual(["reset 80x24", "o a"]);
    expect(p.getView().mode).toBe("playing");
    vi.advanceTimersByTime(600);
    expect(take()).toEqual(["o b"]);
    p.push({ t: 10_700, o: "d" }); // live output keeps arriving, not drawn yet
    expect(take()).toEqual([]);
    vi.advanceTimersByTime(2100); // the 7 s idle gap is cut to 2 s
    expect(take()).toEqual(["o c"]);
    vi.advanceTimersByTime(1800);
    expect(take()).toEqual(["o d"]);
    expect(p.getView().mode).toBe("live");
    p.push({ t: 12_000, o: "e" });
    expect(take()).toEqual(["o e"]);
  });

  it("pause holds the frame while live output keeps arriving; 回到直播 jumps to the edge", () => {
    const { p, take } = rig("live");
    p.header(header(5000));
    p.push({ t: 1000, o: "a" });
    p.live();
    take();
    p.pause();
    p.push({ t: 6000, o: "b" });
    expect(take()).toEqual([]);
    expect(p.getView().mode).toBe("paused");
    p.goLive();
    expect(take()).toEqual(["reset 80x24", "o ab"]);
    expect(p.getView().mode).toBe("live");
  });

  it("a recording starts paused, plays to its end, and replays from the start", () => {
    const { p, take } = rig("paused");
    p.header(header());
    p.push({ t: 500, o: "a" });
    p.push({ t: 1000, o: "b" });
    p.done();
    expect(p.getView()).toMatchObject({ mode: "paused", phase: "ended" });
    expect(p.timelineEnd()).toBe(1000);
    p.play();
    vi.advanceTimersByTime(600);
    expect(take().filter((c) => c.startsWith("o"))).toEqual(["o a"]);
    vi.advanceTimersByTime(600);
    expect(take()).toEqual(["o b"]);
    expect(p.getView().mode).toBe("paused");
    expect(p.position()).toBe(1000);
    p.play();
    expect(take()).toEqual(["reset 80x24"]);
    expect(p.getView().mode).toBe("playing");
  });

  it("speed scales playback", () => {
    const { p, take } = rig("paused");
    p.header(header());
    p.push({ t: 1000, o: "a" });
    p.done();
    take();
    p.setSpeed(4);
    p.play();
    vi.advanceTimersByTime(300);
    expect(take()).toEqual(["o a"]);
  });

  it("a session ending while live stops at the end", () => {
    const { p } = rig("live");
    p.header(header(1000));
    p.push({ t: 500, o: "a" });
    p.live();
    p.done();
    expect(p.getView()).toMatchObject({ mode: "paused", phase: "ended" });
    expect(p.position()).toBe(500);
  });

  it("a reconnect keeps the viewer where they were", () => {
    const { p, take } = rig("live");
    p.header(header(5000));
    p.push({ t: 1000, o: "a" });
    p.push({ t: 2000, o: "b" });
    p.live();
    p.seek(1500);
    p.pause();
    take();
    p.offline("overflow");
    expect(p.getView().phase).toBe("offline");
    p.header(header(6000));
    p.push({ t: 1000, o: "a" });
    p.push({ t: 2000, o: "b" });
    expect(take()).toEqual([]);
    p.live();
    expect(take()).toEqual(["reset 80x24", "o a"]);
    expect(p.getView()).toMatchObject({ mode: "paused", phase: "live" });
  });
});
