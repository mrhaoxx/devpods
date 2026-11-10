// streamTape feeds a TapePlayer from the server: the live stream for an
// open session (falling back to the recording when it has ended or live
// viewing is off), reconnecting with backoff when the stream drops.
import { parseLine } from "./tape";
import type { TapePlayer } from "./tapePlayer";

export type TapeSource = { live?: string; cast: string };
type Target = Pick<TapePlayer, "header" | "push" | "live" | "done" | "offline">;

const MESSAGES: Record<number, string> = { 429: "观看人数已满，稍后自动重试", 502: "网关不可达", 503: "网关不可达" };

export function streamTape(src: TapeSource, p: Target, fetchFn: typeof fetch = fetch): () => void {
  const ctl = new AbortController();
  let timer: ReturnType<typeof setTimeout> | undefined;
  let useLive = !!src.live;
  let delay = 1000;

  const retry = (why: string) => {
    if (ctl.signal.aborted) return;
    p.offline(why);
    timer = setTimeout(run, delay);
    delay = Math.min(delay * 2, 15_000);
  };
  const run = async () => {
    let finished = false;
    let dropped: string | undefined;
    try {
      const resp = await fetchFn(useLive ? src.live! : src.cast, { signal: ctl.signal, credentials: "same-origin" });
      if (!resp.ok || !resp.body) {
        if (useLive && (resp.status === 409 || resp.status === 404)) {
          useLive = false; // ended, or live viewing is off: the recording has it
          return void run();
        }
        return retry(MESSAGES[resp.status] ?? `HTTP ${resp.status}`);
      }
      const reader = resp.body.getReader();
      const dec = new TextDecoder();
      let buf = "";
      const feed = (line: string) => {
        const l = parseLine(line);
        if (l.kind === "header") {
          delay = 1000;
          p.header(l);
        } else if (l.kind === "event") p.push(l.ev);
        else if (l.kind === "live") p.live();
        else if (l.kind === "done") finished = true;
        else if (l.kind === "offline") dropped = l.why;
      };
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        let nl: number;
        let from = 0;
        while ((nl = buf.indexOf("\n", from)) >= 0) {
          feed(buf.slice(from, nl));
          from = nl + 1;
        }
        buf = buf.slice(from);
      }
      if (buf.trim()) feed(buf);
    } catch {
      if (ctl.signal.aborted) return;
      return retry("连接中断");
    }
    if (finished || !useLive) p.done(); // a recording simply ends
    else retry(dropped === "overflow" ? "输出过快，正在重新载入" : "连接中断");
  };
  void run();
  return () => {
    ctl.abort();
    clearTimeout(timer);
  };
}
