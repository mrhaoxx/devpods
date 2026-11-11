import type { ChannelRow } from "../../data/api";

/** Commands that speak a binary protocol over the channel: their output
 *  is not text to show. */
const BINARY_COMMANDS = new Set(["scp", "rsync", "sftp-server", "git-upload-pack", "git-receive-pack", "git-upload-archive"]);

/** binaryCommand: the command's program is one of the file-transfer /
 *  git transports. */
export function binaryCommand(command: string): boolean {
  const program = command.trim().split(/\s+/)[0] ?? "";
  return BINARY_COMMANDS.has(program.slice(program.lastIndexOf("/") + 1));
}

/** replayable: a session channel whose output is text plays — a shell, a
 *  terminal, or a command run without one (`ssh host ls`). Subsystems
 *  (sftp), the binary transports above and forwarded channels show their
 *  file events and, for admins, the raw streams instead. */
export function replayable(ch: ChannelRow): boolean {
  return ch.type === "session" && !ch.subsystem && (ch.pty || !binaryCommand(ch.command));
}

/** plainOutput: the channel had no terminal, so its output ends lines
 *  with a bare line feed; the player turns them into CR LF and picks its
 *  own width. */
export function plainOutput(ch: ChannelRow): boolean {
  return !ch.pty;
}

export function channelLabel(c: ChannelRow): string {
  if (c.type !== "session") return `${c.type} ${c.target}`.trim();
  if (c.subsystem) return c.subsystem;
  if (c.command) return c.command.split(/\s+/)[0];
  return c.pty ? "shell" : "session";
}
