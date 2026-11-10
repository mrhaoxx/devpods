import { useEffect, useRef } from "react";
import * as AsciinemaPlayer from "asciinema-player";
import "asciinema-player/dist/bundle/asciinema-player.css";
import { castPlayerOptions } from "../../lib/audit";

// CastPlayer wraps asciinema-player; changing seekTo jumps the replay.
// live plays an open session through the eventsource driver (audit spec §16).
export function CastPlayer({ src, seekTo, live }: { src: string; seekTo?: number; live?: boolean }) {
  const el = useRef<HTMLDivElement>(null);
  const player = useRef<AsciinemaPlayer.Player | null>(null);

  useEffect(() => {
    if (!el.current) return;
    const p = live
      ? AsciinemaPlayer.create({ driver: "eventsource", url: src }, el.current, { ...castPlayerOptions, autoPlay: true })
      : AsciinemaPlayer.create(src, el.current, { ...castPlayerOptions });
    player.current = p;
    return () => {
      p.dispose();
      player.current = null;
    };
  }, [src, live]);

  useEffect(() => {
    if (seekTo !== undefined && player.current) void player.current.seek(seekTo);
  }, [seekTo]);

  return <div ref={el} className="overflow-hidden rounded-md border border-line bg-black" />;
}
