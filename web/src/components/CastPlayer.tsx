import { useEffect, useRef } from "react";
import * as AsciinemaPlayer from "asciinema-player";
import "asciinema-player/dist/bundle/asciinema-player.css";

// CastPlayer wraps asciinema-player; changing seekTo jumps the replay.
export function CastPlayer({ src, seekTo }: { src: string; seekTo?: number }) {
  const el = useRef<HTMLDivElement>(null);
  const player = useRef<AsciinemaPlayer.Player | null>(null);

  useEffect(() => {
    if (!el.current) return;
    const p = AsciinemaPlayer.create(src, el.current, { fit: "width", idleTimeLimit: 2, terminalFontSize: "13px" });
    player.current = p;
    return () => {
      p.dispose();
      player.current = null;
    };
  }, [src]);

  useEffect(() => {
    if (seekTo !== undefined && player.current) void player.current.seek(seekTo);
  }, [seekTo]);

  return <div ref={el} className="overflow-hidden rounded-lg border border-line bg-black" />;
}
