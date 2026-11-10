// A channel's terminal events on the channel clock (ms since it opened),
// read from the recording (/cast) or the live stream (/live): both are
// asciicast v2 lines, the live one with {"live"} / {"done"} / {"offline"}
// markers (audit spec §16).
export type Size = { cols: number; rows: number };
export type TapeEvent = { t: number; o?: string; size?: Size };
export type StreamLine =
  | { kind: "header"; cols: number; rows: number; start: number; now: number | undefined }
  | { kind: "event"; ev: TapeEvent }
  | { kind: "live" }
  | { kind: "done" }
  | { kind: "offline"; why: string }
  | { kind: "skip" };

const skip: StreamLine = { kind: "skip" };
const num = (v: unknown, d: number) => (typeof v === "number" && Number.isFinite(v) ? v : d);

export function parseLine(line: string): StreamLine {
  let v: unknown;
  try {
    v = JSON.parse(line);
  } catch {
    return skip;
  }
  if (Array.isArray(v)) {
    const [t, code, data] = v as unknown[];
    if (typeof t !== "number" || typeof data !== "string") return skip;
    const ms = Math.round(t * 1000);
    if (code === "o") return { kind: "event", ev: { t: ms, o: data } };
    if (code === "r") {
      const m = /^(\d+)x(\d+)$/.exec(data);
      if (m) return { kind: "event", ev: { t: ms, size: { cols: +m[1], rows: +m[2] } } };
    }
    return skip;
  }
  if (v && typeof v === "object") {
    const o = v as Record<string, unknown>;
    if (o.version === 2)
      return {
        kind: "header",
        cols: num(o.width, 80),
        rows: num(o.height, 24),
        start: Math.round(num(o.start, 0) * 1000),
        now: typeof o.now === "number" ? Math.round(o.now * 1000) : undefined,
      };
    if (o.live === true) return { kind: "live" };
    if (o.done === true) return { kind: "done" };
    if (typeof o.offline === "string") return { kind: "offline", why: o.offline };
  }
  return skip;
}

export type TapeOpts = { maxBytes?: number; keepBytes?: number; feedBytes?: number };
export type Frame = { size: Size; ops: (string | Size)[]; next: number };

/** Tape holds the events, bounded: past maxBytes of output the oldest
 *  are dropped (down to keepBytes) and start moves forward. */
export class Tape {
  readonly events: TapeEvent[] = [];
  bytes = 0;
  private readonly maxBytes: number;
  private readonly keepBytes: number;
  private readonly feedBytes: number;
  constructor(
    private size0: Size,
    public start = 0,
    o: TapeOpts = {},
  ) {
    this.maxBytes = o.maxBytes ?? 96 << 20;
    this.keepBytes = o.keepBytes ?? 64 << 20;
    this.feedBytes = o.feedBytes ?? 8 << 20;
  }

  /** push appends an event; returns how many old events were dropped. */
  push(ev: TapeEvent): number {
    this.events.push(ev);
    if (ev.o) this.bytes += ev.o.length;
    if (this.bytes <= this.maxBytes) return 0;
    let i = 0;
    while (i < this.events.length - 1 && this.bytes > this.keepBytes) {
      const e = this.events[i++];
      if (e.o) this.bytes -= e.o.length;
      if (e.size) this.size0 = e.size;
    }
    this.events.splice(0, i);
    this.start = this.events[0].t;
    return i;
  }

  get end(): number {
    return this.events.length ? this.events[this.events.length - 1].t : this.start;
  }

  /** indexAfter: the first event later than t. */
  indexAfter(t: number): number {
    let lo = 0;
    let hi = this.events.length;
    while (lo < hi) {
      const mid = (lo + hi) >> 1;
      if (this.events[mid].t <= t) lo = mid + 1;
      else hi = mid;
    }
    return lo;
  }

  /** frameAt: how to draw the screen as of t on a cleared terminal —
   *  the last feedBytes of output before t, resizes in order. */
  frameAt(t: number): Frame {
    const end = this.indexAfter(t);
    let j = end;
    let sum = 0;
    while (j > 0) {
      const o = this.events[j - 1].o;
      if (o) {
        if (sum > 0 && sum + o.length > this.feedBytes) break;
        sum += o.length;
      }
      j--;
    }
    let size = this.size0;
    for (let k = j - 1; k >= 0; k--) {
      const s = this.events[k].size;
      if (s) {
        size = s;
        break;
      }
    }
    const ops: (string | Size)[] = [];
    let buf = "";
    for (let k = j; k < end; k++) {
      const e = this.events[k];
      if (e.size) {
        if (buf) {
          ops.push(buf);
          buf = "";
        }
        if (ops.length === 0) size = e.size;
        else ops.push(e.size);
      } else if (e.o) buf += e.o;
    }
    if (buf) ops.push(buf);
    return { size, ops, next: end };
  }
}
