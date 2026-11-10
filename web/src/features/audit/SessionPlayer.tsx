// SessionPlayer: one player for live and replay (audit spec §16). The
// asciinema terminal renders what TapePlayer feeds it through a custom
// driver; the control bar below is ours: play / pause, a scrubber over
// the whole channel with the timeline's events as ticks, the live badge,
// speed and idle skipping.
import { useEffect, useMemo, useRef, useState, useSyncExternalStore } from "react";
import * as AsciinemaPlayer from "asciinema-player";
import "asciinema-player/dist/bundle/asciinema-player.css";
import { castURL, liveURL } from "../../data/api";
import { playerOptions } from "../../lib/audit";
import { streamTape } from "./tapeStream";
import { TapePlayer, type PlayerView } from "./tapePlayer";

export type Marker = { t: number; label: string; kind: "channel" | "command" | "file" | "policy" | "alert" };
const SPEEDS = [0.5, 1, 2, 4, 8, 16];
const tick: Record<Marker["kind"], string> = { channel: "bg-faint", command: "bg-muted", file: "bg-accent", policy: "bg-warm", alert: "bg-fail" };

/** fmtClock: 75_000 → "1:15", 3_725_000 → "1:02:05". */
export function fmtClock(ms: number): string {
  const s = Math.max(0, Math.floor(ms / 1000));
  const h = Math.floor(s / 3600);
  const mm = Math.floor((s % 3600) / 60);
  const ss = String(s % 60).padStart(2, "0");
  return h ? `${h}:${String(mm).padStart(2, "0")}:${ss}` : `${mm}:${ss}`;
}

function Scrubber({ view, pos, end, markers, openedAt, onSeek }: { view: PlayerView; pos: number; end: number; markers: Marker[]; openedAt: number; onSeek: (t: number) => void }) {
  const el = useRef<HTMLDivElement>(null);
  const [hover, setHover] = useState<{ x: number; t: number } | null>(null);
  const drag = useRef(false);
  const last = useRef(0);
  const span = Math.max(end, 1);
  const pct = (t: number) => `${Math.min(100, Math.max(0, (t / span) * 100))}%`;
  const at = (clientX: number) => {
    const r = el.current!.getBoundingClientRect();
    const f = r.width > 0 ? Math.min(1, Math.max(0, (clientX - r.left) / r.width)) : 0;
    return { x: f * r.width, t: Math.max(view.start, f * span) };
  };
  const near = hover && markers.reduce<Marker | null>((best, m) => (Math.abs(m.t - hover.t) <= span * 0.008 && (!best || Math.abs(m.t - hover.t) < Math.abs(best.t - hover.t)) ? m : best), null);
  return (
    <div
      ref={el}
      role="slider"
      tabIndex={0}
      aria-label="播放位置"
      aria-valuemin={0}
      aria-valuemax={Math.round(span / 1000)}
      aria-valuenow={Math.round(pos / 1000)}
      aria-valuetext={fmtClock(pos)}
      className="group relative h-6 min-w-0 flex-1 cursor-pointer touch-none select-none outline-none"
      onPointerDown={(e) => {
        drag.current = true;
        e.currentTarget.setPointerCapture?.(e.pointerId);
        onSeek(at(e.clientX).t);
      }}
      onPointerMove={(e) => {
        const h = at(e.clientX);
        setHover(h);
        if (drag.current && e.timeStamp - last.current > 80) {
          last.current = e.timeStamp;
          onSeek(h.t);
        }
      }}
      onPointerUp={(e) => {
        if (drag.current) onSeek(at(e.clientX).t);
        drag.current = false;
      }}
      onPointerLeave={() => setHover(null)}
      onKeyDown={(e) => {
        const d = e.key === "ArrowLeft" ? -5000 : e.key === "ArrowRight" ? 5000 : 0;
        if (d) {
          e.preventDefault();
          onSeek(pos + d);
        }
      }}
    >
      <div className="absolute inset-x-0 top-1/2 h-1 -translate-y-1/2 overflow-hidden rounded-full bg-line">
        {view.start > 0 && <div className="absolute inset-y-0 left-0 bg-faint/40" style={{ width: pct(view.start) }} title="这部分过早，没有载入" />}
        <div className="absolute inset-y-0 bg-accent" style={{ left: pct(view.start), width: `calc(${pct(pos)} - ${pct(view.start)})` }} />
      </div>
      {markers.map((m, i) => (
        <span key={i} className={`pointer-events-none absolute top-1/2 h-2.5 w-px -translate-y-1/2 ${tick[m.kind]}`} style={{ left: pct(m.t) }} />
      ))}
      <span
        className={`pointer-events-none absolute top-1/2 h-3 w-3 -translate-x-1/2 -translate-y-1/2 rounded-full border-2 border-surface shadow-sm group-focus-visible:ring-2 group-focus-visible:ring-accent ${view.mode === "live" ? "bg-fail" : "bg-accent"}`}
        style={{ left: pct(pos) }}
      />
      {hover && (
        <div className="mono pointer-events-none absolute bottom-full z-10 mb-1 max-w-64 -translate-x-1/2 truncate rounded border border-line bg-surface px-1.5 py-0.5 text-[11px] text-ink shadow-sm" style={{ left: hover.x }}>
          {new Date(openedAt + hover.t).toLocaleTimeString()} · {fmtClock(hover.t)}
          {near && <span className="text-muted"> · {near.label}</span>}
        </div>
      )}
    </div>
  );
}

export function SessionPlayer({
  sessionId,
  channel,
  open,
  openedAt,
  markers = [],
  seek,
  onPosition,
}: {
  sessionId: string;
  channel: number;
  /** the session is still open: start at the live edge */
  open: boolean;
  /** when the channel opened (the tape's zero), ISO */
  openedAt: string;
  markers?: Marker[];
  /** a jump request, ms on the channel clock; n makes repeats distinct */
  seek?: { t: number; n: number };
  onPosition?: (ms: number) => void;
}) {
  const el = useRef<HTMLDivElement>(null);
  // the player outlives re-renders; open only picks where it starts
  const player = useMemo(() => new TapePlayer({ initial: open ? "live" : "paused" }), [sessionId, channel]); // eslint-disable-line react-hooks/exhaustive-deps
  const view = useSyncExternalStore(player.subscribe, player.getView);
  const [clock, setClock] = useState({ pos: 0, end: 0 });
  const zero = useMemo(() => new Date(openedAt).getTime(), [openedAt]);

  useEffect(() => {
    if (!el.current) return;
    let dispatch: ((ev: string, data?: unknown) => void) | undefined;
    let booted = false;
    const term = AsciinemaPlayer.create(
      (cb) => {
        dispatch = cb.dispatch;
        player.attach({
          reset: (size) => cb.dispatch("reset", { size, init: "" }),
          output: (d) => cb.dispatch("output", d),
          resize: (size) => cb.dispatch("resize", size),
        });
        return {
          // the core "autoplays" once at start; our player decides that itself
          play: () => (booted ? player.play() : void (booted = true)),
          pause: () => player.pause(),
          seek: (where) => {
            const pos = player.position();
            if (typeof where === "number") player.seek(where * 1000);
            else if (where === "<<") player.seek(pos - 5000);
            else if (where === ">>") player.seek(pos + 5000);
            else if (where === "<<<") player.seek(pos - 60_000);
            else if (where === ">>>") player.seek(pos + 60_000);
            else if (typeof where === "string" && where.endsWith("%")) player.seek((parseFloat(where) / 100) * player.timelineEnd());
            else return false;
            return true;
          },
          getTiming: () => ({ time: player.position() / 1000, rate: 0 }),
        };
      },
      el.current,
      { ...playerOptions },
    );
    // keep the terminal's own play state (space key, cursor blink) in step
    const sync = () => dispatch?.(player.getView().mode === "paused" ? "pause" : "playing");
    const unsub = player.subscribe(sync);
    const stop = streamTape({ live: open ? liveURL(sessionId, channel) : undefined, cast: castURL(sessionId, channel) }, player);
    return () => {
      stop();
      unsub();
      term.dispose();
      player.dispose();
    };
  }, [player]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    if (seek) player.seek(seek.t);
  }, [seek, player]);

  const moving = view.mode !== "paused" || view.phase === "live";
  useEffect(() => {
    const read = () => {
      const pos = player.position();
      setClock({ pos, end: player.timelineEnd() });
      onPosition?.(pos);
    };
    read();
    if (!moving) return;
    const id = setInterval(read, 250);
    return () => clearInterval(id);
  }, [player, view, moving]); // eslint-disable-line react-hooks/exhaustive-deps

  const status =
    view.phase === "connecting" || view.phase === "history"
      ? "载入中…"
      : view.phase === "offline"
        ? `已断开，正在重连${view.reason ? `（${view.reason}）` : ""}`
        : view.phase === "ended" && open
          ? "会话已结束"
          : "";

  return (
    <div className="overflow-hidden rounded-md border border-line">
      <div ref={el} className="bg-black" />
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 border-t border-line bg-surface px-2 py-1.5 text-xs">
        <button
          type="button"
          onClick={() => player.toggle()}
          aria-label={view.mode === "paused" ? "播放" : "暂停"}
          className="grid h-6 w-6 shrink-0 place-items-center rounded text-ink hover:bg-sunk"
        >
          {view.mode === "paused" ? (
            <svg viewBox="0 0 12 12" className="h-3 w-3 fill-current" aria-hidden>
              <path d="M2.5 1.2v9.6L10.5 6z" />
            </svg>
          ) : (
            <svg viewBox="0 0 12 12" className="h-3 w-3 fill-current" aria-hidden>
              <path d="M2.5 1.5h2.5v9H2.5zM7 1.5h2.5v9H7z" />
            </svg>
          )}
        </button>
        <span className="mono shrink-0 tabular-nums text-muted" title="播放位置的时刻">
          <span className="text-ink">{new Date(zero + clock.pos).toLocaleTimeString()}</span> · {fmtClock(clock.pos)} / {fmtClock(clock.end)}
        </span>
        <Scrubber view={view} pos={clock.pos} end={clock.end} markers={markers} openedAt={zero} onSeek={(t) => player.seek(t)} />
        {view.phase === "live" &&
          (view.mode === "live" ? (
            <span className="inline-flex shrink-0 items-center gap-1.5 rounded border border-line px-1.5 py-0.5 font-medium text-ink">
              <span className="h-1.5 w-1.5 rounded-full bg-fail" aria-hidden />
              直播
            </span>
          ) : (
            <button type="button" onClick={() => player.goLive()} className="inline-flex shrink-0 items-center gap-1.5 rounded border border-line px-1.5 py-0.5 text-muted hover:bg-sunk hover:text-ink">
              <span className="h-1.5 w-1.5 rounded-full bg-faint" aria-hidden />
              回到直播
            </button>
          ))}
        <label className="flex shrink-0 items-center gap-1 text-muted">
          <span className="sr-only">速度</span>
          <select aria-label="速度" value={view.speed} onChange={(e) => player.setSpeed(Number(e.target.value))} className="mono rounded border border-line bg-surface px-1 py-0.5 text-ink">
            {SPEEDS.map((x) => (
              <option key={x} value={x}>
                {x}×
              </option>
            ))}
          </select>
        </label>
        <label className="flex shrink-0 items-center gap-1 text-muted" title="回放时把超过 2 秒的空闲缩短到 2 秒">
          <input type="checkbox" checked={view.skipIdle} onChange={(e) => player.setSkipIdle(e.target.checked)} />
          跳过空闲
        </label>
        {status && (
          <span role="status" className="basis-full text-[11px] text-muted">
            {status}
          </span>
        )}
      </div>
    </div>
  );
}
