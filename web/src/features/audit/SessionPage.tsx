// 会话详情 (UI spec §5.8): timeline (channels, commands, files, 拦截,
// 告警) beside one player that is live for an open session and can be
// scrubbed back (audit spec §16); shells, terminals and commands play,
// file transfers and forwards do not.
import { useCallback, useMemo, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { disconnectSession, fileContentURL, getAuditSession, rawURL, type ChannelRow, type FileEventRow } from "../../data/api";
import { useMe } from "../../data/queries";
import { castOffset, mergeTimeline, type TimelineItem } from "../../lib/audit";
import { fmtBytes, fmtDuration } from "../../lib/format";
import { useNow } from "../../lib/useNow";
import { Badge } from "../../design/Badge";
import { Button } from "../../design/Button";
import { ConfirmDialog } from "../../design/ConfirmDialog";
import { EmptyState } from "../../design/EmptyState";
import { Panel } from "../../design/Panel";
import { SessionPlayer, type Marker } from "./SessionPlayer";
import { AuditError, confirmDownload, fmtTS } from "./common";
import { channelLabel, plainOutput, replayable } from "./replay";

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
      {f.blocked && (
        <>
          {" "}
          <Badge tone="warm">已拦截</Badge>
        </>
      )}
      {!f.blocked && f.s3_key && !f.complete && (
        <>
          {" "}
          <Badge>不完整</Badge>
        </>
      )}
      {f.s3_key && (
        <a href={fileContentURL(f.event_id)} onClick={confirmDownload} className="ml-2 text-accent hover:underline">
          ⬇ 下载 {fmtBytes(f.size)}
        </a>
      )}
    </>
  );
}

export function SessionPage() {
  const { id = "" } = useParams();
  const me = useMe().data;
  const q = useQuery({
    queryKey: ["audit", "session", id],
    queryFn: () => getAuditSession(id),
    retry: false,
    // an open session's timeline keeps growing
    refetchInterval: (query) => (query.state.data && !query.state.data.session.closed_at ? 5000 : false),
  });
  const now = useNow(5000);
  const qc = useQueryClient();
  const [cutting, setCutting] = useState(false);
  const cut = useMutation({
    mutationFn: () => disconnectSession(id),
    onSettled: () => void qc.invalidateQueries({ queryKey: ["audit", "session", id] }),
  });
  const channels: ChannelRow[] = q.data?.channels ?? [];
  const defaultCh = (channels.find(replayable) ?? channels[0])?.channel_id;
  const [picked, setPicked] = useState<number | undefined>();
  const ch = picked ?? defaultCh;
  const [seek, setSeek] = useState<{ t: number; n: number }>();
  const [playhead, setPlayhead] = useState(-1); // the timeline item at the player's position
  const timeline = useMemo(() => (q.data ? mergeTimeline(q.data) : []), [q.data]);
  const chOpened = channels.find((c) => c.channel_id === ch)?.opened_at;
  const markers = useMemo<Marker[]>(
    () =>
      chOpened
        ? timeline
            .filter((it) => it.kind !== "channel" && (it.channelId ?? ch) === ch)
            .map((it) => ({ t: castOffset(chOpened, it.ts) * 1000, label: it.text, kind: it.kind }))
        : [],
    [timeline, chOpened, ch],
  );
  const onPosition = useCallback(
    (ms: number) => {
      if (!chOpened) return;
      const at = new Date(chOpened).getTime() + ms;
      let idx = -1;
      for (let i = 0; i < timeline.length; i++) {
        if (new Date(timeline[i].ts).getTime() <= at) idx = i;
        else break;
      }
      setPlayhead(idx);
    },
    [timeline, chOpened],
  );

  if (q.isError) return <AuditError error={q.error} />;
  if (!q.data) return <EmptyState title="加载中…" />;
  const s = q.data.session;
  const end = s.closed_at ? new Date(s.closed_at) : now;
  const current = channels.find((c) => c.channel_id === ch);
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
    if (replayable(target)) setSeek({ t: castOffset(target.opened_at, item.ts) * 1000, n: Date.now() });
  };
  const channelFiles = (q.data.files ?? []).filter((f) => f.channel_id === ch);
  const open = !s.closed_at && !!current && !current.closed_at;

  return (
    <div className="space-y-3 p-3">
      <header className="flex flex-wrap items-baseline gap-x-4 gap-y-1 text-xs text-muted">
        <h2 className="mono text-base font-semibold text-ink">{s.user}</h2>
        <span>
          DevPod{" "}
          <Link to={`/devpods/${s.devpod}`} className="mono text-ink hover:text-accent">
            {s.devpod}
          </Link>
        </span>
        <span>
          来源 <span className="mono text-ink">{s.client_ip}</span> · {s.proxy_alias ? `${s.proxy_alias}/` : ""}
          {s.auth_kind}
        </span>
        <span>开始 {fmtTS(s.opened_at)}</span>
        <span>
          时长 <span className="mono text-ink">{fmtDuration(end.getTime() - new Date(s.opened_at).getTime())}</span>
          {!s.closed_at && " （进行中）"}
        </span>
        <span className="mono">
          ↓{fmtBytes(s.bytes_out)} ↑{fmtBytes(s.bytes_in)}
        </span>
        {s.close_reason && <span>结束：{s.close_reason}</span>}
        {s.gaps > 0 && <span className="text-fail">gap {s.gaps}</span>}
        {me?.admin && !s.closed_at && (
          <Button variant="danger" size="sm" className="ml-auto" disabled={cut.isPending} onClick={() => setCutting(true)}>
            断开会话
          </Button>
        )}
      </header>
      {cut.isError && (
        <p role="alert" className="text-xs text-fail">
          断开失败：{cut.error instanceof Error ? cut.error.message : String(cut.error)}
        </p>
      )}
      <ConfirmDialog open={cutting} onOpenChange={setCutting} title="断开这个会话？" confirmLabel="断开" danger
        onConfirm={() => {
          setCutting(false);
          cut.mutate();
        }}
      >
        <span className="mono">{s.user}</span> 到 <span className="mono">{s.devpod}</span> 的 SSH 连接会立刻被切断，其中正在运行的前台程序会收到挂断信号。DevPod 本身不受影响，用户可以重新连接。此操作会写入访问记录。
      </ConfirmDialog>
      <p className="mono text-[11px] text-faint">
        策略：{snapshot.join(", ") || "全部允许"} · 会话 {s.session_id}
      </p>
      <div className="grid gap-3 lg:grid-cols-[minmax(0,1fr)_minmax(0,1.5fr)]">
        <Panel title="时间线">
          <ol className="mono max-h-[70vh] overflow-y-auto p-1 text-xs">
            {timeline.map((item, i) => (
              <li key={i} aria-current={i === playhead ? "true" : undefined} className={`flex gap-2 rounded px-1.5 py-1 hover:bg-sunk ${i === playhead ? "bg-accent-soft" : ""}`}>
                <button
                  type="button"
                  onClick={() => jump(item)}
                  aria-label={`跳到 ${new Date(item.ts).toLocaleTimeString()}`}
                  className="shrink-0 text-faint hover:text-accent"
                >
                  {new Date(item.ts).toLocaleTimeString()}
                </button>
                <span className={`min-w-0 break-all ${kindTone[item.kind]}`}>
                  {item.text}
                  {item.kind === "file" && <FileLink f={item.ref as FileEventRow} />}
                </span>
              </li>
            ))}
            {timeline.length === 0 && <li className="p-4 text-center text-faint">还没有记录</li>}
          </ol>
        </Panel>
        <div className="space-y-2">
          <div className="flex flex-wrap gap-1 text-xs">
            {channels.map((c) => (
              <button
                key={c.channel_id}
                type="button"
                onClick={() => setPicked(c.channel_id)}
                className={`mono rounded-md border px-2 py-0.5 ${c.channel_id === ch ? "border-accent bg-accent-soft text-accent" : "border-line text-muted hover:text-ink"}`}
              >
                #{c.channel_id} {channelLabel(c)}
              </button>
            ))}
          </div>
          {!current ? (
            <EmptyState title="没有通道" />
          ) : replayable(current) ? (
            open || current.raw_recorded ? (
              <SessionPlayer
                key={current.channel_id}
                sessionId={id}
                channel={current.channel_id}
                open={open}
                openedAt={current.opened_at}
                text={plainOutput(current)}
                markers={markers}
                seek={seek}
                onPosition={onPosition}
              />
            ) : (
              <EmptyState title="该通道只记录了元数据" />
            )
          ) : (
            <div className="space-y-2 rounded-md border border-dashed border-line p-3 text-xs">
              <p className="text-muted">该通道（{channelLabel(current)}）传输的是二进制数据（文件传输或转发），没有可显示的文本。</p>
              {channelFiles.length > 0 && (
                <ul className="space-y-1">
                  {channelFiles.map((f) => (
                    <li key={f.event_id} className="mono">
                      {f.op} {f.path}
                      <FileLink f={f} />
                    </li>
                  ))}
                </ul>
              )}
            </div>
          )}
          {me?.admin && current?.raw_recorded && (
            <p className="mono text-[11px] text-faint">
              原始流（仅管理员）：
              {(["in", "out", "err"] as const).map((st) => (
                <a key={st} href={rawURL(id, current.channel_id, st)} onClick={confirmDownload} className="ml-2 text-accent hover:underline">
                  {st}
                </a>
              ))}
            </p>
          )}
        </div>
      </div>
    </div>
  );
}
