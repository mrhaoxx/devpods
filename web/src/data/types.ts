// TypeScript mirrors of the webui's JSON (Go sources named per type).
// Units: CPU in cores, memory and storage in bytes, rates in bytes/s,
// GPU memory in MiB, timestamps as RFC 3339 strings.
import type { DevPod, UserQuota } from "./api";

/* live/snapshot.go */
export type GPULive = { index: number; uuid: string; name: string; util: number; memUsed: number; memTotal: number };
export type SessionLive = { user: string };
export type DevPodLive = {
  cpu: number | null;
  cpuLimit: number;
  mem: number | null;
  memLimit: number;
  gpu: GPULive[];
  netRx: number | null;
  netTx: number | null;
  netKind: string; // pod | tcp-only | ""
  rdmaRx: number | null;
  rdmaTx: number | null;
  rdmaKind: "vf" | "shared" | "";
  ioR: number | null;
  ioW: number | null;
  sessions: SessionLive[];
  idle: boolean;
};
/** api_live.go liveItem: one row of /api/live/devpods. */
export type LiveItem = DevPodLive & { name: string; owner: string };
export type LiveListFrame = { ts: string; items: LiveItem[] };

export type CardLive = { index: number; uuid: string; devpod: string; owner: string; util: number };
export type NodeLive = {
  name: string;
  ready: boolean;
  labels?: Record<string, string>;
  cpuAllocatable: number;
  cpuAllocated: number;
  cpuUsed: number;
  memAllocatable: number;
  memAllocated: number;
  memUsed: number;
  gpuModel?: string;
  gpuTotal: number;
  gpuUsed: number;
  cards?: CardLive[];
  devpods: number;
};
export type UserLive = { user: string; devpods: number; running: number; cpu: number; mem: number; gpu: number };
export type KPI = {
  cpuUsed: number;
  cpuAllocatable: number;
  memUsed: number;
  memAllocatable: number;
  gpuUsed: number;
  gpuTotal: number;
  running: number;
  pending: number;
  failed: number;
  stopped: number;
  sessions: number;
  sessionUsers: number;
  rdma: number;
};
export type AttentionItem = {
  kind: string; // alert | idle_gpu | pending_long | quota_high (more arrive with reservations)
  uid?: string;
  devpod?: string;
  owner?: string;
  since: string;
  detail?: string;
};
export type OverviewFrame = { ts: string; kpi: KPI; nodes: NodeLive[]; users: UserLive[]; attention: AttentionItem[] };

export type ProcLive = {
  pid: number;
  start: number;
  exe: string;
  cmdline: string;
  cpu: number;
  rss: number;
  ioR: number;
  ioW: number;
  tcpRx: number;
  tcpTx: number;
  gpu?: number[];
  user?: string;
  how?: string;
};

/* telemetry/wire/wire.go */
export type WireContainer = {
  ts: string;
  cpu: number;
  cpu_limit: number;
  throttled_usec: number;
  cpuset?: string;
  mem_ws: number;
  mem_anon: number;
  mem_file: number;
  mem_shmem: number;
  mem_limit: number;
  oom_kills: number;
  io_r: number;
  io_w: number;
  io_r_iops: number;
  io_w_iops: number;
  net_rx: number;
  net_tx: number;
  net_tcp_rx: number;
  net_tcp_tx: number;
  net_other_rx: number;
  net_other_tx: number;
  net_kind: string;
  pids: number;
};
export type WireGPU = { ts: string; index: number; uuid: string; name: string; util: number; mem_used: number; mem_total: number; power: number; temp: number };
export type WireRDMA = {
  ts: string;
  ibdev: string;
  port: number;
  tx_bps: number;
  rx_bps: number;
  tx_pps: number;
  rx_pps: number;
  health?: Record<string, number>;
  scope: string; // vf_exclusive | pf_shared
};
/** live/state.go DevPodSample: the stream's "metrics" event. */
export type DevPodSample = { ts: string; container: WireContainer; gpus: WireGPU[] | null; rdma: WireRDMA[] | null };

export type WireInfo = {
  ts: string;
  os?: string;
  kernel?: string;
  cpu_limit: number;
  cpuset?: string;
  mem_limit: number;
  gpus?: { index: number; uuid: string; name: string; mem_total: number; driver?: string; cuda?: string }[];
  rdma?: { ibdev: string; port: number; scope: string }[];
  shell?: string;
  agent_version?: string;
  collectors?: Record<string, string>;
};
export type WireSnapshot = {
  ts: string;
  procs: number;
  threads: number;
  zombies: number;
  listen?: { port: number; proto: string; addr?: string; pid?: number }[];
  storage?: { mount: string; size: number; used: number }[];
};
/** api_metrics.go handleDevPodInfo. */
export type ContainerInfo = { info: WireInfo; infoTs: string; snapshot?: WireSnapshot; snapshotTs?: string };

/* audit/store/telemetry_query.go */
export type MetricRange = "15m" | "1h" | "24h" | "7d";
export type MetricSeries = { step: number; ts: number[]; series: Record<string, (number | null)[]> };

/* api_nodes.go */
export type ResView = { allocatable: number; allocated: number; used?: number };
export type NodeView = {
  name: string;
  ready: boolean;
  labels: Record<string, string>;
  cpu: ResView;
  mem: ResView;
  gpu: { model?: string; total: number; used: number; cards: CardLive[] };
  rdma: { resource: string; allocatable: number }[];
  devpods: number;
};
export type NodeDevPod = { name: string; owner: string; phase: string; cpu: number | null; mem: number | null; gpu: number };
export type NodeDetail = { node: NodeView; devpods: NodeDevPod[]; live: boolean; topology?: unknown };

/* api_devpods.go PATCH */
export type SpecPatch = {
  running?: boolean;
  image?: string;
  cpu?: string;
  memory?: string;
  gpu?: string;
  shell?: string;
  collaborators?: string[];
};
export type PatchResult = { devpod: DevPod; rebuild: boolean };

/* api_admin_users*.go */
export type QuotaForm = { maxDevPods?: number | null; cpu?: string; memory?: string; gpu?: string; storage?: string };
export type AdminUserRow = {
  name: string;
  displayName?: string;
  admin: boolean;
  hasPassword: boolean;
  devpods: number;
  running: number;
  usage: { cpu?: string; memory?: string; gpu?: string; storage?: string };
  quota?: UserQuota;
  liveUsage?: { cpu: number; mem: number; gpu: number; devpodsRunning: number };
  lastSession?: string;
  source: "oidc" | "password" | "ldap";
  customPolicy: boolean;
  pubkeys: number;
};
export type BatchOp = "setQuota" | "setPolicy" | "resetPassword" | "hibernateAll";
export type BatchRequest = { names: string[]; op: BatchOp; args?: { quota?: QuotaForm; policy?: unknown } };
export type BatchResult = { name: string; ok: boolean; error?: string; password?: string; hibernated?: number };
export type ImportResult = { line: number; username: string; displayName?: string; ok: boolean; error?: string; password?: string };

/* api_search.go */
export type SearchResult = {
  users: { name: string; displayName?: string }[];
  devpods: { name: string; owner: string; phase?: string }[];
  sessions: { id: string; user: string; devpod: string; openedAt: string }[];
  commands: { sessionId: string; ts: string; user: string; devpod: string; cmd: string }[];
  auditError?: string;
};
