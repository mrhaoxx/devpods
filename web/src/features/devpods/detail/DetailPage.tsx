// DevPod 详情 (UI spec §5.2). One SSE connection carries status, events,
// metrics and processes; with live data off, status and events come
// from the plain detail stream instead.
import { LoadMore, usePaged } from "../../audit/common";
import { Provenance } from "./Provenance";
import { preloadYamlEditor } from "../../../design/yaml/YamlEditor";
import { Notice } from "../../../design/Form";
import { phaseSummary, pinnedNode, queueOf, queuePriorityOf, reservationNotes } from "../status";
import { PriorityDialog } from "../../queue/PriorityDialog";
import { useReservationsNow } from "../../reservations/useReservationsNow";
import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import {
  ApiFailure,
  getDevPodHistory,
  listAuditSessions,
  setQueuePriority,
  shellPath,
  sshCommand,
  sshConfig,
  startDevPodAt,
  watchDevPod,
  type DevPodDetail,
  type DevPodSample,
  type K8sEvent,
  type ProcLive,
  type SessionSummary,
} from "../../../data/api";
import { clusterSeg, segCluster } from "../../../data/cluster";
import { useClusterList } from "../../../data/clusterFilter";
import { invalidateDevPods, qk, useAuditSessions, useDevPod, useDevPodMutations, useMe, useMetrics, useNodeCeilings } from "../../../data/queries";
import { devpodStream } from "../../../data/live/streams";
import { useLiveEnabled, useLiveRow } from "../../../data/live/hooks";
import { StatusDot } from "../../../design/StatusDot";
import { Button } from "../../../design/Button";
import { CopyBlock, CopyField, copyText } from "../../../design/Copy";
import { Panel } from "../../../design/Panel";
import { EmptyState } from "../../../design/EmptyState";
import { ConfirmDialog } from "../../../design/ConfirmDialog";
import { BulkDialog, type BulkKind } from "../BulkDialog";
import { sharedWithMe } from "../columns";
import { useUrlState, type Schema } from "../../../lib/urlState";
import { useNow } from "../../../lib/useNow";
import { fmtAge } from "../../../lib/format";
import { LiveCards } from "./LiveCards";
import { ProcessTable } from "./ProcessesTab";
import { SessionList } from "./SessionsTab";
import { EventList } from "./EventsTab";
import { HistoryList } from "../../audit/History";
import { MonitorTab } from "./MonitorTab";
import { ContainerTab } from "./ContainerTab";
import { PortsPanel } from "./PortsPanel";
import { YamlTab } from "./YamlTab";
import { SnapshotControl } from "../../images/SnapshotControl";
import { devpodToConfig } from "../../configs/exportConfig";
import { useUpTarget } from "../../../lib/upLevel";

const schema = {
  tab: { kind: "string", default: "monitor" },
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
  const { cluster: seg, name = "" } = useParams();
  const cluster = segCluster(seg);
  const ceilings = useNodeCeilings();
  // the DevPod: its cluster comes from the address, not from what it says
  const r = useMemo(() => ({ cluster, name }), [cluster, name]);
  const me = useMe().data;
  const nav = useNavigate();
  const qc = useQueryClient();
  const live = useLiveEnabled();
  const [url, setUrl] = useUrlState(schema);
  // the Pod is told in the 容器 tab (old links say ?tab=pod)
  const tab = url.tab === "pod" ? "container" : url.tab;
  const now = useNow(5000);
  const q = useDevPod(r);
  const clusterList = useClusterList();
  const [detail, setDetail] = useState<DevPodDetail | null>(null);
  const [deleted, setDeleted] = useState(false);
  const [events, setEvents] = useState<Record<string, K8sEvent>>({});
  const [confirmDelete, setConfirmDelete] = useState(false);
  // the priority dialog: to start at a priority, or to change the one it waits at
  const [prioritizing, setPrioritizing] = useState<"start" | "queue" | null>(null);
  const [confirmStop, setConfirmStop] = useState(false); // a queue job's rank
  const { setRunning, patchSpec, remove, redeploy } = useDevPodMutations();
  // the list's actions that need input, for this DevPod alone
  const [bulkKind, setBulkKind] = useState<BulkKind | null>(null);

  useEffect(() => {
    setDetail(null);
    setDeleted(false);
    setEvents({});
  }, [r]);

  const onDetail = useCallback(
    (d: DevPodDetail) => {
      setDetail(d);
      qc.setQueryData(qk.devpod(r), d);
    },
    [qc, r],
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

  // One stream for the page whatever the tab: the cards above keep their
  // values and curves when the tab changes (a stream per tab emptied them
  // until its first message). The 进程 tab opens a second one beside it
  // for every process, and shows the ten of the first until that arrives.
  const spec = useMemo(() => devpodStream(r, false, onOther), [r, onOther]);
  const allSpec = useMemo(() => (tab === "processes" ? devpodStream(r, true) : null), [r, tab]);
  const metrics = useLiveRow<DevPodSample>(spec, "metrics");
  const recent = useMetrics(r, "15m"); // the cards' sparklines start from history
  const procs = useLiveRow<ProcLive[]>(spec, "processes");
  const allProcs = useLiveRow<ProcLive[]>(allSpec, "processes");
  // without live data the detail stream still carries status and events
  useEffect(() => {
    if (live) return;
    return watchDevPod(r, { onDetail, onEvent: (t, ev) => onEvent(t, ev) });
  }, [live, r, onDetail, onEvent]);

  const audit = !!me?.features.audit;
  // 概览 shows the open ones (of the newest); 会话 pages through them all, 50 at a time
  const sessions = useAuditSessions({ devpod: name, cluster }, { enabled: audit && tab === "overview", refetchInterval: 10_000 });
  const sessionPages = usePaged<SessionSummary>("devpod-sessions", { devpod: name, ...(cluster ? { cluster } : {}), limit: "50" }, listAuditSessions, {
    enabled: audit && tab === "sessions",
    refetchInterval: 10_000,
  });

  const reservations = useReservationsNow();
  const d = detail ?? q.data ?? null;
  // up a level (Cmd+↑): my list, or everyone's for someone else's DevPod
  useUpTarget(d && me && d.devpod.spec.owner !== me.user ? "/admin/devpods" : null);
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
  const pod = d.pod;
  const runningPhase = dp.status?.phase === "Running";
  // with no limit of its own: its node's, as the cards' and charts' ceiling
  const ceiling = ceilings(cluster, pod?.node || pinnedNode(dp));
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
    ["events", "事件"],
    ...(audit ? ([["history", "记录"]] as [string, string][]) : []),
    ["yaml", "YAML"],
  ];
  // the container's sshd port: never proxied, so not listed among the ports
  const sshPort = Number(dp.status?.endpoint?.split(":").pop()) || undefined;
  const clusters = clusterList.data?.items ?? [];
  const several = clusters.length >= 2;
  const clusterName = clusters.find((c) => c.name === cluster)?.displayName || cluster;
  const ssh = me ? sshCommand(me, owner, dp.metadata.name, cluster) : "";

  return (
    <div className="space-y-3 p-3">
      {dp.spec.job && (
        <Notice tone="warm">
          这是任务{" "}
          <Link to={`/queue/${clusterSeg(cluster)}/${dp.spec.job.name}`} className="mono text-accent hover:underline">
            {dp.spec.job.name}
          </Link>{" "}
          的第 {dp.spec.job.rank} 台：它随任务开始、随任务结束。休眠或删除它会让整个任务失败；要停下任务，到任务页取消它。
        </Notice>
      )}
      {reservationNotes(dp, { node: d.pod?.node, cluster }, reservations).map((n) => (
        <Notice key={n.type} tone={n.tone}>
          <span className="font-medium">{n.label}</span>
          {n.message && `：${n.message}`}
        </Notice>
      ))}
      <header className="flex flex-wrap items-center gap-2">
        <h2 className="mono text-base font-semibold">{dp.metadata.name}</h2>
        {cluster && several && (
          <span title="所在集群" className="rounded border border-line px-1.5 text-[11px] text-muted">
            {clusterName}
          </span>
        )}
        <StatusDot phase={dp.status?.phase} />
        {runningPhase && pod?.startedAt && <span className="mono text-xs text-muted">已运行 {fmtAge(pod.startedAt, now)}</span>}
        <span className="flex-1" />
        {me?.features.webshell && runningPhase && (
          <Button onClick={() => window.open(shellPath(r), "_blank", "noopener")} title="在新标签页打开终端（经 SSH 网关，会话会被记录）">
            终端
          </Button>
        )}
        {me?.features.snapshots && <SnapshotControl devpod={r} owner={owner} running={runningPhase} />}
        {me && (
          <Button onClick={() => void copyText(ssh)} title={ssh}>
            复制 SSH
          </Button>
        )}
        <Button onClick={() => nav("/configs/new", { state: { content: devpodToConfig(dp), title: dp.metadata.name } })} title="把这个 DevPod 的定义存成可复用、可分享的配置">
          存为配置
        </Button>
        <Button onClick={() => setUrl({ tab: "yaml" }, "push")}>编辑</Button>
        <Button onClick={() => setBulkKind("collaborators")} title="添加、移除或替换协作者（不会重建 DevPod）">
          协作者
        </Button>
        {!sharedWithMe({ owner }, { admin: !!me?.admin, me }) && (
          <>
            <Button onClick={() => setBulkKind("migrate")} title="换到别的节点，或取消固定节点交给调度器">
              迁移
            </Button>
            <Button onClick={() => setBulkKind("image")} title="换镜像：运行中的会用新镜像重建">
              设置镜像
            </Button>
            <Button onClick={() => setBulkKind("redeploy")} title="删除并重建 Pod：进程和 SSH 会话会中断，home 保留">
              重新部署
            </Button>
          </>
        )}
        {queueOf(dp) && !dp.spec.running ? (
          <>
            <Button onClick={() => setPrioritizing("queue")} title="排队时的优先级：数字大的先启动">
              改优先级…
            </Button>
            <Button onClick={() => setRunning.mutate({ ref: r, running: false })} title="离开队列：它不再等着启动">
              取消排队
            </Button>
          </>
        ) : (
          <>
            {!dp.spec.running && !dp.spec.job && (
              <Button onClick={() => setPrioritizing("start")} title="启动，以这个优先级在队列里等">
                按优先级启动…
              </Button>
            )}
            <Button onClick={() => (dp.spec.job && dp.spec.running ? setConfirmStop(true) : setRunning.mutate({ ref: r, running: !dp.spec.running }))}>
              {dp.spec.running ? "休眠" : "启动"}
            </Button>
          </>
        )}
        <Button variant="danger" onClick={() => setConfirmDelete(true)}>
          删除
        </Button>
      </header>
      <Provenance dp={cluster ? { ...dp, cluster } : dp} outdated={d.configOutdated} />
      <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs">
        {me?.admin && <Meta k="Owner">{owner}</Meta>}
        <Meta k="节点">{pod?.node || pinnedNode(dp)}</Meta>
        <Meta k="镜像">{image}</Meta>
        <Meta k="IP">{pod?.ip}</Meta>
        <Meta k="home">{dp.spec.persistence && `PVC ${dp.spec.persistence.size}`}</Meta>
        <Meta k="协作者">{dp.spec.collaborators?.join(", ")}</Meta>
        <Meta k="Kore">{kore}</Meta>
      </div>
      {queueOf(dp) && !dp.spec.running && (
        <div className="rounded-md bg-warm-soft px-3 py-1.5 text-xs text-warm">{phaseSummary(dp, undefined, new Date())}：轮到它时会自动启动，并分到一台合适的机器。</div>
      )}
      {dp.status?.message && !runningPhase && !queueOf(dp) && <div className="rounded-md bg-warm-soft px-3 py-1.5 text-xs text-warm">{dp.status.message}</div>}

      <LiveCards row={live ? metrics.row : undefined} hist={recent.data} ceiling={ceiling} />

      {/* on a phone the tabs are wider than the screen: the row slides */}
      <div role="tablist" className="relative flex gap-1 overflow-x-auto overflow-y-hidden border-b border-line text-xs [scrollbar-width:none]">
        {tabs.map(([id, label]) => (
          <button
            key={id}
            role="tab"
            type="button"
            aria-selected={tab === id}
            onClick={() => setUrl({ tab: id }, "push")}
            // Monaco is large: start fetching it when the YAML tab is about to be opened
            onPointerEnter={id === "yaml" ? () => void preloadYamlEditor()?.catch(() => {}) : undefined}
            onFocus={id === "yaml" ? () => void preloadYamlEditor()?.catch(() => {}) : undefined}
            className={`-mb-px shrink-0 border-b-2 px-3 py-1.5 whitespace-nowrap ${tab === id ? "border-accent font-semibold text-accent" : "border-transparent text-muted hover:text-ink"}`}
          >
            {label}
          </button>
        ))}
      </div>

      {tab === "overview" && (
        <div className="grid grid-cols-1 gap-3 lg:grid-cols-2 [&>*]:min-w-0">
          <Panel title="SSH">
            <div className="space-y-2 p-3">
              <CopyField value={ssh} />
              {me && <CopyBlock value={sshConfig(me, owner, dp.metadata.name, cluster)} />}
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
      {tab === "monitor" && <MonitorTab r={r} history={live ? metrics.row?.history : undefined} ceiling={ceiling} />}
      {tab === "processes" && (
        <Panel title="全部进程（每秒刷新）">
          <ProcessTable procs={allProcs.row?.value ?? procs.row?.value} live={live} />
        </Panel>
      )}
      {tab === "sessions" && audit && (
        <Panel title="会话（近 7 天）">
          <SessionList items={sessionPages.isLoading ? undefined : sessionPages.items} error={sessionPages.error} now={now} />
          <LoadMore auto hasMore={sessionPages.hasNextPage} busy={sessionPages.isFetchingNextPage} onClick={() => void sessionPages.fetchNextPage()} />
        </Panel>
      )}
      {tab === "ports" && me?.features.httpProxy && <PortsPanel r={r} sshPort={sshPort} />}
      {tab === "container" && <ContainerTab r={r} ports={!!me?.features.httpProxy} />}
      {tab === "yaml" && <YamlTab r={r} />}
      {tab === "events" && (
        <Panel title="事件">
          <EventList events={events} />
        </Panel>
      )}
      {tab === "history" && audit && (
        <Panel title="记录">
          <HistoryList queryKey={["devpod-history", r.cluster, r.name]} load={(cursor) => getDevPodHistory(r, cursor)} />
        </Panel>
      )}

      <BulkDialog
        key={bulkKind ?? "none"}
        kind={bulkKind}
        targets={[{ cluster: r.cluster, name: dp.metadata.name, collaborators: dp.spec.collaborators ?? [] }]}
        onClose={() => setBulkKind(null)}
        onRun={(plan) => {
          setBulkKind(null);
          if (plan.redeploy) redeploy.mutate(r);
          else patchSpec.mutate({ ref: r, body: plan.patch!({ cluster: r.cluster, name: dp.metadata.name, collaborators: dp.spec.collaborators ?? [] }, 0) });
        }}
      />
      {patchSpec.isError && <Notice>修改失败：{String(patchSpec.error)}</Notice>}
      {redeploy.isError && <Notice>重新部署失败：{String(redeploy.error)}</Notice>}
      {prioritizing && (
        <PriorityDialog
          title={prioritizing === "start" ? `按优先级启动 ${name}` : `${name} 排队的优先级`}
          current={prioritizing === "queue" ? queuePriorityOf(dp) : 0}
          cluster={cluster}
          confirmLabel={prioritizing === "start" ? "启动" : "设定"}
          onSubmit={async (priority) => {
            await (prioritizing === "start" ? startDevPodAt(r, priority) : setQueuePriority(r, priority));
            invalidateDevPods(qc, r);
          }}
          onClose={() => setPrioritizing(null)}
        />
      )}
      <ConfirmDialog
        open={confirmDelete}
        onOpenChange={setConfirmDelete}
        title={`删除 ${dp.metadata.name}？`}
        confirmLabel="确认删除"
        danger
        onConfirm={() => {
          setConfirmDelete(false);
          remove.mutate(r, { onSuccess: () => nav("/") });
        }}
      >
        Pod 和持久化数据都会被移除，不能恢复。{dp.spec.job && ` 它是任务 ${dp.spec.job.name} 的一台：删除它会让整个任务失败。`}
      </ConfirmDialog>
      <ConfirmDialog
        open={confirmStop}
        onOpenChange={setConfirmStop}
        title={`休眠 ${dp.metadata.name}？`}
        confirmLabel="仍然休眠"
        danger
        onConfirm={() => {
          setConfirmStop(false);
          setRunning.mutate({ ref: r, running: false });
        }}
      >
        它是任务 {dp.spec.job?.name} 的一台：休眠它会让整个任务失败，其他各台也会停下。
      </ConfirmDialog>
    </div>
  );
}
