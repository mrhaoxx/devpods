// 终端 (UI spec §5.12): a full-page terminal in its own tab. The webui
// bridges it to the SSH gateway as the logged-in user, so it is recorded
// like any SSH session. Binary frames carry terminal bytes; a text frame
// is a control message ({"type":"resize"}).
import { useCallback, useEffect, useRef, useState } from "react";
import { Link, Navigate, useParams } from "react-router-dom";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { ApiFailure, probeShell, shellSocketURL } from "../../data/api";
import { useMe } from "../../data/queries";
import { Button } from "../../design/Button";
import { termClipboardAction, termKeySequence } from "../../lib/termKeys";
import { termTheme } from "../../lib/termTheme";
import { useTheme } from "../../app/theme";

type State = { kind: "connecting" } | { kind: "open" } | { kind: "closed"; reason: string };

// the console's resolved theme (it follows the system unless the user
// picked one): the terminal takes the matching full color set
const currentTheme = () => termTheme(document.documentElement.dataset.theme === "dark" ? "dark" : "light");

const REASONS: [RegExp, string][] = [
  [/^shell exited with status (\d+)$/, "shell 已退出（状态码 $1）"],
  [/^shell exited$/, "shell 已退出"],
];
function reasonText(reason: string): string {
  for (const [re, zh] of REASONS) if (re.test(reason)) return reason.replace(re, zh);
  return reason || "连接已断开";
}

export function ShellPage() {
  const { name = "" } = useParams();
  const me = useMe();
  const host = useRef<HTMLDivElement>(null);
  const term = useRef<Terminal | null>(null);
  const sock = useRef<WebSocket | null>(null);
  const [state, setState] = useState<State>({ kind: "connecting" });
  const [attempt, setAttempt] = useState(0);
  useTheme(); // this page is its own tab: follow the system theme here too

  useEffect(() => {
    document.title = `${name} · 终端`;
  }, [name]);

  useEffect(() => {
    if (!host.current) return;
    // Option is Meta (ESC prefix), as in desktop terminals: Option+B / F /
    // D reach the shell instead of starting a dead-key composition that
    // leaves the input stuck
    const t = new Terminal({ macOptionIsMeta: true, cursorBlink: true, fontSize: 13, fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace', theme: currentTheme(), scrollback: 5000 });
    const fit = new FitAddon();
    t.loadAddon(fit);
    t.open(host.current);
    // Fit on the next frame, and only when the box really changed: the
    // terminal's own scrollbar keeps a fixed gutter (see .term-host in
    // index.css), so the columns do not jump when scrollback appears.
    let raf = 0;
    const refit = () => {
      cancelAnimationFrame(raf);
      raf = requestAnimationFrame(() => {
        try {
          fit.fit();
        } catch {
          /* not laid out yet */
        }
      });
    };
    fit.fit();
    void document.fonts?.ready.then(refit); // the cell width changes when the font arrives
    term.current = t;
    const enc = new TextEncoder();
    const send = (d: string) => {
      if (sock.current?.readyState === WebSocket.OPEN) sock.current.send(enc.encode(d));
    };
    const data = t.onData(send);
    const mac = /Mac|iPhone|iPad/.test(navigator.platform);
    // editing shortcuts (word / line movement and deletion) and
    // Ctrl+Shift+C / V: handled here, kept from xterm and the browser
    t.attachCustomKeyEventHandler((ev) => {
      const seq = termKeySequence(ev, mac);
      const clip = seq === null ? termClipboardAction(ev, mac) : null;
      if (seq === null && clip === null) return true;
      ev.preventDefault();
      if (ev.type === "keydown") {
        if (seq !== null) send(seq);
        else if (clip === "copy") void navigator.clipboard?.writeText(t.getSelection());
        else void navigator.clipboard?.readText().then((text) => t.paste(text));
      }
      return false;
    });
    const resize = t.onResize(({ cols, rows }) => {
      if (sock.current?.readyState === WebSocket.OPEN) sock.current.send(JSON.stringify({ type: "resize", cols, rows }));
    });
    const ro = new ResizeObserver(refit);
    ro.observe(host.current);
    const mo = new MutationObserver(() => (t.options.theme = currentTheme()));
    mo.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
    return () => {
      cancelAnimationFrame(raf);
      mo.disconnect();
      ro.disconnect();
      data.dispose();
      resize.dispose();
      t.dispose();
      term.current = null;
    };
  }, []);

  useEffect(() => {
    const t = term.current;
    if (!t || !name) return;
    setState({ kind: "connecting" });
    const ws = new WebSocket(shellSocketURL(name, t.cols, t.rows));
    ws.binaryType = "arraybuffer";
    sock.current = ws;
    let opened = false;
    ws.onopen = () => {
      opened = true;
      setState({ kind: "open" });
      // the size in the URL was read before the layout settled: say it again
      ws.send(JSON.stringify({ type: "resize", cols: t.cols, rows: t.rows }));
      t.focus();
    };
    ws.onmessage = (m) => {
      if (typeof m.data !== "string") t.write(new Uint8Array(m.data as ArrayBuffer)); // binary frames are terminal output
    };
    ws.onclose = (e) => {
      if (sock.current !== ws) return;
      sock.current = null;
      if (opened) return setState({ kind: "closed", reason: reasonText(e.reason) });
      // the handshake failed: ask the server why
      probeShell(name).then(
        () => setState({ kind: "closed", reason: "无法连接：SSH 网关拒绝了连接，或同时打开的终端已达上限" }),
        (err: unknown) => setState({ kind: "closed", reason: err instanceof ApiFailure ? err.body.message : "无法连接" }),
      );
    };
    return () => {
      sock.current = null;
      ws.close();
    };
  }, [name, attempt]);

  const reconnect = useCallback(() => {
    term.current?.reset();
    setAttempt((n) => n + 1);
  }, []);

  if (me.isError) return <Navigate to="/login" replace />;
  return (
    <div className="flex h-screen flex-col bg-surface">
      <header className="flex items-center gap-3 border-b border-line px-3 py-1.5 text-xs">
        <Link to={`/devpods/${name}`} className="mono font-semibold text-ink hover:text-accent">
          {name}
        </Link>
        <span className="text-muted">终端</span>
        <span role="status" className="ml-auto text-muted">
          {state.kind === "connecting" ? "连接中…" : state.kind === "open" ? "已连接" : state.reason}
        </span>
        {state.kind === "closed" && (
          <Button variant="primary" onClick={reconnect}>
            重新连接
          </Button>
        )}
      </header>
      {/* the padding lives on the wrapper: the fit measures the inner box,
          which has none and never overflows the page */}
      <div className="min-h-0 flex-1 overflow-hidden p-1" style={{ background: "var(--color-surface)" }}>
        <div ref={host} className="term-host h-full w-full overflow-hidden" />
      </div>
    </div>
  );
}
