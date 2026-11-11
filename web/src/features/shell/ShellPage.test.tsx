import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, screen, waitFor } from "@testing-library/react";
import { Route, Routes } from "react-router-dom";
import { renderWithProviders, stubFetch } from "../../test/render";
import { shellSocketURL } from "../../data/api";
import { ShellPage } from "./ShellPage";

const term = vi.hoisted(() => ({ written: [] as string[], onData: (_: string) => {}, onResize: (_: { cols: number; rows: number }) => {}, onKey: (_: KeyboardEvent): boolean => true }));
vi.mock("@xterm/xterm", () => ({
  Terminal: class {
    cols = 100;
    rows = 30;
    options = {};
    loadAddon() {}
    open() {}
    focus() {}
    reset() {}
    dispose() {}
    write(d: Uint8Array) {
      term.written.push(new TextDecoder().decode(d));
    }
    onData(fn: (d: string) => void) {
      term.onData = fn;
      return { dispose() {} };
    }
    attachCustomKeyEventHandler(fn: (e: KeyboardEvent) => boolean) {
      term.onKey = fn;
    }
    getSelection() {
      return "";
    }
    paste() {}
    onResize(fn: (s: { cols: number; rows: number }) => void) {
      term.onResize = fn;
      return { dispose() {} };
    }
  },
}));
vi.mock("@xterm/addon-fit", () => ({ FitAddon: class { fit() {} } }));

class FakeSocket {
  static OPEN = 1;
  static all: FakeSocket[] = [];
  readyState = 0;
  binaryType = "";
  sent: (string | Uint8Array)[] = [];
  onopen?: () => void;
  onmessage?: (m: { data: unknown }) => void;
  onclose?: (e: { reason: string }) => void;
  constructor(public url: string) {
    FakeSocket.all.push(this);
  }
  send(d: string | Uint8Array) {
    this.sent.push(d);
  }
  close() {}
  open() {
    this.readyState = 1;
    this.onopen?.();
  }
}

const render = () =>
  renderWithProviders(
    <Routes>
      <Route path="/devpods/:name/shell" element={<ShellPage />} />
    </Routes>,
    { route: "/devpods/alice-a/shell" },
  );

describe("ShellPage", () => {
  beforeEach(() => {
    FakeSocket.all = [];
    term.written = [];
    vi.stubGlobal("WebSocket", FakeSocket);
    vi.stubGlobal("ResizeObserver", class { observe() {} disconnect() {} });
  });
  afterEach(() => vi.unstubAllGlobals());

  it("builds the socket URL from the page's scheme", () => {
    expect(shellSocketURL("a", 80, 24, { protocol: "https:", host: "ui.example" })).toBe("wss://ui.example/api/devpods/a/shell?cols=80&rows=24");
    expect(shellSocketURL("a", 80, 24, { protocol: "http:", host: "localhost:5173" })).toBe("ws://localhost:5173/api/devpods/a/shell?cols=80&rows=24");
  });

  it("bridges the terminal: output in, keys out as binary, resize as a control message", async () => {
    stubFetch({});
    render();
    await waitFor(() => expect(FakeSocket.all.length).toBe(1));
    const ws = FakeSocket.all[0];
    expect(ws.url).toContain("/api/devpods/alice-a/shell?cols=100&rows=30");
    expect(ws.binaryType).toBe("arraybuffer");
    act(() => ws.open());
    expect(screen.getByRole("status").textContent).toBe("已连接");
    act(() => ws.onmessage?.({ data: new TextEncoder().encode("$ ").buffer }));
    expect(term.written).toEqual(["$ "]);
    term.onData("ls\r");
    // on open the page states its size again (the URL's was read before layout settled)
    expect(ws.sent[0]).toBe('{"type":"resize","cols":100,"rows":30}');
    expect(new TextDecoder().decode(ws.sent[1] as Uint8Array)).toBe("ls\r");
    term.onResize({ cols: 120, rows: 40 });
    expect(ws.sent[2]).toBe('{"type":"resize","cols":120,"rows":40}');
    // Option+→ goes out as "forward one word" and never reaches xterm or the browser
    const ev = new KeyboardEvent("keydown", { key: "ArrowRight", altKey: true, cancelable: true });
    expect(term.onKey(ev)).toBe(false);
    expect(ev.defaultPrevented).toBe(true);
    expect(new TextDecoder().decode(ws.sent[3] as Uint8Array)).toBe("\x1bf");
    expect(term.onKey(new KeyboardEvent("keyup", { key: "ArrowRight", altKey: true }))).toBe(false);
    expect(ws.sent.length).toBe(4); // sent once, on keydown
    expect(term.onKey(new KeyboardEvent("keydown", { key: "a" }))).toBe(true);
  });

  it("shows why the shell ended and reconnects on demand", async () => {
    stubFetch({});
    render();
    await waitFor(() => expect(FakeSocket.all.length).toBe(1));
    act(() => FakeSocket.all[0].open());
    act(() => FakeSocket.all[0].onclose?.({ reason: "shell exited with status 3" }));
    expect(screen.getByRole("status").textContent).toBe("shell 已退出（状态码 3）");
    fireEvent.click(screen.getByRole("button", { name: "重新连接" }));
    await waitFor(() => expect(FakeSocket.all.length).toBe(2));
  });

  it("a failed handshake shows the server's reason", async () => {
    stubFetch({ "/api/devpods/alice-a/shell": new Response(JSON.stringify({ code: "NOT_RUNNING", message: "the DevPod is not running" }), { status: 409 }) });
    render();
    await waitFor(() => expect(FakeSocket.all.length).toBe(1));
    act(() => FakeSocket.all[0].onclose?.({ reason: "" }));
    await waitFor(() => expect(screen.getByRole("status").textContent).toBe("the DevPod is not running"));
  });
});
