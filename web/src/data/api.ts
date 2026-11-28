import { apiIn, type Ref } from "./cluster";

export interface ApiError {
  code: string;
  message: string;
  detail?: unknown;
}

export class ApiFailure extends Error {
  constructor(
    public status: number,
    public body: ApiError,
  ) {
    super(body.message);
  }
  get code(): string {
    return this.body.code;
  }
}

export type ReqOptions = { raw?: { body: BodyInit; contentType: string } };

/** req calls the API; non-2xx answers throw ApiFailure (a body that is
 *  not JSON — a proxy error page — becomes code HTTP_<status>). */
export async function req<T>(method: string, path: string, body?: unknown, opts: ReqOptions = {}): Promise<T> {
  const resp = await fetch(path, {
    method,
    headers: opts.raw ? { "Content-Type": opts.raw.contentType } : body !== undefined ? { "Content-Type": "application/json" } : undefined,
    body: opts.raw ? opts.raw.body : body !== undefined ? JSON.stringify(body) : undefined,
  });
  if (resp.status === 401 && !path.startsWith("/api/auth/")) {
    window.location.href = "/login";
    throw new ApiFailure(401, { code: "UNAUTHORIZED", message: "not logged in" });
  }
  if (!resp.ok) {
    let err: ApiError;
    try {
      err = (await resp.json()) as ApiError;
      if (!err || typeof err.code !== "string") throw new Error("no code");
    } catch {
      err = { code: `HTTP_${resp.status}`, message: `${resp.status} ${resp.statusText}`.trim() };
    }
    throw new ApiFailure(resp.status, err);
  }
  if (resp.status === 204) return undefined as T;
  return (await resp.json()) as T;
}

export interface Me {
  user: string;
  admin: boolean;
  /** management permissions the user may hold somewhere (grants): what pages to show; each action is judged itself */
  permissions?: { setReservable: boolean; viewAudit: boolean; disconnectSessions: boolean; manageReservations?: boolean };
  nameBudget: number;
  quota: { maxDevPods?: number; compute?: Record<string, string>; storage?: string };
  usage: { devpods: number; running: number; compute: Record<string, string>; storage: string };
  /** the kinds of GPU here, the default first */
  gpuTypes?: { resource: string; label: string }[];
  features: { pubkeySelfService: boolean; kore: boolean; passwordAuth: boolean; audit?: boolean; telemetry?: boolean; reservations?: boolean; clusters?: boolean; webshell?: boolean; httpProxy?: boolean; snapshots?: boolean; snapshotDefaultRegistry?: boolean; queue?: boolean };
  hasPassword: boolean;
  ssh: { host: string; port: number; loginSuffix?: string };
  /** the DevPod CLI's SSH login name (ssh <user>+<cli>@gateway), while it is on */
  cli?: string;
  /** per cluster, in a console of several */
  clusters?: Record<string, { quota: Me["quota"]; usage: Me["usage"]; ssh?: Me["ssh"] }>;
}

// sshUser builds the login string: "<owner>+<pod>" plus an optional
// deployment-configured suffix (e.g. "+hpc101" for a proxy route).
/** sshOf: the gateway serving a cluster (the console's own without one). */
export const sshOf = (me: Me | undefined, cluster?: string) => (cluster && me?.clusters?.[cluster]?.ssh) || me?.ssh;
/** meIn: my quota and usage in a cluster (all of them without one). */
export const meIn = (me: Me | undefined, cluster?: string) => (cluster && me?.clusters?.[cluster]) || (me ? { quota: me.quota, usage: me.usage } : undefined);

/** sshUser: the login for a DevPod, given its whole name. The owner's is
 *  <owner>+<the rest of the name after "<owner>-">; anyone else (a
 *  collaborator) logs in as themselves with the whole name, by which the
 *  gateway finds it. */
export function sshUser(me: Me | undefined, owner: string, name: string, cluster?: string): string {
  const suffix = sshOf(me, cluster)?.loginSuffix;
  const login = !me || me.user === owner ? `${owner}+${name.startsWith(owner + "-") ? name.slice(owner.length + 1) : name}` : `${me.user}+${name}`;
  return `${login}${suffix ? `+${suffix}` : ""}`;
}

export type AuthConfig = { password: boolean; oauth: boolean };
export type UserQuota = {
  maxDevPods?: number;
  compute?: Record<string, string>;
  storage?: string;
};
export type AdminUser = {
  name: string;
  displayName?: string;
  admin: boolean;
  hasPassword: boolean;
  devpods: number;
  running: number;
  usage: { cpu?: string; memory?: string; gpu?: string; storage?: string };
  quota?: UserQuota;
};
export type QuotaPatch = { maxDevPods?: number | null; cpu?: string; memory?: string; gpu?: string; storage?: string };
export type AdminDevPod = {
  name: string;
  owner: string;
  phase: string;
  running: boolean;
  cpu?: string;
  memory?: string;
  storage?: string;
};

export const authConfig = () => req<AuthConfig>("GET", "/api/auth/config");
export const passwordLogin = (username: string, password: string) =>
  req<{ user: string }>("POST", "/api/auth/password", { username, password });
export const logout = () => req<void>("POST", "/auth/logout");
export const changePassword = (oldPassword: string, newPassword: string) =>
  req<void>("PUT", "/api/me/password", { oldPassword, newPassword });
export const listUsers = () => req<{ items: AdminUser[]; defaultQuota: UserQuota }>("GET", "/api/admin/users");
export const createUser = (username: string, displayName: string, password: string) =>
  req<AdminUser>("POST", "/api/admin/users", { username, displayName, password });
export const resetUserPassword = (name: string, password: string) =>
  req<void>("PATCH", `/api/admin/users/${name}`, { password });
export const setUserQuota = (name: string, quota: QuotaPatch) =>
  req<void>("PATCH", `/api/admin/users/${name}`, { quota });
/** setUserClusterQuota: the user's quota in one cluster (a console of several). */
export const setUserClusterQuota = (name: string, cluster: string, quota: QuotaPatch) =>
  req<void>("PATCH", `/api/admin/users/${name}`, { quotas: { [cluster]: quota } });
export const deleteUser = (name: string) => req<void>("DELETE", `/api/admin/users/${name}`);
export const listAllDevPods = () =>
  req<{ items: AdminDevPod[]; devpods: DevPod[]; pods: Record<string, PodInfo> }>("GET", "/api/admin/devpods");

export type KoreZone = {
  id: number;
  cpus: string;
  freeCpus: string;
  smtSiblings?: number[][];
  memory?: string;
  devices?: { name: string; type: string }[];
};
export type KoreAlloc = { pod: string; container: string; cpuset: string; devpod?: string };
export type KorePool = { name: string; cpuset: string; numa?: number[]; members?: string[] };
export type KoreNode = {
  node: string;
  reservedCpus?: string;
  zones: KoreZone[];
  allocations: KoreAlloc[];
  pools: KorePool[];
};
export const koreTopology = () => req<{ nodes: KoreNode[] }>("GET", "/api/kore/topology");
export const devpodTopology = (r: Ref) =>
  req<{ node: KoreNode | null; cpuset?: string }>("GET", apiIn(r.cluster, `/api/devpods/${r.name}/topology`));

// parseCpuList expands "8-15,40-47" into a set of core numbers.
export function parseCpuList(s?: string): Set<number> {
  const set = new Set<number>();
  if (!s) return set;
  for (const part of s.split(",")) {
    if (!part) continue;
    const [a, b] = part.split("-").map(Number);
    for (let i = a; i <= (b ?? a); i++) set.add(i);
  }
  return set;
}

// sshCommand renders the copy-pastable login line for a DevPod (its whole
// name) using the deployment's advertised gateway address (-p only when
// non-22).
export function sshCommand(me: Me | undefined, owner: string, name: string, cluster?: string): string {
  const ssh = sshOf(me, cluster);
  const host = ssh?.host || "<gateway>";
  const port = ssh?.port ?? 22;
  const flag = host !== "<gateway>" && port !== 22 ? `-p ${port} ` : "";
  return `ssh ${flag}${sshUser(me, owner, name, cluster)}@${host}`;
}

// sshConfig renders a ~/.ssh/config block so the user can shortcut the
// full login to `ssh <alias>`. The alias is the DevPod's name.
export function sshConfig(me: Me | undefined, owner: string, name: string, cluster?: string): string {
  const ssh = sshOf(me, cluster);
  const host = ssh?.host || "<gateway>";
  const port = ssh?.port ?? 22;
  return `Host ${name}\n    HostName ${host}\n    Port ${port}\n    User ${sshUser(me, owner, name, cluster)}`;
}

export type K8sEvent = {
  metadata: { uid: string };
  reason?: string;
  message?: string;
  type?: string; // Normal | Warning
  count?: number;
  lastTimestamp?: string;
};

// DevPod objects are passed through as loosely-typed JSON; the UI
// reads a handful of paths and must tolerate schema growth.
export type ContainerSpec = { name: string; image?: string; resources?: { limits?: Record<string, string> } };
export type DevPod = {
  /** the cluster it is in, in a console of several (lists say it; watch events do not) */
  cluster?: string;
  metadata: { name: string; uid?: string; creationTimestamp?: string; labels?: Record<string, string>; annotations?: Record<string, string> };
  spec: {
    owner: string;
    running: boolean;
    shell?: string;
    collaborators?: string[];
    persistence?: { size: string; mountPath?: string };
    /** set on a job's rank: the job and which rank */
    job?: { name: string; rank: number };
    pod?: { spec?: { containers?: ContainerSpec[]; nodeSelector?: Record<string, string> } };
  };
  status?: {
    phase?: string;
    endpoint?: string;
    message?: string;
    hibernatedAt?: string;
    retryCount?: number;
    lastFailureAt?: string;
    conditions?: { type: string; status: string; message?: string }[];
  };
};

// PodInfo is the live Pod state attached to list/watch/detail payloads.
export type PodInfo = { node: string; ip: string; startedAt?: string };
export type Presence = { count: number; users: string[] };

export type BindingInfo = {
  allocatedCpuset?: string;
  reservedNuma?: string;
  pool?: string;
  poolSize?: string;
};

/** me: who I am; in a console of several clusters, per cluster too (meIn, sshOf). */
export const me = () => req<Me>("GET", "/api/me");
export type DevPodList = { items: DevPod[]; pods: Record<string, PodInfo> };
/** listDevPods: mine, everyone's (admins), or mine and those shared with me (the 我的 DevPod list). */
export const listDevPods = (scope: ListScope = "mine") =>
  req<DevPodList>("GET", scope === "all" ? "/api/devpods?scope=all" : scope === "shared" ? "/api/devpods?shared=1" : "/api/devpods");
export type ListScope = "mine" | "all" | "shared";
export const auditPresence = (all = false) =>
  req<{ items: Record<string, Presence> }>("GET", `/api/audit/presence${all ? "?scope=all" : ""}`);
export const getDevPod = (r: Ref) => req<DevPodDetail>("GET", apiIn(r.cluster, `/api/devpods/${r.name}`));
export const createDevPod = (cluster: string | undefined, body: unknown) => req<DevPod>("POST", apiIn(cluster, "/api/devpods"), body);
export const patchDevPod = (r: Ref, running: boolean) => req<PatchResult>("PATCH", apiIn(r.cluster, `/api/devpods/${r.name}`), { running });
/** startDevPodAt: start a DevPod, waiting in the queue's line at priority. */
export const startDevPodAt = (r: Ref, priority: number) => req<PatchResult>("PATCH", apiIn(r.cluster, `/api/devpods/${r.name}`), { running: true, priority });
/** setQueuePriority: the priority of a DevPod waiting in line. */
export const setQueuePriority = (r: Ref, priority: number) => req<PatchResult>("PATCH", apiIn(r.cluster, `/api/devpods/${r.name}`), { priority });
export const deleteDevPod = (r: Ref) => req<void>("DELETE", apiIn(r.cluster, `/api/devpods/${r.name}`));
export const getEvents = (r: Ref) => req<{ items: unknown[] }>("GET", apiIn(r.cluster, `/api/devpods/${r.name}/events`));
export const getPubkeys = () => req<{ pubkeys: string[] | null }>("GET", "/api/me/pubkeys");
export const putPubkeys = (pubkeys: string[]) => req<{ pubkeys: string[] }>("PUT", "/api/me/pubkeys", { pubkeys });

export { watchDevPods, watchDevPod } from "./sse";
/** configOutdated: the config it came from has changed since (only when I may see that config) */
/** refOf: a DevPod as a Ref (its cluster and name). */
export const refOf = (dp: DevPod): Ref => ({ cluster: dp.cluster, name: dp.metadata.name });
/** podKey: where a list's pods map holds a DevPod's Pod — <cluster>/<name> in a console of several clusters. */
export const podKey = (dp: DevPod): string => (dp.cluster ? `${dp.cluster}/${dp.metadata.name}` : dp.metadata.name);

export type DevPodDetail = { devpod: DevPod; binding?: BindingInfo; pod?: PodInfo; configOutdated?: boolean };

/* ─── Audit ───────────────────────────────────────────────────────── */
export type SessionRow = {
  session_id: string;
  user: string;
  devpod: string;
  devpod_uid: string;
  owner: string;
  gateway_pod: string;
  client_ip: string;
  pubkey_fp: string;
  /** the key itself, an authorized_keys line (absent in a session of before keys were kept) */
  pubkey?: string;
  auth_kind: string;
  proxy_alias: string;
  backend_local_addr: string;
  opened_at: string;
  closed_at: string | null;
  close_reason: string;
  bytes_in: number;
  bytes_out: number;
  policy_snapshot: string;
  gaps: number;
};
/** cluster, gateway: of a session through a gateway of several clusters */
export type SessionSummary = SessionRow & {
  commands: number;
  files: number;
  policy_hits: number;
  alerts: number;
  cluster?: string;
  gateway?: string;
  /** the comment of the user's key with the session's fingerprint, as the user's keys are now */
  pubkey_name?: string;
};
export type ChannelRow = {
  session_id: string;
  channel_id: number;
  type: string;
  command: string;
  subsystem: string;
  target: string;
  pty: boolean;
  term: string;
  opened_at: string;
  closed_at: string | null;
  exit_status: number | null;
  bytes_in: number;
  bytes_out: number;
  raw_recorded: boolean;
};
export type CommandRow = {
  /** its DevPod's cluster, in a console of several */
  cluster?: string;
  event_id: string;
  ts: string;
  session_id: string;
  user: string;
  source: string;
  channel_id: number;
  cwd: string;
  cmdline: string;
  exit_code: number | null;
  duration_ms: number;
  attribution: string;
};
export type FileEventRow = {
  cluster?: string;
  event_id: string;
  ts: string;
  session_id: string;
  channel_id: number;
  user: string;
  protocol: string;
  op: string;
  path: string;
  path2: string;
  size: number;
  sha256: string;
  complete: boolean;
  blocked: boolean;
  s3_key: string;
};
// One request to a DevPod port through a gateway's HTTP proxy (one
// WebSocket connection); user "" is anonymous, query is as sent.
export type HTTPRequestRow = {
  ts: string;
  duration_ms: number;
  cluster?: string;
  gateway?: string;
  gateway_pod: string;
  devpod: string;
  devpod_uid: string;
  owner: string;
  port: number;
  access: string;
  user: string;
  method: string;
  host: string;
  path: string;
  query: string;
  status: number;
  bytes_in: number;
  bytes_out: number;
  websocket: boolean;
  client_ip: string;
  forwarded_for: string;
  user_agent: string;
  request_id: string;
};
export type PolicyEventRow = { ts: string; session_id: string; user: string; rule: string; action: string; detail: string };
export type AlertRow = { cluster?: string; ts: string; devpod_uid: string; session_id: string; kind: string; source: string; detail: string };
export type AccessLogRow = { ts: string; viewer: string; action: string; target_session: string; target_file: string; client_ip: string };
export type SessionDetail = {
  session: SessionRow;
  /** the comment of the user's key with the session's fingerprint, as the user's keys are now */
  pubkey_name?: string;
  /** the key as an authorized_keys line: recorded, else the user's key with that fingerprint */
  pubkey_line?: string;
  cluster?: string;
  gateway?: string;
  channels: ChannelRow[] | null;
  commands: CommandRow[] | null;
  files: FileEventRow[] | null;
  policy: PolicyEventRow[] | null;
  alerts: AlertRow[] | null;
};
export type Page<T> = { items: T[]; next: string };

const qs = (q: Record<string, string | undefined>) =>
  new URLSearchParams(Object.entries(q).filter(([, v]) => v) as [string, string][]).toString();

export const listAuditSessions = (q: Record<string, string | undefined>) =>
  req<Page<SessionSummary>>("GET", `/api/audit/sessions?${qs(q)}`);
export const getAuditSession = (id: string) => req<SessionDetail>("GET", `/api/audit/sessions/${id}`);
export const searchCommands = (q: Record<string, string | undefined>) => req<Page<CommandRow>>("GET", `/api/audit/commands?${qs(q)}`);
export const searchFiles = (q: Record<string, string | undefined>) => req<Page<FileEventRow>>("GET", `/api/audit/files?${qs(q)}`);
export const searchHTTP = (q: Record<string, string | undefined>) => req<Page<HTTPRequestRow>>("GET", `/api/audit/http?${qs(q)}`);

/** AuditEvent: one thing done to a DevPod, a job or a machine (the audit store's events). */
export type AuditEvent = {
  ts: string;
  cluster?: string;
  kind: string; // devpod | job | machine
  object: string;
  object_uid?: string;
  owner?: string;
  actor: string; // a user, or system:<what>
  via: string; // web | cli | dp | token:<id> | system
  action: string;
  node?: string;
  reason?: string;
  detail?: string;
  origin?: string; // the DevPod a dp call came from
  client_ip?: string;
  request_id?: string;
};
export const searchEvents = (q: Record<string, string | undefined>) => req<Page<AuditEvent>>("GET", `/api/audit/events?${qs(q)}`);
export const getDevPodHistory = (r: Ref, cursor?: string) => req<Page<AuditEvent>>("GET", apiIn(r.cluster, `/api/devpods/${r.name}/history${cursor ? `?${qs({ cursor })}` : ""}`));
export const getJobHistory = (id: string, cursor?: string) => req<Page<AuditEvent>>("GET", `/api/jobs/${id}/history${cursor ? `?${qs({ cursor })}` : ""}`);
export const listAlerts = (q: Record<string, string | undefined>) => req<Page<AlertRow>>("GET", `/api/audit/alerts?${qs(q)}`);
export const listAccessLog = (q: Record<string, string | undefined>) => req<Page<AccessLogRow>>("GET", `/api/audit/access-log?${qs(q)}`);
export const castURL = (id: string, ch: number) => `/api/audit/sessions/${id}/channels/${ch}/cast`;
/** liveURL: an open session's channel from its start, then live (audit spec §16). */
export const liveURL = (id: string, ch: number) => `/api/audit/sessions/${id}/channels/${ch}/live`;
export const rawURL = (id: string, ch: number, stream: "in" | "out" | "err") =>
  `/api/audit/sessions/${id}/channels/${ch}/raw?stream=${stream}`;
/** disconnectSession: an admin cuts an open SSH session (audit spec §17). */
export const disconnectSession = (id: string) => req<void>("POST", `/api/audit/sessions/${id}/disconnect`);
export const fileContentURL = (eventId: string) => `/api/audit/files/${eventId}/content`;
export const getAuditPolicyDefault = () => req<AuditPolicy>("GET", "/api/admin/audit/policy-default");

export type PolicyAction = "Allow" | "Deny";
export type AuditPolicy = {
  download?: PolicyAction;
  upload?: PolicyAction;
  maxFileSize?: string;
  forwarding?: { local?: PolicyAction; remote?: PolicyAction; recordContent?: boolean };
  agentForwarding?: PolicyAction;
  x11Forwarding?: PolicyAction;
  failMode?: "Open" | "Closed";
};
export type EffectivePolicy = Record<string, boolean | number>;
export const getUserAuditPolicy = (name: string) =>
  req<{ default: AuditPolicy; user: AuditPolicy | null; effective: EffectivePolicy; warnings: string[] }>(
    "GET",
    `/api/admin/users/${name}/audit-policy`,
  );
export const putUserAuditPolicy = (name: string, p: AuditPolicy | null) =>
  req<AuditPolicy | null>("PUT", `/api/admin/users/${name}/audit-policy`, p ?? {}); // {} = inherit every field

/* ─── Console UI additions ─────────────────────────────────────────── */
export * from "./types";
import type {
  AdminUserRow,
  BatchRequest,
  BatchResult,
  ContainerInfo,
  ImportResult,
  MetricRange,
  MetricSeries,
  NodeDetail,
  NodeView,
  PatchResult,
  RecentFrame,
  SearchResult,
  SpecPatch,
} from "./types";

export const patchDevPodSpec = (r: Ref, body: SpecPatch) => req<PatchResult>("PATCH", apiIn(r.cluster, `/api/devpods/${r.name}`), body);
/** redeployDevPod recreates the Pod (sessions drop, the home volume stays). */
export const redeployDevPod = (r: Ref) => req<void>("POST", apiIn(r.cluster, `/api/devpods/${r.name}/redeploy`));
/** rebuildFromConfig: the DevPod from the config it came from, as that config is now. */
export function rebuildFromConfig(r: Ref, dryRun: true): Promise<{ yaml: string; rebuild: boolean }>;
export function rebuildFromConfig(r: Ref, dryRun?: false): Promise<PatchResult>;
export function rebuildFromConfig(r: Ref, dryRun = false) {
  return req("POST", apiIn(r.cluster, `/api/devpods/${r.name}/rebuild-from-config`), dryRun ? { dryRun: true } : {});
}
export const getMetrics = (r: Ref, range: MetricRange) =>
  req<MetricSeries>("GET", apiIn(r.cluster, `/api/devpods/${r.name}/metrics?range=${range}`));
export const getRecent = (p: { scope?: "mine" | "all"; ref?: Ref }) =>
  req<RecentFrame>("GET", apiIn(p.ref?.cluster, `/api/live/devpods/recent?${p.ref ? `name=${encodeURIComponent(p.ref.name)}` : (p.scope ?? "mine") === "mine" ? "scope=mine&shared=1" : `scope=${p.scope}`}`));
export const getInfo = (r: Ref) => req<ContainerInfo>("GET", apiIn(r.cluster, `/api/devpods/${r.name}/info`));
export const listNodes = () => req<{ items: NodeView[]; live: boolean }>("GET", "/api/admin/nodes");
/** Each node's CPU / memory use over the sparkline window, from ClickHouse. */
export const getNodesRecent = () => req<RecentFrame>("GET", "/api/admin/nodes/recent");
export const getNode = (r: Ref) => req<NodeDetail>("GET", apiIn(r.cluster, `/api/admin/nodes/${r.name}`));
export const listAdminUsers = () => req<{ items: AdminUserRow[]; defaultQuota: UserQuota }>("GET", "/api/admin/users");
export const usersBatch = (body: BatchRequest) => req<{ results: BatchResult[] }>("POST", "/api/admin/users:batch", body);
export const importUsers = (csv: string, dryRun: boolean) =>
  req<{ rows: ImportResult[]; created: number }>("POST", `/api/admin/users:import${dryRun ? "?dryRun=1" : ""}`, undefined, {
    raw: { body: csv, contentType: "text/csv" },
  });
export const setUserAdmin = (name: string, admin: boolean) => req<void>("PATCH", `/api/admin/users/${name}`, { admin });
export const search = (q: string) => req<SearchResult>("GET", `/api/search?q=${encodeURIComponent(q)}`);

/** shellPath: the web terminal page of a DevPod (opened in its own tab). */
export const shellPath = (r: Ref) => `/devpods/${r.cluster || "-"}/${r.name}/shell`;
/** shellSocketURL: the terminal WebSocket (UI spec §5.12). */
export const shellSocketURL = (r: Ref, cols: number, rows: number, loc: Pick<Location, "protocol" | "host"> = window.location) =>
  `${loc.protocol === "https:" ? "wss" : "ws"}://${loc.host}${apiIn(r.cluster, `/api/devpods/${r.name}/shell`)}?cols=${cols}&rows=${rows}`;
/** probeShell asks why a terminal cannot be opened (a failed WebSocket handshake hides its status). */
export const probeShell = (r: Ref) => req<void>("GET", apiIn(r.cluster, `/api/devpods/${r.name}/shell?probe=1`));

/* api_http.go: DevPod ports published through the gateway's HTTP proxy */
/** publicPorts: anyone with the link; userPorts: anyone signed in to the console; the rest: owner, collaborators, admins. */
export type HttpView = { id: string; urlTemplate: string; publicPorts: number[]; userPorts: number[]; /** reached over TLS */ httpsPorts?: number[] };
export const getHttp = (r: Ref) => req<HttpView>("GET", apiIn(r.cluster, `/api/devpods/${r.name}/http`));

/** The Pod behind a DevPod, as `kubectl describe pod` tells it. */
export type PodContainer = {
  name: string;
  image: string;
  state?: string;
  tone?: "ok" | "wait" | "fail" | "";
  lastState?: string;
  ready: boolean;
  restarts: number;
  requests?: Record<string, string>;
  limits?: Record<string, string>;
  mounts?: string[];
};
export type PodDetail = {
  name: string;
  phase: string;
  reason?: string;
  message?: string;
  ready: string;
  restarts: number;
  qos?: string;
  node?: string;
  hostIP?: string;
  podIP?: string;
  createdAt?: string;
  startedAt?: string;
  deletingSince?: string;
  restartPolicy?: string;
  serviceAccount?: string;
  gracePeriodSeconds?: number;
  hostNetwork?: boolean;
  conditions: { type: string; status: string; reason?: string; message?: string; since?: string }[];
  initContainers: PodContainer[];
  containers: PodContainer[];
  volumes: { name: string; kind: string; detail?: string }[];
  labels: Record<string, string>;
  nodeSelector: Record<string, string>;
  tolerations: string[];
};
export const getDevPodPod = (r: Ref) => req<{ pod: PodDetail | null }>("GET", apiIn(r.cluster, `/api/devpods/${r.name}/pod`));
/** A DevPod's manifest as YAML, and saving it back (the spec is replaced). */
export const getDevPodYaml = (r: Ref) => req<{ yaml: string; running: boolean }>("GET", apiIn(r.cluster, `/api/devpods/${r.name}/yaml`));
export const putDevPodYaml = (r: Ref, yaml: string) => req<{ devpod: DevPod; rebuild: boolean }>("PUT", apiIn(r.cluster, `/api/devpods/${r.name}/yaml`), { yaml });
export const putHttp = (r: Ref, body: { publicPorts?: number[]; userPorts?: number[]; httpsPorts?: number[]; reset?: boolean }) => req<HttpView>("PUT", apiIn(r.cluster, `/api/devpods/${r.name}/http`), body);
/** httpTicket: the URL that signs this browser in on a private port's hostname. */
export const httpTicket = (host: string, rd: string) => req<{ url: string }>("POST", "/api/http/ticket", { host, rd });

/* tokens.go: personal access tokens */
export type TokenScope = { id: string; group: string; label: string; admin?: boolean };
export type TokenView = { id: string; name: string; scopes: string[]; devpods: string[]; clusters?: string[]; createdAt: string; expiresAt: string | null; expired: boolean };
export const listTokens = () => req<{ items: TokenView[]; scopes: TokenScope[] }>("GET", "/api/me/tokens");
export const createToken = (body: { name: string; scopes: string[]; devpods: string[]; expiresInDays: number; clusters?: string[] }) =>
  req<{ token: string; info: TokenView }>("POST", "/api/me/tokens", body);
export const deleteToken = (id: string) => req<void>("DELETE", `/api/me/tokens/${id}`);

/* api_reservations.go: node choice and node reservations */
export type NodeChoice = {
  cluster?: string;
  name: string;
  ready: boolean;
  cpu: { allocatable: number; allocated: number };
  mem: { allocatable: number; allocated: number };
  gpuModel?: string;
  gpuTotal: number;
  gpuUsed: number;
  reservable: boolean;
  /** I may run others' reservations here (an admin, or a 管理预约 grant) */
  manageReservations?: boolean;
  /** the access rules let me use it only under a reservation */
  reservationRequired?: boolean;
  /** what it is to me: usable, needs a reservation I do not hold now, or barred */
  access: "ok" | "reservation" | "denied";
  pending: number;
  /** my reservation of it that covers now */
  mine: { user: string; until: string } | null;
  /** my DevPods may be pinned here now */
  selectable: boolean;
  reason?: string;
};
export const listNodeChoices = () => req<{ items: NodeChoice[] }>("GET", "/api/nodes");
export type Reservation = {
  cluster?: string;
  id: string;
  node: string;
  user: string;
  start: string;
  end: string;
  phase: "Upcoming" | "Active" | "Ended" | "Cancelled";
  mine: boolean;
  cancelledBy?: string;
  /** what it is for, as its holder says */
  purpose?: string;
  /** I may change or end it: mine, an admin, or a 管理预约 grant on its machine */
  canManage?: boolean;
  /** the cards and cores it holds; both absent: the whole machine */
  gpu?: number;
  cpu?: number;
  /** it does not fit beside the machine's other reservations (made before reservations held shares) */
  overbooked?: boolean;
  /** not a reservation: a queue job's rank, drawn on the timeline (what it holds, until when at most) */
  job?: { id: string; title?: string; preemptible?: boolean };
};
/** a running queue job's rank on a machine, as the timeline draws it */
export type JobHold = { cluster?: string; id: string; title?: string; owner: string; mine: boolean; rank: number; node: string; gpu: number; cpu: number; start: string; end: string; preemptible?: boolean };
export type ReservationRules = { granularitySeconds: number; maxDurationSeconds: number; maxAdvanceSeconds: number; clearNoticeSeconds: number; timezone: string; now: string };
export const getReservationRules = () => req<ReservationRules>("GET", "/api/reservations/rules");
export const listReservations = (from: string, to: string) => req<{ items: Reservation[]; jobs?: JobHold[] }>("GET", `/api/reservations?from=${encodeURIComponent(from)}&to=${encodeURIComponent(to)}`);
/** listMyReservations: a page of mine, of any time, the latest start first; `next` asks for the page after. */
export const listMyReservations = (cursor?: string, limit = 50) =>
  req<{ items: Reservation[]; next?: string }>("GET", `/api/reservations?mine=1&limit=${limit}${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`);
type Period = {
  node: string;
  start: string;
  end: string;
  purpose?: string;
  /** for someone else (an admin, or a 管理预约 grant) */
  user?: string;
  /** part of the machine: cards (with their share of the cores unless cpu says) and cores; neither: all of it */
  gpu?: number;
  cpu?: number;
};
/** ResRef names a reservation: its id and cluster. */
export type ResRef = { id: string; cluster?: string };
export const createReservation = (cluster: string | undefined, p: Period) => req<Reservation>("POST", apiIn(cluster, "/api/reservations"), p);
export const patchReservation = (r: ResRef, body: { start?: string; end?: string; purpose?: string; gpu?: number; cpu?: number; whole?: boolean }) => req<Reservation>("PATCH", apiIn(r.cluster, `/api/reservations/${r.id}`), body);
export const deleteReservation = (r: ResRef) => req<void>("DELETE", apiIn(r.cluster, `/api/reservations/${r.id}`));
/** a machine one may switch reservable (a grant, or an admin) */
export type ManagedNode = { cluster?: string; name: string; reservable: boolean; ready: boolean; gpuModel?: string; gpuTotal: number };
export const listManagedNodes = () => req<{ items: ManagedNode[] }>("GET", "/api/nodes/managed");
export const setReservable = (r: Ref, reservable: boolean) =>
  req<{ reservable: boolean; pending: number }>("PUT", apiIn(r.cluster, `/api/admin/nodes/${r.name}/reservable`), { reservable });

/* api_snapshots.go: saving a DevPod as an image (snapshot spec v2) */
export type SnapshotView = {
  cluster?: string;
  id: string;
  devpod: string;
  owner: string;
  image: string;
  phase: "Pending" | "Running" | "Succeeded" | "Failed";
  digest: string;
  sizeBytes: number;
  message: string;
  customRegistry: boolean;
  createdAt: string;
  startedAt: string | null;
  completedAt: string | null;
};
export type SnapshotProgress = {
  phase: SnapshotView["phase"];
  stage: "pending" | "commit" | "push" | "done" | "error";
  elapsedSeconds: number;
  sizeBytes?: number;
  /** upload progress over the layers the registry lacks */
  pushedBytes?: number;
  totalBytes?: number;
  digest?: string;
  message?: string;
  lines: string[];
};
/** Either the default registry ({name, tag?}) or an address of the user's
 *  own ({image, pushAuth}); the server decides which credentials apply. */
export type SnapshotRequest = { name?: string; tag?: string; pause?: boolean; image?: string; pushAuth?: { username: string; password: string } };
export const createSnapshot = (r: Ref, body: SnapshotRequest) => req<SnapshotView>("POST", apiIn(r.cluster, `/api/devpods/${r.name}/snapshots`), body);
export const listSnapshots = (p: { all?: boolean; devpod?: string } = {}) =>
  req<{ items: SnapshotView[] }>("GET", `/api/snapshots?${new URLSearchParams({ ...(p.all ? { scope: "all" } : {}), ...(p.devpod ? { devpod: p.devpod } : {}) })}`);
/** SnapRef names a snapshot: its id and cluster. */
export type SnapRef = { id: string; cluster?: string };
export const deleteSnapshot = (s: SnapRef) => req<void>("DELETE", apiIn(s.cluster, `/api/snapshots/${s.id}`));
/** retrySnapshot starts a failed save again with its settings; the failed record stays. */
export const retrySnapshot = (s: SnapRef) => req<SnapshotView>("POST", apiIn(s.cluster, `/api/snapshots/${s.id}/retry`));

/** streamSnapshotProgress reads the progress stream until the server ends
 *  it (after the final event). It does not reconnect: a finished stream is
 *  finished, and a broken one is reported through onEnd(false). */
export function streamSnapshotProgress(snap: SnapRef, onEvent: (p: SnapshotProgress) => void, onEnd: (clean: boolean) => void, fetchFn: typeof fetch = fetch): () => void {
  const ctl = new AbortController();
  void (async () => {
    try {
      const resp = await fetchFn(apiIn(snap.cluster, `/api/snapshots/${snap.id}/progress`), { signal: ctl.signal, credentials: "same-origin" });
      if (!resp.ok || !resp.body) return onEnd(false);
      const reader = resp.body.getReader();
      const dec = new TextDecoder();
      let buf = "";
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        let nl: number;
        while ((nl = buf.indexOf("\n")) >= 0) {
          const line = buf.slice(0, nl);
          buf = buf.slice(nl + 1);
          if (line.startsWith("data: ")) {
            try {
              onEvent(JSON.parse(line.slice(6)) as SnapshotProgress);
            } catch {
              /* a torn line: the next event replaces it */
            }
          }
        }
      }
      onEnd(true);
    } catch {
      if (!ctl.signal.aborted) onEnd(false);
    }
  })();
  return () => ctl.abort();
}

/* registry credentials (snapshot spec v2 §5.3): used to pull private
 * images for the user's DevPods and to push snapshots to that registry */
export type RegistryView = { host: string; username: string; pending: boolean };
export const listRegistries = () => req<{ items: RegistryView[] }>("GET", "/api/me/registries");
/** clusters (a console of several): cluster → ok | offline | failed, where the credential went. */
export const putRegistry = (host: string, body: { username: string; password: string }) =>
  req<{ clusters?: Record<string, string> } | undefined>("PUT", `/api/me/registries/${encodeURIComponent(host)}`, body);
export const deleteRegistry = (host: string) => req<void>("DELETE", `/api/me/registries/${encodeURIComponent(host)}`);

// The queue (design 2026-11-26): jobs that wait for the reservable
// machines' unreserved capacity and run as one DevPod per rank.
export type JobPhase = "Queued" | "Starting" | "Running" | "Succeeded" | "Failed" | "TimedOut" | "Cancelled";
export type JobRank = { rank: number; node?: string; devpod?: string; phase?: string; exitCode?: number; log?: string };
export type Job = {
  cluster?: string;
  id: string;
  /** set on a queue row that stands for a DevPod waiting in line: its name ("" not shown) */
  devpod?: string;
  title?: string;
  owner: string;
  phase: JobPhase;
  /** why it waits (capacity, behind, quota, preempting, preempted, gang-timeout) or how it ended */
  reason?: string;
  detail?: Record<string, string>;
  /** its place among the waiting, from 1 */
  position?: number;
  nodes: number;
  gpus: number;
  cpus: number;
  timeLimitSeconds: number;
  priority: number;
  preemptible?: boolean;
  requeue?: boolean; // stopped for another, it waits again (false: it ends)
  /** held back by its owner or an admin: out of the queue's order while it waits */
  held?: boolean;
  mine: boolean;
  canManage: boolean;
  submitted: string;
  expectedStart?: string;
  startedAt?: string;
  finishedAt?: string;
  attempts?: number;
  preemptions?: number;
  ranks?: JobRank[];
  /** what it runs: its owner's and admins' only */
  command?: string;
  args?: string[];
  workdir?: string;
  source?: { devpod?: string; config?: string; vars?: Record<string, string> };
};
export type JobRequest = {
  title?: string;
  command?: string;
  script?: string;
  args?: string[];
  workdir?: string;
  from: { devpod?: string; config?: string; vars?: Record<string, string> };
  nodes: number;
  gpus: number;
  cpus?: number;
  timeLimitSeconds?: number;
  priority?: number;
  preemptible?: boolean;
  requeue?: boolean; // false: stopped for another, it ends (unset: it waits again)
  privileges?: { hostNetwork?: boolean; privileged?: boolean; capAdd?: string[] };
  env?: Record<string, string>;
  dryRun?: boolean;
};
export type JobEstimate = {
  position?: number;
  startsNow?: boolean;
  expectedStart?: string;
  reason?: string;
  detail?: Record<string, string>;
  cpus: number;
  /** what the submitter should know (a working directory the machines may not share): said, not refused */
  warnings?: string[];
};
export type JobLimits = { maxPriority: number; unlimitedPriority: boolean; defaultTimeLimitSeconds: number; maxTimeLimitSeconds: number };
/** listJobs: the queues — or, ended, the jobs that have ended (kept in the audit store), the last to end first, a page at a time (next: the next page's cursor). */
export const listJobs = (q: { mine?: boolean; ended?: boolean; cursor?: string } = {}) => {
  const p = new URLSearchParams();
  if (q.mine) p.set("mine", "1");
  if (q.ended) p.set("ended", "1");
  if (q.cursor) p.set("cursor", q.cursor);
  const qs = p.toString();
  return req<{ items: Job[]; devpods?: QueuedDevPod[]; next?: string }>("GET", `/api/jobs${qs ? `?${qs}` : ""}`);
};

/** QueuedDevPod: a DevPod waiting in the queue's line (its name only to
 *  its owner and admins). */
export type QueuedDevPod = {
  name?: string;
  cluster?: string;
  owner: string;
  mine: boolean;
  priority: number;
  position?: number;
  reason?: string;
  detail?: Record<string, string>;
  expectedStart?: string;
  queuedAt: string;
  gpus: number;
  cpus: number;
  node?: string;
};
export const getJob = (id: string) => req<Job>("GET", `/api/jobs/${id}`);
export const submitJob = (cluster: string | undefined, body: JobRequest) => req<Job & JobEstimate>("POST", apiIn(cluster, "/api/jobs"), body);
export const cancelJob = (id: string) => req<void>("DELETE", `/api/jobs/${id}`);
export const resubmitJob = (id: string) => req<Job>("POST", `/api/jobs/${id}/resubmit`);
/** patchJob: a job's priority (waiting or running; raised at most to its owner's cap, an admin's past it), or held back / released (waiting). */
export const patchJob = (id: string, body: { priority?: number; held?: boolean }) => req<Job>("PATCH", `/api/jobs/${id}`, body);
export const jobLimits = (cluster: string | undefined) => req<JobLimits>("GET", apiIn(cluster, "/api/jobs/limits"));
export type PriorityRule = { condition: string; maxPriority: number; maxJobs?: number | null };
export const getPriorityRules = () => req<{ items: PriorityRule[] }>("GET", "/api/admin/job-priority-rules");
export const putPriorityRules = (items: PriorityRule[]) => req<{ items: PriorityRule[] }>("PUT", "/api/admin/job-priority-rules", { items });
/** what the queue can use now in a cluster: free cards and cores, the most cards one machine has */
export type QueueFree = { cluster?: string; gpu: number; cpu: number; maxGPU: number; machines: number };
export const jobsFree = () => req<{ items: QueueFree[] }>("GET", "/api/jobs/free");
