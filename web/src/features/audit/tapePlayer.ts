// TapePlayer: one player for live and replay (audit spec §16). It keeps
// the channel's events (Tape) and a playhead on the channel clock:
//   live    — at the edge, new output is drawn as it arrives
//   playing — behind the edge, a clock replays events; on catching up
//             with an open session it becomes live
//   paused  — the frame holds; events keep arriving
import { Tape, type Size, type TapeEvent, type TapeOpts } from "./tape";

export type Sink = { reset(size: Size): void; output(data: string): void; resize(size: Size): void };
export type Mode = "live" | "playing" | "paused";
/** connecting → history (loading) → live (open session) | ended; offline while reconnecting. */
export type Phase = "connecting" | "history" | "live" | "ended" | "offline";
export type PlayerView = { mode: Mode; phase: Phase; speed: number; skipIdle: boolean; start: number; reason?: string };
export type StreamHeader = { cols: number; rows: number; start: number; now: number | undefined };

export class TapePlayer {
  private tape = new Tape({ cols: 80, rows: 24 });
  private sink?: Sink;
  private mode: Mode;
  private phase: Phase = "connecting";
  private pos = 0; // paused position, or where the playing clock started
  private baseWall = 0;
  private next = 0; // the next event to draw
  private drawn = false;
  private speed = 1;
  private skipIdle = true;
  private reason?: string;
  private timer?: ReturnType<typeof setTimeout>;
  private clock?: { chan: number; wall: number }; // the server's channel clock
  private reloading = false;
  private listeners = new Set<() => void>();
  private view: PlayerView;
  private readonly idleLimit: number;
  private readonly tapeOpts?: TapeOpts;

  constructor(o: { initial: "live" | "paused"; idleLimitMs?: number; tape?: TapeOpts }) {
    this.mode = o.initial;
    this.idleLimit = o.idleLimitMs ?? 2000;
    this.tapeOpts = o.tape;
    this.view = this.snapshot();
  }

  /* ---- observers ---- */
  subscribe = (fn: () => void) => {
    this.listeners.add(fn);
    return () => void this.listeners.delete(fn);
  };
  getView = () => this.view;
  private snapshot(): PlayerView {
    return { mode: this.mode, phase: this.phase, speed: this.speed, skipIdle: this.skipIdle, start: this.tape.start, reason: this.reason };
  }
  private notify() {
    this.view = this.snapshot();
    this.listeners.forEach((fn) => fn());
  }

  private liveNow(): number {
    const c = this.clock;
    return Math.max(this.tape.end, c ? c.chan + (Date.now() - c.wall) : 0);
  }
  /** timelineEnd: now for an open session, else the last event. */
  timelineEnd(): number {
    return this.phase === "ended" || !this.clock ? this.tape.end : this.liveNow();
  }
  position(): number {
    if (this.mode === "live") return this.liveNow();
    if (this.mode === "playing") return Math.min(this.pos + (Date.now() - this.baseWall) * this.speed, this.timelineEnd());
    return this.pos;
  }

  /* ---- drawing ---- */
  attach(sink: Sink) {
    this.sink = sink;
    if (this.drawn) this.draw(this.mode === "live" ? this.tape.end : this.position());
  }
  private draw(t: number) {
    const f = this.tape.frameAt(t);
    this.next = f.next;
    this.drawn = true;
    if (!this.sink) return;
    this.sink.reset(f.size);
    for (const op of f.ops) typeof op === "string" ? this.sink.output(op) : this.sink.resize(op);
  }
  private feedUntil(t: number) {
    const end = this.tape.indexAfter(t);
    let buf = "";
    for (let k = this.next; k < end; k++) {
      const e = this.tape.events[k];
      if (e.size) {
        if (buf) this.sink?.output(buf);
        buf = "";
        this.sink?.resize(e.size);
      } else if (e.o) buf += e.o;
    }
    if (buf) this.sink?.output(buf);
    this.next = Math.max(this.next, end);
  }
  private stopClock() {
    clearTimeout(this.timer);
    this.timer = undefined;
  }
  private tick = () => {
    this.timer = undefined;
    if (this.mode !== "playing" || this.reloading) return;
    const p = this.position();
    this.feedUntil(p);
    const ev = this.tape.events[this.next];
    if (!ev) {
      if (this.phase === "live") {
        this.mode = "live"; // caught up
        this.notify();
      } else if (this.phase === "ended") {
        this.mode = "paused";
        this.pos = this.tape.end;
        this.notify();
      }
      return; // else: more history is on its way; push() ticks again
    }
    let wait = ev.t - p;
    if (this.skipIdle && wait > this.idleLimit) {
      this.pos = ev.t - this.idleLimit;
      this.baseWall = Date.now();
      wait = this.idleLimit;
    }
    this.timer = setTimeout(this.tick, Math.max(16, wait / this.speed));
  };

  /* ---- the stream ---- */
  header(h: StreamHeader) {
    this.reloading = this.phase === "offline" || this.drawn;
    this.tape = new Tape({ cols: h.cols, rows: h.rows }, h.start, this.tapeOpts);
    this.clock = h.now === undefined ? undefined : { chan: h.now, wall: Date.now() };
    this.phase = "history";
    this.reason = undefined;
    this.next = 0;
    this.stopClock();
    this.notify();
  }
  push(ev: TapeEvent) {
    const dropped = this.tape.push(ev);
    if (dropped) {
      this.next = Math.max(0, this.next - dropped);
      this.notify(); // start moved
    }
    if (this.reloading) return;
    if (this.mode === "live") {
      if (this.phase !== "live") return; // history is drawn once, at the hand-over
      if (ev.size) this.sink?.resize(ev.size);
      else if (ev.o) this.sink?.output(ev.o);
      this.next = this.tape.events.length;
    } else if (this.mode === "playing" && this.timer === undefined) this.tick();
  }
  /** resume draws where the viewer was once the (re)loaded history is in. */
  private resume() {
    const was = this.reloading;
    this.reloading = false;
    if (this.mode === "live") {
      if (this.phase === "live") this.draw(this.tape.end);
      return;
    }
    if (was || !this.drawn) {
      this.pos = Math.min(Math.max(this.pos, this.tape.start), this.tape.end);
      this.draw(this.pos);
    }
    if (this.mode === "playing") {
      this.baseWall = Date.now();
      this.tick();
    }
  }
  live() {
    this.phase = "live";
    this.resume();
    this.notify();
  }
  done() {
    this.phase = "ended";
    if (this.mode === "live") {
      this.mode = "paused";
      this.pos = this.tape.end;
      if (this.reloading || !this.drawn) this.draw(this.pos);
      this.reloading = false;
    } else this.resume();
    this.notify();
  }
  offline(why: string) {
    if (this.mode === "playing") this.pos = this.position();
    this.stopClock();
    this.phase = "offline";
    this.reason = why;
    this.notify();
  }

  /* ---- controls ---- */
  play() {
    if (this.mode !== "paused") return;
    if (this.phase === "live" && this.pos >= this.tape.end) return this.goLive();
    if (this.phase === "ended" && this.pos >= this.tape.end) this.draw((this.pos = this.tape.start));
    else if (this.pos < this.tape.start) this.draw((this.pos = this.tape.start));
    this.mode = "playing";
    this.baseWall = Date.now();
    this.notify();
    this.tick();
  }
  pause() {
    if (this.mode === "paused") return;
    const p = this.position();
    if (this.mode === "playing" && !this.reloading) this.feedUntil(p);
    this.stopClock();
    this.pos = this.mode === "live" ? this.tape.end : p;
    this.mode = "paused";
    this.notify();
  }
  toggle() {
    this.mode === "paused" ? this.play() : this.pause();
  }
  seek(t: number) {
    if (this.phase === "live" && t >= this.liveNow() - 500) return this.goLive();
    t = Math.min(Math.max(t, this.tape.start), this.timelineEnd());
    this.stopClock();
    this.pos = t;
    if (!this.reloading) this.draw(t);
    if (this.mode === "live") this.mode = "playing";
    if (this.mode === "playing") {
      this.baseWall = Date.now();
      this.notify();
      this.tick();
    } else this.notify();
  }
  goLive() {
    if (this.phase !== "live") return;
    this.stopClock();
    this.mode = "live";
    this.draw(this.tape.end);
    this.notify();
  }
  setSpeed(x: number) {
    if (this.mode === "playing") {
      this.pos = this.position();
      this.baseWall = Date.now();
      this.stopClock();
    }
    this.speed = x;
    this.notify();
    if (this.mode === "playing") this.tick();
  }
  setSkipIdle(b: boolean) {
    this.skipIdle = b;
    this.notify();
    if (this.mode === "playing") {
      this.pos = this.position();
      this.baseWall = Date.now();
      this.stopClock();
      this.tick();
    }
  }
  dispose() {
    this.stopClock();
    this.listeners.clear();
    this.sink = undefined;
  }
}
