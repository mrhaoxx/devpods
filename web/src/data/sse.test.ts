import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { makeOpenSSE } from "./sse";

class FakeES {
  static all: FakeES[] = [];
  onopen: (() => void) | null = null;
  onmessage: ((m: { data: string }) => void) | null = null;
  onerror: (() => void) | null = null;
  listeners: Record<string, (m: { data: string }) => void> = {};
  closed = false;
  constructor(public url: string) {
    FakeES.all.push(this);
  }
  addEventListener(ev: string, fn: (m: { data: string }) => void) {
    this.listeners[ev] = fn;
  }
  close() {
    this.closed = true;
  }
}

const noop = { onMessage() {}, onNamed() {}, onOpen() {}, onError() {} };

describe("openSSE", () => {
  beforeEach(() => {
    FakeES.all = [];
    vi.useFakeTimers();
  });
  afterEach(() => vi.useRealTimers());

  it("reconnects with backoff 1s, 2s, 4s after errors and resets after open", () => {
    const open = makeOpenSSE(FakeES as unknown as typeof EventSource);
    open("/s", noop);
    expect(FakeES.all).toHaveLength(1);
    FakeES.all[0].onerror!();
    vi.advanceTimersByTime(999);
    expect(FakeES.all).toHaveLength(1);
    vi.advanceTimersByTime(1);
    expect(FakeES.all).toHaveLength(2);
    FakeES.all[1].onerror!();
    vi.advanceTimersByTime(1999);
    expect(FakeES.all).toHaveLength(2);
    vi.advanceTimersByTime(1);
    expect(FakeES.all).toHaveLength(3);
    FakeES.all[2].onerror!();
    vi.advanceTimersByTime(4000);
    expect(FakeES.all).toHaveLength(4);
    FakeES.all[3].onopen!();
    FakeES.all[3].onerror!();
    vi.advanceTimersByTime(1000);
    expect(FakeES.all).toHaveLength(5);
  });

  it("dispatches named liveStatus events to onNamed and data to onMessage", () => {
    const onNamed = vi.fn();
    const onMessage = vi.fn();
    makeOpenSSE(FakeES as unknown as typeof EventSource)("/s", { ...noop, onNamed, onMessage });
    FakeES.all[0].listeners.liveStatus({ data: '{"connected":false}' });
    FakeES.all[0].onmessage!({ data: '{"a":1}' });
    FakeES.all[0].onmessage!({ data: "not json" });
    expect(onNamed).toHaveBeenCalledWith("liveStatus", { connected: false });
    expect(onMessage).toHaveBeenCalledTimes(1);
    expect(onMessage).toHaveBeenCalledWith({ a: 1 });
  });

  it("close() stops reconnecting", () => {
    const close = makeOpenSSE(FakeES as unknown as typeof EventSource)("/s", noop);
    FakeES.all[0].onerror!();
    close();
    vi.advanceTimersByTime(60_000);
    expect(FakeES.all).toHaveLength(1);
    expect(FakeES.all[0].closed).toBe(true);
  });
});
