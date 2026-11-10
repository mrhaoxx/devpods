// LiveHub shares one SSE connection per live stream across every
// component that needs it (reference-counted), keeps a 60 s ring of
// samples per row key, and lets a table cell subscribe to just its own
// row, so a per-second update re-renders only the rows that changed
// (UI spec §7.2). Live data never enters the Query cache.
import type { OpenSSE } from "../sse";

export type LiveStatus = "connecting" | "live" | "reconnecting" | "off";

export interface StreamSpec {
  id: string;
  url: string;
  /** rows splits one message into keyed row values. */
  rows(msg: unknown): Array<[key: string, value: unknown]>;
  /** onOther receives messages that carry no rows (DevPod and event
   *  messages on the detail stream). */
  onOther?(msg: unknown): void;
}

export type LiveRow<T> = { value: T; ts: number; history: ReadonlyArray<{ ts: number; value: T }> };

type Stream = {
  spec: StreamSpec;
  refs: number;
  close: () => void;
  status: LiveStatus;
  leaderDown: boolean;
  rows: Map<string, LiveRow<unknown>>;
  listeners: Map<string, Set<() => void>>;
};

const isLiveStatus = (m: unknown): m is { kind: "liveStatus"; connected: boolean } =>
  !!m && typeof m === "object" && (m as { kind?: unknown }).kind === "liveStatus" && typeof (m as { connected?: unknown }).connected === "boolean";

export class LiveHub {
  private streams = new Map<string, Stream>();
  private statusListeners = new Set<() => void>();
  private open: OpenSSE;
  private now: () => number;
  private windowMs: number;

  constructor(d: { open: OpenSSE; now?: () => number; windowMs?: number }) {
    this.open = d.open;
    this.now = d.now ?? Date.now;
    this.windowMs = d.windowMs ?? 60_000;
  }

  acquire(spec: StreamSpec): () => void {
    let s = this.streams.get(spec.id);
    if (s) {
      s.refs++;
    } else {
      const st: Stream = { spec, refs: 1, close: () => {}, status: "connecting", leaderDown: false, rows: new Map(), listeners: new Map() };
      this.streams.set(spec.id, st);
      s = st;
      st.close = this.open(spec.url, {
        onOpen: () => this.setStatus(st, st.leaderDown ? "reconnecting" : "live"),
        onError: () => this.setStatus(st, "reconnecting"),
        onNamed: (ev, d) => {
          if (ev === "liveStatus") this.leader(st, !!(d as { connected?: boolean }).connected);
        },
        onMessage: (m) => this.message(st, m),
      });
    }
    let released = false;
    return () => {
      if (released) return;
      released = true;
      const cur = this.streams.get(spec.id);
      if (!cur || --cur.refs > 0) return;
      cur.close();
      this.streams.delete(spec.id);
      this.emitStatus();
    };
  }

  row<T>(streamId: string, key: string): LiveRow<T> | undefined {
    return this.streams.get(streamId)?.rows.get(key) as LiveRow<T> | undefined;
  }

  subscribeRow(streamId: string, key: string, fn: () => void): () => void {
    const s = this.streams.get(streamId);
    if (!s) return () => {};
    let set = s.listeners.get(key);
    if (!set) s.listeners.set(key, (set = new Set()));
    set.add(fn);
    return () => set!.delete(fn);
  }

  status(streamId: string): LiveStatus {
    return this.streams.get(streamId)?.status ?? "connecting";
  }

  subscribeStatus(fn: () => void): () => void {
    this.statusListeners.add(fn);
    return () => this.statusListeners.delete(fn);
  }

  anyReconnecting(): boolean {
    for (const s of this.streams.values()) if (s.status === "reconnecting") return true;
    return false;
  }

  private message(s: Stream, m: unknown) {
    if (isLiveStatus(m)) {
      this.leader(s, m.connected);
      return;
    }
    const rows = s.spec.rows(m);
    if (rows.length === 0) {
      s.spec.onOther?.(m);
      return;
    }
    const now = this.now();
    const cutoff = now - this.windowMs;
    for (const [key, value] of rows) {
      const prev = s.rows.get(key);
      const history = (prev?.history ?? []).filter((h) => h.ts > cutoff);
      history.push({ ts: now, value });
      s.rows.set(key, { value, ts: now, history });
      s.listeners.get(key)?.forEach((fn) => fn());
    }
    if (!s.leaderDown) this.setStatus(s, "live");
  }

  private leader(s: Stream, connected: boolean) {
    s.leaderDown = !connected;
    this.setStatus(s, connected ? "live" : "reconnecting");
  }

  private setStatus(s: Stream, st: LiveStatus) {
    if (s.status === st) return;
    s.status = st;
    this.emitStatus();
  }

  private emitStatus() {
    this.statusListeners.forEach((fn) => fn());
  }
}
