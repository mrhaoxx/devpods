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
  nameBudget: number;
  quota: { maxDevPods?: number; compute?: Record<string, string>; storage?: string };
  usage: { devpods: number; running: number; compute: Record<string, string>; storage: string };
  features: { pubkeySelfService: boolean; kore: boolean; passwordAuth: boolean; audit?: boolean; telemetry?: boolean; reservations?: boolean; webshell?: boolean; httpProxy?: boolean; snapshots?: boolean; snapshotDefaultRegistry?: boolean };
  hasPassword: boolean;
  ssh: { host: string; port: number; loginSuffix?: string };
}

// sshUser builds the login string: "<owner>+<pod>" plus an optional
// deployment-configured suffix (e.g. "+hpc101" for a proxy route).
export function sshUser(me: Me | undefined, owner: string, pod: string): string {
  const suffix = me?.ssh?.loginSuffix;
  return `${owner}+${pod}${suffix ? `+${suffix}` : ""}`;
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
export const devpodTopology = (name: string) =>
  req<{ node: KoreNode | null; cpuset?: string }>("GET", `/api/devpods/${name}/topology`);

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

// sshCommand renders the copy-pastable login line using the
// deployment's advertised gateway address (-p only when non-22).
export function sshCommand(me: Me | undefined, owner: string, pod: string): string {
  const host = me?.ssh?.host || "<gateway>";
  const port = me?.ssh?.port ?? 22;
  const flag = host !== "<gateway>" && port !== 22 ? `-p ${port} ` : "";
  return `ssh ${flag}${sshUser(me, owner, pod)}@${host}`;
}

// sshConfig renders a ~/.ssh/config block so the user can shortcut the
// full login to `ssh <alias>`. The alias is the DevPod name.
export function sshConfig(me: Me | undefined, owner: string, pod: string): string {
  const host = me?.ssh?.host || "<gateway>";
  const port = me?.ssh?.port ?? 22;
  const alias = `${owner}-${pod}`;
  return `Host ${alias}\n    HostName ${host}\n    Port ${port}\n    User ${sshUser(me, owner, pod)}`;
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
  metadata: { name: string; uid?: string; creationTimestamp?: string; annotations?: Record<string, string> };
  spec: {
    owner: string;
    running: boolean;
    shell?: string;
    collaborators?: string[];
    persistence?: { size: string; mountPath?: string };
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

export type Template = {
  metadata: { name: string };
  spec: {
    displayName: string;
    description?: string;
    binding?: { annotations: Record<string, string>; resources: unknown };
    podPreset?: { image: string };
  };
};

export type BindingInfo = {
  allocatedCpuset?: string;
  reservedNuma?: string;
  pool?: string;
  poolSize?: string;
};

export const me = () => req<Me>("GET", "/api/me");
export type DevPodList = { items: DevPod[]; pods: Record<string, PodInfo> };
export const listDevPods = (scope: "mine" | "all" = "mine") =>
  req<DevPodList>("GET", scope === "all" ? "/api/devpods?scope=all" : "/api/devpods");
export const auditPresence = (all = false) =>
  req<{ items: Record<string, Presence> }>("GET", `/api/audit/presence${all ? "?scope=all" : ""}`);
export const getDevPod = (n: string) => req<DevPodDetail>("GET", `/api/devpods/${n}`);
export const createDevPod = (body: unknown) => req<DevPod>("POST", "/api/devpods", body);
export const patchDevPod = (n: string, running: boolean) => req<PatchResult>("PATCH", `/api/devpods/${n}`, { running });
export const deleteDevPod = (n: string) => req<void>("DELETE", `/api/devpods/${n}`);
export const getEvents = (n: string) => req<{ items: unknown[] }>("GET", `/api/devpods/${n}/events`);
export const listTemplates = () => req<{ items: Template[] }>("GET", "/api/templates");
export const getPubkeys = () => req<{ pubkeys: string[] | null }>("GET", "/api/me/pubkeys");
export const putPubkeys = (pubkeys: string[]) => req<{ pubkeys: string[] }>("PUT", "/api/me/pubkeys", { pubkeys });

export { watchDevPods, watchDevPod } from "./sse";
export type DevPodDetail = { devpod: DevPod; binding?: BindingInfo; pod?: PodInfo };

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
export type SessionSummary = SessionRow & { commands: number; files: number; policy_hits: number; alerts: number };
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
export type PolicyEventRow = { ts: string; session_id: string; user: string; rule: string; action: string; detail: string };
export type AlertRow = { ts: string; devpod_uid: string; session_id: string; kind: string; source: string; detail: string };
export type AccessLogRow = { ts: string; viewer: string; action: string; target_session: string; target_file: string; client_ip: string };
export type SessionDetail = {
  session: SessionRow;
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

export const patchDevPodSpec = (n: string, body: SpecPatch) => req<PatchResult>("PATCH", `/api/devpods/${n}`, body);
/** redeployDevPod recreates the Pod (sessions drop, the home volume stays). */
export const redeployDevPod = (n: string) => req<void>("POST", `/api/devpods/${n}/redeploy`);
export const getMetrics = (n: string, range: MetricRange) =>
  req<MetricSeries>("GET", `/api/devpods/${n}/metrics?range=${range}`);
export const getRecent = (p: { scope?: "mine" | "all"; name?: string }) =>
  req<RecentFrame>("GET", `/api/live/devpods/recent?${p.name ? `name=${encodeURIComponent(p.name)}` : `scope=${p.scope ?? "mine"}`}`);
export const getInfo = (n: string) => req<ContainerInfo>("GET", `/api/devpods/${n}/info`);
export const listNodes = () => req<{ items: NodeView[]; live: boolean }>("GET", "/api/admin/nodes");
/** Each node's CPU / memory use over the sparkline window, from ClickHouse. */
export const getNodesRecent = () => req<RecentFrame>("GET", "/api/admin/nodes/recent");
export const getNode = (n: string) => req<NodeDetail>("GET", `/api/admin/nodes/${n}`);
export const listAdminUsers = () => req<{ items: AdminUserRow[]; defaultQuota: UserQuota }>("GET", "/api/admin/users");
export const usersBatch = (body: BatchRequest) => req<{ results: BatchResult[] }>("POST", "/api/admin/users:batch", body);
export const importUsers = (csv: string, dryRun: boolean) =>
  req<{ rows: ImportResult[]; created: number }>("POST", `/api/admin/users:import${dryRun ? "?dryRun=1" : ""}`, undefined, {
    raw: { body: csv, contentType: "text/csv" },
  });
export const setUserAdmin = (name: string, admin: boolean) => req<void>("PATCH", `/api/admin/users/${name}`, { admin });
export const search = (q: string) => req<SearchResult>("GET", `/api/search?q=${encodeURIComponent(q)}`);

/** shellPath: the web terminal page of a DevPod (opened in its own tab). */
export const shellPath = (name: string) => `/devpods/${name}/shell`;
/** shellSocketURL: the terminal WebSocket (UI spec §5.12). */
export const shellSocketURL = (name: string, cols: number, rows: number, loc: Pick<Location, "protocol" | "host"> = window.location) =>
  `${loc.protocol === "https:" ? "wss" : "ws"}://${loc.host}/api/devpods/${name}/shell?cols=${cols}&rows=${rows}`;
/** probeShell asks why a terminal cannot be opened (a failed WebSocket handshake hides its status). */
export const probeShell = (name: string) => req<void>("GET", `/api/devpods/${name}/shell?probe=1`);

/* api_http.go: DevPod ports published through the gateway's HTTP proxy */
/** publicPorts: anyone with the link; userPorts: anyone signed in to the console; the rest: owner, collaborators, admins. */
export type HttpView = { id: string; urlTemplate: string; publicPorts: number[]; userPorts: number[] };
export const getHttp = (n: string) => req<HttpView>("GET", `/api/devpods/${n}/http`);

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
export const getDevPodPod = (n: string) => req<{ pod: PodDetail | null }>("GET", `/api/devpods/${n}/pod`);
/** A DevPod's manifest as YAML, and saving it back (the spec is replaced). */
export const getDevPodYaml = (n: string) => req<{ yaml: string; running: boolean }>("GET", `/api/devpods/${n}/yaml`);
export const putDevPodYaml = (n: string, yaml: string) => req<{ devpod: DevPod; rebuild: boolean }>("PUT", `/api/devpods/${n}/yaml`, { yaml });
export const putHttp = (n: string, body: { publicPorts?: number[]; userPorts?: number[]; reset?: boolean }) => req<HttpView>("PUT", `/api/devpods/${n}/http`, body);
/** httpTicket: the URL that signs this browser in on a private port's hostname. */
export const httpTicket = (host: string, rd: string) => req<{ url: string }>("POST", "/api/http/ticket", { host, rd });

/* tokens.go: personal access tokens */
export type TokenScope = { id: string; group: string; label: string; admin?: boolean };
export type TokenView = { id: string; name: string; scopes: string[]; devpods: string[]; createdAt: string; expiresAt: string | null; expired: boolean };
export const listTokens = () => req<{ items: TokenView[]; scopes: TokenScope[] }>("GET", "/api/me/tokens");
export const createToken = (body: { name: string; scopes: string[]; devpods: string[]; expiresInDays: number }) =>
  req<{ token: string; info: TokenView }>("POST", "/api/me/tokens", body);
export const deleteToken = (id: string) => req<void>("DELETE", `/api/me/tokens/${id}`);

/* api_reservations.go: node choice and node reservations */
export type NodeChoice = {
  name: string;
  ready: boolean;
  cpu: { allocatable: number; allocated: number };
  mem: { allocatable: number; allocated: number };
  gpuModel?: string;
  gpuTotal: number;
  gpuUsed: number;
  reservable: boolean;
  pending: number;
  held: { user: string; until: string } | null;
  next: { user: string; start: string } | null;
  /** my DevPods may be pinned here now */
  selectable: boolean;
  reason?: string;
};
export const listNodeChoices = () => req<{ items: NodeChoice[] }>("GET", "/api/nodes");
export type Reservation = {
  id: string;
  segments: string[];
  node: string;
  user: string;
  start: string;
  end: string;
  phase: "Upcoming" | "Active" | "Ended" | "Cancelled";
  mine: boolean;
  cancelledBy?: string;
};
export type ReservationRules = { granularitySeconds: number; maxDurationSeconds: number; maxAdvanceSeconds: number; clearNoticeSeconds: number; timezone: string; now: string };
export const getReservationRules = () => req<ReservationRules>("GET", "/api/reservations/rules");
export const listReservations = (from: string, to: string) => req<{ items: Reservation[] }>("GET", `/api/reservations?from=${encodeURIComponent(from)}&to=${encodeURIComponent(to)}`);
type Period = { node: string; start: string; end: string };
/** previewReservation checks a period without creating it. */
export const previewReservation = (p: Period) => req<{ willHibernate: number }>("POST", "/api/reservations?dryRun=true", p);
export const createReservation = (p: Period) => req<Reservation>("POST", "/api/reservations", p);
export const patchReservation = (id: string, body: { start?: string; end?: string }) => req<Reservation>("PATCH", `/api/reservations/${id}`, body);
export const deleteReservation = (id: string) => req<void>("DELETE", `/api/reservations/${id}`);
export const setReservable = (node: string, reservable: boolean) => req<{ reservable: boolean; pending: number }>("PUT", `/api/admin/nodes/${node}/reservable`, { reservable });

/* api_snapshots.go: saving a DevPod as an image (snapshot spec v2) */
export type SnapshotView = {
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
export const createSnapshot = (devpod: string, body: SnapshotRequest) => req<SnapshotView>("POST", `/api/devpods/${devpod}/snapshots`, body);
export const listSnapshots = (p: { all?: boolean; devpod?: string } = {}) =>
  req<{ items: SnapshotView[] }>("GET", `/api/snapshots?${new URLSearchParams({ ...(p.all ? { scope: "all" } : {}), ...(p.devpod ? { devpod: p.devpod } : {}) })}`);
export const deleteSnapshot = (id: string) => req<void>("DELETE", `/api/snapshots/${id}`);

/** streamSnapshotProgress reads the progress stream until the server ends
 *  it (after the final event). It does not reconnect: a finished stream is
 *  finished, and a broken one is reported through onEnd(false). */
export function streamSnapshotProgress(id: string, onEvent: (p: SnapshotProgress) => void, onEnd: (clean: boolean) => void, fetchFn: typeof fetch = fetch): () => void {
  const ctl = new AbortController();
  void (async () => {
    try {
      const resp = await fetchFn(`/api/snapshots/${id}/progress`, { signal: ctl.signal, credentials: "same-origin" });
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
export const putRegistry = (host: string, body: { username: string; password: string }) => req<void>("PUT", `/api/me/registries/${encodeURIComponent(host)}`, body);
export const deleteRegistry = (host: string) => req<void>("DELETE", `/api/me/registries/${encodeURIComponent(host)}`);
