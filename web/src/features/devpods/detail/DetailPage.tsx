// DevPod 详情 (UI spec §5.2). One SSE connection carries status, events,
// metrics and processes; with live data off, status and events come
// from the plain detail stream instead.
import { preloadYamlEditor } from "../../../design/yaml/YamlEditor";
import { Notice } from "../../../design/Form";
import { reservationNotes } from "../status";
import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import {
  ApiFailure,
  shellPath,
  sshCommand,
  sshConfig,
  watchDevPod,
  type DevPodDetail,
  type DevPodSample,
  type K8sEvent,
  type ProcLive,
} from "../../../data/api";
import { qk, useAuditSessions, useDevPod, useDevPodMutations, useMe, useMetrics } from "../../../data/queries";
import { devpodStream } from "../../../data/live/streams";
import { useLiveEnabled, useLiveRow } from "../../../data/live/hooks";
import { StatusDot } from "../../../design/StatusDot";
import { Button } from "../../../design/Button";
import { CopyBlock, CopyField, copyText } from "../../../design/Copy";
import { Panel } from "../../../design/Panel";
import { EmptyState } from "../../../design/EmptyState";
import { ConfirmDialog } from "../../../design/ConfirmDialog";
import { useUrlState, type Schema } from "../../../lib/urlState";
import { useNow } from "../../../lib/useNow";
import { fmtAge } from "../../../lib/format";
import { DevPodDrawer } from "../DevPodDrawer";
import { LiveCards } from "./LiveCards";
import { ProcessTable } from "./ProcessesTab";
import { SessionList } from "./SessionsTab";
import { EventList } from "./EventsTab";
import { MonitorTab } from "./MonitorTab";
import { ContainerTab } from "./ContainerTab";
import { PortsPanel } from "./PortsPanel";
import { PodTab } from "./PodTab";
import { YamlTab } from "./YamlTab";
import { SnapshotControl } from "../../images/SnapshotControl";
import { devpodToConfig } from "../../configs/exportConfig";

const schema = {
  tab: { kind: "string", default: "monitor" },
  edit: { kind: "bool", default: false },
} satisfies Schema;

type Msg = { kind: string; type?: string; detail?: DevPodDetail; event?: K8sEvent };

function Meta({ k, children }: { k: string; children: ReactNode }) {
  if (!children) return null;
  return (
    <span className="inline-flex min-w-0 items-baseline gap-1">
      <span className="text-faint">{k}</span>
      <span className="mono truncate text-ink">{children}</span>
    </span>
  );
}

export function DetailPage() {
  const { name = "" } = useParams();
  const me = useMe().data;
  const nav = useNavigate();
  const qc = useQueryClient();
  const live = useLiveEnabled();
  const [url, setUrl] = useUrlState(schema);
  const now = useNow(5000);
  const q = useDevPod(name);
  const [detail, setDetail] = useState<DevPodDetail | null>(null);
  const [deleted, setDeleted] = useState(false);
  const [events, setEvents] = useState<Record<string, K8sEvent>>({});
  const [confirmDelete, setConfirmDelete] = useState(false);
  const { setRunning, remove } = useDevPodMutations();

  useEffect(() => {
    setDetail(null);
    setDeleted(false);
    setEvents({});
  }, [name]);

  const onDetail = useCallback(
    (d: DevPodDetail) => {
      setDetail(d);
      qc.setQueryData(qk.devpod(name), d);
    },
    [qc, name],
  );
  const onEvent = useCallback(
    (type: string, ev: K8sEvent) =>
      setEvents((m) => {
        const next = { ...m };
        if (type === "DELETED") delete next[ev.metadata.uid];
        else next[ev.metadata.uid] = ev;
        return next;
      }),
    [],
  );
  const onOther = useCallback(
    (raw: unknown) => {
      const m = raw as Msg;
      if (m.kind === "devpod") {
        if (m.type === "DELETED") setDeleted(true);
        else if (m.detail) onDetail(m.detail);
      } else if (m.kind === "event" && m.event) onEvent(m.type ?? "ADDED", m.event);
    },
    [onDetail, onEvent],
  );

  const allProcs = url.tab === "processes";
  const spec = useMemo(() => devpodStream(name, allProcs, onOther), [name, allProcs, onOther]);
  const metrics = useLiveRow<DevPodSample>(spec, "metrics");
  const recent = useMetrics(name, "15m"); // the cards' sparklines start from history
  const procs = useLiveRow<ProcLive[]>(spec, "processes");
  // without live data the detail stream still carries status and events
  useEffect(() => {
    if (live) return;
    return watchDevPod(name, { onDetail, onEvent: (t, ev) => onEvent(t, ev) });
  }, [live, name, onDetail, onEvent]);

  const audit = !!me?.features.audit;
  const sessions = useAuditSessions({ devpod: name }, { enabled: audit && (url.tab === "overview" || url.tab === "sessions"), refetchInterval: 10_000 });

  const d = detail ?? q.data ?? null;
  const notFound = q.error instanceof ApiFailure && q.error.status === 404;
  if (deleted || (notFound && !detail)) {
    return (
      <div className="p-4">
        <EmptyState
          title="该 DevPod 已删除"
          hint={
            <Link to="/" className="text-accent hover:underline">
              ← 返回列表
            </Link>
          }
        />
      </div>
    );
  }
  if (!d) return <EmptyState title={q.isError ? "加载失败" : "加载中…"} hint={q.isError ? String(q.error) : undefined} />;

  const dp = d.devpod;
  const owner = dp.spec.owner;
  const suffix = dp.metadata.name.startsWith(owner + "-") ? dp.metadata.name.slice(owner.length + 1) : dp.metadata.name;
  const pod = d.pod;
  const runningPhase = dp.status?.phase === "Running";
  const image = dp.spec.pod?.spec?.containers?.[0]?.image;
  const b = d.binding;
  const kore = b?.pool ? `pool ${b.pool}` : b?.allocatedCpuset ? `pin ${b.allocatedCpuset}${b.reservedNuma ? ` · NUMA ${b.reservedNuma}` : ""}` : b ? "绑定中" : "";
  const tabs: [string, string][] = [
    ["monitor", "监控"],
    ["overview", "概览"],
    ["processes", "进程"],
    ...(audit ? ([["sessions", "会话"]] as [string, string][]) : []),
    ...(me?.features.httpProxy ? ([["ports", "端口"]] as [string, string][]) : []),
    ["container", "容器"],
    ["pod", "Pod"],
    ["events", "事件"],
    ["yaml", "YAML"],
  ];
  // the container's sshd port: never proxied, so not listed among the ports
  const sshPort = Number(dp.status?.endpoint?.split(":").pop()) || undefined;
  const ssh = me ? sshCommand(me, owner, suffix) : "";

  return (
    <div className="space-y-3 p-3">
      {reservationNotes(dp).map((n) => (
        <Notice key={n.type} tone={n.tone}>
          <span className="font-medium">{n.label}</span>
          {n.message && `：${n.message}`}
        </Notice>
      ))}
      <header className="flex flex-wrap items-center gap-2">
        <h2 className="mono text-base font-semibold">{dp.metadata.name}</h2>
        <StatusDot phase={dp.status?.phase} />
        {runningPhase && pod?.startedAt && <span className="mono text-xs text-muted">已运行 {fmtAge(pod.startedAt, now)}</span>}
        <span className="flex-1" />
        {me?.features.webshell && runningPhase && (
          <Button onClick={() => window.open(shellPath(name), "_blank", "noopener")} title="在新标签页打开终端（经 SSH 网关，会话会被记录）">
            终端
          </Button>
        )}
        {me?.features.snapshots && <SnapshotControl devpod={name} owner={owner} running={runningPhase} />}
        {me && (
          <Button onClick={() => void copyText(ssh)} title={ssh}>
            复制 SSH
          </Button>
        )}
        <Button onClick={() => nav("/configs/new", { state: { content: devpodToConfig(dp), title: dp.metadata.name } })} title="把这个 DevPod 的定义存成可复用、可分享的配置">
          存为配置
        </Button>
        <Button onClick={() => setUrl({ edit: true })}>编辑</Button>
        <Button onClick={() => setRunning.mutate({ name, running: !dp.spec.running })}>{dp.spec.running ? "休眠" : "启动"}</Button>
        <Button variant="danger" onClick={() => setConfirmDelete(true)}>
          删除
        </Button>
      </header>
      <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs">
        {me?.admin && <Meta k="Owner">{owner}</Meta>}
        <Meta k="节点">{pod?.node}</Meta>
        <Meta k="镜像">{image}</Meta>
        <Meta k="IP">{pod?.ip}</Meta>
        <Meta k="home">{dp.spec.persistence ? `PVC ${dp.spec.persistence.size}` : "无持久化"}</Meta>
        <Meta k="协作者">{dp.spec.collaborators?.join(", ")}</Meta>
        <Meta k="Kore">{kore}</Meta>
      </div>
      {dp.status?.message && !runningPhase && <div className="rounded-md bg-warm-soft px-3 py-1.5 text-xs text-warm">{dp.status.message}</div>}

      <LiveCards row={live ? metrics.row : undefined} hist={recent.data} />

      <div role="tablist" className="flex gap-1 border-b border-line text-xs">
        {tabs.map(([id, label]) => (
          <button
            key={id}
            role="tab"
            type="button"
            aria-selected={url.tab === id}
            onClick={() => setUrl({ tab: id })}
            // Monaco is large: start fetching it when the YAML tab is about to be opened
            onPointerEnter={id === "yaml" ? () => void preloadYamlEditor()?.catch(() => {}) : undefined}
            onFocus={id === "yaml" ? () => void preloadYamlEditor()?.catch(() => {}) : undefined}
            className={`-mb-px border-b-2 px-3 py-1.5 ${url.tab === id ? "border-accent font-semibold text-accent" : "border-transparent text-muted hover:text-ink"}`}
          >
            {label}
          </button>
        ))}
      </div>

      {url.tab === "overview" && (
        <div className="grid gap-3 lg:grid-cols-2">
          <Panel title="SSH">
            <div className="space-y-2 p-3">
              <CopyField value={ssh} />
              {me && <CopyBlock value={sshConfig(me, owner, suffix)} />}
            </div>
          </Panel>
          <Panel title="在线会话">
            {audit ? (
              <SessionList items={sessions.data?.items} error={sessions.error} now={now} onlyOpen />
            ) : (
              <div className="p-3 text-xs text-muted">审计未开启，无法列出会话。</div>
            )}
          </Panel>
          <div className="lg:col-span-2">
            <Panel title="占用最高的 10 个进程">
              <ProcessTable procs={procs.row?.value} limit={10} live={live} />
            </Panel>
          </div>
        </div>
      )}
      {url.tab === "monitor" && <MonitorTab name={name} history={live ? metrics.row?.history : undefined} />}
      {url.tab === "processes" && (
        <Panel title="全部进程（每秒刷新）">
          <ProcessTable procs={procs.row?.value} live={live} />
        </Panel>
      )}
      {url.tab === "sessions" && audit && (
        <Panel title="会话">
          <SessionList items={sessions.data?.items} error={sessions.error} now={now} />
        </Panel>
      )}
      {url.tab === "ports" && me?.features.httpProxy && <PortsPanel name={name} sshPort={sshPort} />}
      {url.tab === "container" && <ContainerTab name={name} />}
      {url.tab === "pod" && <PodTab name={name} />}
      {url.tab === "yaml" && <YamlTab name={name} />}
      {url.tab === "events" && (
        <Panel title="事件">
          <EventList events={events} />
        </Panel>
      )}

      {url.edit && <DevPodDrawer mode="edit" devpod={dp} open onClose={() => setUrl({ edit: false })} />}
      <ConfirmDialog
        open={confirmDelete}
        onOpenChange={setConfirmDelete}
        title={`删除 ${dp.metadata.name}？`}
        confirmLabel="确认删除"
        danger
        onConfirm={() => {
          setConfirmDelete(false);
          remove.mutate(name, { onSuccess: () => nav("/") });
        }}
      >
        Pod 和持久化数据都会被移除，不能恢复。
      </ConfirmDialog>
    </div>
  );
}
