import { describe, expect, it, vi } from "vitest";
import { LiveHub } from "./LiveHub";
import { devpodStream, devpodsStream } from "./streams";
import type { SseHandlers } from "../sse";

function fakeOpen() {
  const conns: { url: string; h: SseHandlers; closed: boolean }[] = [];
  const open = (url: string, h: SseHandlers) => {
    const c = { url, h, closed: false };
    conns.push(c);
    return () => {
      c.closed = true;
    };
  };
  return { open, conns };
}

const frame = (items: { name: string; cpu: number }[]) => ({ ts: "", items: items.map((i) => ({ owner: "a", ...i })) });

describe("LiveHub", () => {
  it("opens one connection for two acquires and closes on the last release", () => {
    const { open, conns } = fakeOpen();
    const hub = new LiveHub({ open });
    const r1 = hub.acquire(devpodsStream("mine"));
    const r2 = hub.acquire(devpodsStream("mine"));
    expect(conns).toHaveLength(1);
    expect(conns[0].url).toBe("/api/live/devpods?scope=mine");
    r1();
    expect(conns[0].closed).toBe(false);
    r2();
    expect(conns[0].closed).toBe(true);
  });

  it("keeps only the last 60 s of history per key", () => {
    const { open, conns } = fakeOpen();
    let now = 0;
    const hub = new LiveHub({ open, now: () => now });
    hub.acquire(devpodsStream("mine"));
    conns[0].h.onMessage(frame([{ name: "a", cpu: 1 }]));
    now = 30_000;
    conns[0].h.onMessage(frame([{ name: "a", cpu: 2 }]));
    expect(hub.row("devpods:mine", "a")?.history).toHaveLength(2);
    now = 61_000;
    conns[0].h.onMessage(frame([{ name: "a", cpu: 3 }]));
    const h = hub.row<{ cpu: number }>("devpods:mine", "a")!.history;
    expect(h.map((x) => x.value.cpu)).toEqual([2, 3]);
  });

  it("notifies only listeners of the key that changed", () => {
    const { open, conns } = fakeOpen();
    const hub = new LiveHub({ open });
    hub.acquire(devpodsStream("mine"));
    const a = vi.fn();
    const b = vi.fn();
    hub.subscribeRow("devpods:mine", "a", a);
    hub.subscribeRow("devpods:mine", "b", b);
    conns[0].h.onMessage(frame([{ name: "a", cpu: 1 }]));
    expect(a).toHaveBeenCalledTimes(1);
    expect(b).not.toHaveBeenCalled();
    const before = hub.row("devpods:mine", "a");
    expect(hub.row("devpods:mine", "a")).toBe(before);
  });

  it("error keeps rows and sets reconnecting; next message sets live", () => {
    const { open, conns } = fakeOpen();
    const hub = new LiveHub({ open });
    hub.acquire(devpodsStream("mine"));
    conns[0].h.onMessage(frame([{ name: "a", cpu: 1 }]));
    expect(hub.status("devpods:mine")).toBe("live");
    conns[0].h.onError();
    expect(hub.status("devpods:mine")).toBe("reconnecting");
    expect(hub.anyReconnecting()).toBe(true);
    expect(hub.row("devpods:mine", "a")).toBeTruthy();
    conns[0].h.onMessage(frame([{ name: "a", cpu: 2 }]));
    expect(hub.status("devpods:mine")).toBe("live");
  });

  it("liveStatus connected:false sets reconnecting, connected:true restores live", () => {
    const { open, conns } = fakeOpen();
    const hub = new LiveHub({ open });
    const onStatus = vi.fn();
    hub.subscribeStatus(onStatus);
    hub.acquire(devpodsStream("mine"));
    conns[0].h.onOpen();
    conns[0].h.onNamed("liveStatus", { connected: false });
    expect(hub.status("devpods:mine")).toBe("reconnecting");
    conns[0].h.onMessage(frame([{ name: "a", cpu: 1 }]));
    expect(hub.status("devpods:mine")).toBe("reconnecting"); // the leader is still away
    conns[0].h.onNamed("liveStatus", { connected: true });
    expect(hub.status("devpods:mine")).toBe("live");
    expect(onStatus).toHaveBeenCalled();
  });

  it("devpod stream forwards devpod and event messages to onOther", () => {
    const { open, conns } = fakeOpen();
    const hub = new LiveHub({ open });
    const other = vi.fn();
    hub.acquire(devpodStream("alice-x", false, other));
    expect(conns[0].url).toBe("/api/devpods/alice-x/stream");
    conns[0].h.onMessage({ kind: "devpod", type: "MODIFIED", detail: {} });
    conns[0].h.onMessage({ kind: "event", type: "ADDED", event: {} });
    conns[0].h.onMessage({ kind: "metrics", metrics: { ts: "t" } });
    conns[0].h.onMessage({ kind: "liveStatus", connected: false });
    expect(other).toHaveBeenCalledTimes(2);
    expect(hub.row("devpod:alice-x", "metrics")?.value).toEqual({ ts: "t" });
    expect(hub.status("devpod:alice-x")).toBe("reconnecting");
    hub.acquire(devpodStream("alice-x", true));
    expect(conns[1].url).toBe("/api/devpods/alice-x/stream?procs=all");
  });
});
