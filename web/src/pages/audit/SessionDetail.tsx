import { useMemo, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { castURL, fileContentURL, getAuditSession, me, rawURL, ChannelRow, FileEventRow } from "../../api";
import { castOffset, mergeTimeline, TimelineItem } from "../../audit";
import { CastPlayer } from "../../components/CastPlayer";
import { useNow } from "../../components/DevPodTable";
import { fmtBytes, fmtDuration } from "../../format";
import { BackLink, Shell, cx } from "../../ui";
import { fmtTS } from "./common";

const kindTone: Record<TimelineItem["kind"], string> = {
  channel: "text-faint",
  command: "text-ink",
  file: "text-accent",
  policy: "text-warm",
  alert: "text-fail",
};

function FileLink({ f }: { f: FileEventRow }) {
  return (
    <>
      {f.blocked && <span className="ml-1 rounded border border-warm/30 bg-warm-soft px-1 text-[9.5px] text-warm">blocked</span>}
      {!f.blocked && f.s3_key && !f.complete && <span className="ml-1 rounded border border-line px-1 text-[9.5px] text-muted">incomplete</span>}
      {f.s3_key && (
        <a
          href={fileContentURL(f.event_id)}
          onClick={(e) => {
            if (!confirm("此操作将被记录")) e.preventDefault();
          }}
          className="ml-2 text-accent hover:underline"
        >
          ⬇ {fmtBytes(f.size)}
        </a>
      )}
    </>
  );
}

export default function AuditSessionDetail() {
  const { id = "" } = useParams();
  const meQ = useQuery({ queryKey: ["me"], queryFn: me });
  const q = useQuery({ queryKey: ["audit-session", id], queryFn: () => getAuditSession(id) });
  const now = useNow(5000);
  const channels: ChannelRow[] = q.data?.channels ?? [];
  const defaultCh = (channels.find((c) => c.pty) ?? channels.find((c) => c.type === "session"))?.channel_id;
  const [picked, setPicked] = useState<number | undefined>();
  const ch = picked ?? defaultCh;
  const [seek, setSeek] = useState<{ t: number; n: number }>();
  const timeline = useMemo(() => (q.data ? mergeTimeline(q.data) : []), [q.data]);

  if (!q.data) return <Shell wide><p className="mono py-16 text-center text-sm text-faint">{q.error ? String(q.error) : "loading…"}</p></Shell>;
  const s = q.data.session;
  const end = s.closed_at ? new Date(s.closed_at) : now;
  let snapshot: string[] = [];
  try {
    snapshot = Object.entries(JSON.parse(s.policy_snapshot || "{}") as Record<string, unknown>)
      .filter(([, v]) => v)
      .map(([k, v]) => (v === true ? k : `${k}=${v}`));
  } catch {
    /* old rows */
  }

  const jump = (item: TimelineItem) => {
    const target = channels.find((c) => c.channel_id === (item.channelId ?? ch));
    if (!target) return;
    setPicked(target.channel_id);
    setSeek({ t: castOffset(target.opened_at, item.ts), n: Date.now() });
  };
  const current = channels.find((c) => c.channel_id === ch);

  return (
    <Shell wide>
      <BackLink to="/audit/sessions" label="Sessions" />
      <header className="mono mt-3 flex flex-wrap items-baseline gap-x-4 gap-y-1 text-xs text-muted">
        <h1 className="text-xl font-semibold tracking-tight text-ink">{s.user}</h1>
        <span>
          devpod <Link to={`/devpods/${s.devpod}`} className="text-ink hover:text-accent">{s.devpod}</Link>
        </span>
        <span>from <b className="font-medium text-ink">{s.client_ip}</b></span>
        <span>start <b className="font-medium text-ink">{fmtTS(s.opened_at)}</b></span>
        <span>dur <b className="font-medium text-ink">{fmtDuration(end.getTime() - new Date(s.opened_at).getTime())}</b>{!s.closed_at && " (live)"}</span>
        <span>↓{fmtBytes(s.bytes_out)} ↑{fmtBytes(s.bytes_in)}</span>
        {s.close_reason && <span>closed: {s.close_reason}</span>}
        {s.gaps > 0 && <span className="text-fail">{s.gaps} gaps</span>}
      </header>
      <p className="mono mb-4 mt-1 text-[11px] text-faint">policy: {snapshot.join(", ") || "allow everything"} · session {s.session_id}</p>

      <div className="grid gap-4 lg:grid-cols-[minmax(0,1.6fr)_minmax(0,1fr)]">
        <div>
          <div className="mono mb-2 flex flex-wrap gap-1 text-xs">
            {channels.map((c) => (
              <button
                key={c.channel_id}
                onClick={() => setPicked(c.channel_id)}
                className={cx(
                  "rounded-md border px-2 py-0.5",
                  c.channel_id === ch ? "border-accent/40 bg-accent-soft text-accent" : "border-line bg-surface text-muted hover:text-ink",
                )}
              >
                #{c.channel_id} {c.type === "session" ? c.command || c.subsystem || (c.pty ? "shell" : "session") : `${c.type} ${c.target}`}
              </button>
            ))}
          </div>
          {ch !== undefined && current?.raw_recorded ? (
            <CastPlayer key={ch} src={castURL(id, ch)} seekTo={seek?.t} />
          ) : (
            <p className="mono rounded-lg border border-dashed border-line p-8 text-center text-xs text-faint">
              {current ? "Content of this channel was not recorded (metadata only)." : "No channel."}
            </p>
          )}
          {meQ.data?.admin && ch !== undefined && current?.raw_recorded && (
            <p className="mono mt-2 text-[11px] text-faint">
              raw:{" "}
              {(["in", "out", "err"] as const).map((st) => (
                <a key={st} href={rawURL(id, ch, st)} className="mr-2 text-accent hover:underline">
                  {st}
                </a>
              ))}
            </p>
          )}
        </div>

        <ol className="mono max-h-[70vh] overflow-y-auto rounded-lg border border-line bg-surface p-2 text-xs">
          {timeline.map((item, i) => (
            <li key={i} className="flex gap-2 rounded px-1.5 py-1 hover:bg-sunk">
              <button onClick={() => jump(item)} className="shrink-0 text-faint hover:text-accent" title="jump the replay here">
                {new Date(item.ts).toLocaleTimeString()}
              </button>
              <span className={cx("min-w-0 break-all", kindTone[item.kind])}>
                {item.text}
                {item.kind === "file" && <FileLink f={item.ref as FileEventRow} />}
              </span>
            </li>
          ))}
          {timeline.length === 0 && <li className="p-4 text-center text-faint">Nothing recorded yet.</li>}
        </ol>
      </div>
    </Shell>
  );
}
