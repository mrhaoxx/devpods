import { afterEach, describe, expect, it, vi } from "vitest";
import { streamTape } from "./tapeStream";

const body = (...chunks: string[]) =>
  new Response(
    new ReadableStream({
      start(c) {
        for (const ch of chunks) c.enqueue(new TextEncoder().encode(ch));
        c.close();
      },
    }),
  );

function target() {
  const calls: string[] = [];
  return {
    calls,
    header: (h: { cols: number }) => calls.push(`header ${h.cols}`),
    push: (e: { t: number; o?: string }) => calls.push(`push ${e.t} ${e.o ?? "size"}`),
    live: () => calls.push("live"),
    done: () => calls.push("done"),
    offline: (w: string) => calls.push(`offline ${w}`),
  };
}

describe("streamTape", () => {
  afterEach(() => vi.useRealTimers());

  it("feeds lines split across chunks and ends on done", async () => {
    const t = target();
    const fetchFn = vi.fn(async (_url: string) => body('{"version":2,"width":100,"height":30}\n[1,"o","a', 'b"]\n{"live":true}\n[2,"r","90x20"]\n{"done":true}\n'));
    streamTape({ live: "/live", cast: "/cast" }, t, fetchFn as unknown as typeof fetch);
    await vi.waitFor(() => expect(t.calls).toEqual(["header 100", "push 1000 ab", "live", "push 2000 size", "done"]));
    expect(fetchFn.mock.calls[0][0]).toBe("/live");
  });

  it("an ended session falls back to the recording", async () => {
    const t = target();
    const fetchFn = vi.fn(async (url: string) => (url === "/live" ? new Response("{}", { status: 409 }) : body('{"version":2,"width":80,"height":24}\n[1,"o","x"]\n')));
    streamTape({ live: "/live", cast: "/cast" }, t, fetchFn as unknown as typeof fetch);
    await vi.waitFor(() => expect(t.calls).toEqual(["header 80", "push 1000 x", "done"]));
  });

  it("a dropped live stream reconnects", async () => {
    vi.useFakeTimers();
    const t = target();
    let n = 0;
    const fetchFn = vi.fn(async () => (n++ === 0 ? body('{"version":2,"width":80,"height":24}\n{"offline":"overflow"}\n') : body('{"version":2,"width":80,"height":24}\n{"done":true}\n')));
    const stop = streamTape({ live: "/live", cast: "/cast" }, t, fetchFn as unknown as typeof fetch);
    await vi.waitFor(() => expect(t.calls).toEqual(["header 80", "offline 输出过快，正在重新载入"]));
    await vi.advanceTimersByTimeAsync(1100);
    await vi.waitFor(() => expect(t.calls.slice(2)).toEqual(["header 80", "done"]));
    stop();
  });
});
