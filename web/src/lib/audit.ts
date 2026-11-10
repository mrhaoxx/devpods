import { SessionDetail } from "../data/api";

export type TimelineItem = {
  ts: string;
  kind: "channel" | "command" | "file" | "policy" | "alert";
  channelId?: number;
  text: string;
  ref?: unknown;
};

// mergeTimeline flattens a session's rows into one time-ordered list.
// Equal timestamps keep the section order: channels, commands, files,
// policy, alerts (Array.prototype.sort is stable).
export function mergeTimeline(d: SessionDetail): TimelineItem[] {
  const out: TimelineItem[] = [];
  for (const c of d.channels ?? []) {
    const what = c.command || (c.subsystem && `subsystem ${c.subsystem}`) || c.target || (c.pty ? "shell" : c.type);
    out.push({ ts: c.opened_at, kind: "channel", channelId: c.channel_id, text: `#${c.channel_id} ${c.type} · ${what}${c.pty ? " (pty)" : ""}`, ref: c });
  }
  for (const c of d.commands ?? []) {
    const exit = c.exit_code == null ? "" : ` → ${c.exit_code}`;
    out.push({ ts: c.ts, kind: "command", channelId: c.channel_id, text: `${c.cmdline}${exit}`, ref: c });
  }
  for (const f of d.files ?? []) {
    out.push({ ts: f.ts, kind: "file", channelId: f.channel_id, text: `${f.protocol} ${f.op} ${f.path}${f.path2 ? ` → ${f.path2}` : ""}`, ref: f });
  }
  for (const p of d.policy ?? []) {
    out.push({ ts: p.ts, kind: "policy", text: `${p.rule} ${p.action}: ${p.detail}`, ref: p });
  }
  for (const a of d.alerts ?? []) {
    out.push({ ts: a.ts, kind: "alert", text: `${a.kind}: ${a.detail}`, ref: a });
  }
  return out.sort((a, b) => new Date(a.ts).getTime() - new Date(b.ts).getTime());
}

// castPlayerOptions configures asciinema-player for audit replays. No
// idleTimeLimit: it compresses idle gaps and seek() then runs on the
// compressed clock, so a timeline jump (castOffset, wall-clock seconds)
// would land late after any pause.
export const castPlayerOptions = { fit: "width", terminalFontSize: "13px" } as const;

// castOffset is where in a channel's recording an event happened.
export function castOffset(channelOpenedAt: string, ts: string): number {
  return Math.max(0, (new Date(ts).getTime() - new Date(channelOpenedAt).getTime()) / 1000);
}
