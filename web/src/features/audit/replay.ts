import type { ChannelRow } from "../../data/api";

/** replayable: only interactive pty and shell channels get a replay
 *  (UI spec §5.8); exec, sftp, scp and forwarded channels show their
 *  file events and, for admins, the raw streams. */
export function replayable(ch: ChannelRow): boolean {
  return ch.type === "session" && (ch.pty || (!ch.command && !ch.subsystem));
}

export function channelLabel(c: ChannelRow): string {
  if (c.type !== "session") return `${c.type} ${c.target}`.trim();
  if (c.subsystem) return c.subsystem;
  if (c.command) return c.command.split(/\s+/)[0];
  return c.pty ? "shell" : "session";
}
