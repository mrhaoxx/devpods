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
}

async function req<T>(method: string, path: string, body?: unknown): Promise<T> {
  const resp = await fetch(path, {
    method,
    headers: body ? { "Content-Type": "application/json" } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  });
  if (resp.status === 401) {
    window.location.href = "/login";
    throw new ApiFailure(401, { code: "UNAUTHORIZED", message: "not logged in" });
  }
  if (!resp.ok) {
    throw new ApiFailure(resp.status, (await resp.json()) as ApiError);
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
  features: { pubkeySelfService: boolean; kore: boolean; passwordAuth: boolean; audit?: boolean };
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
  metadata: { name: string; uid?: string; creationTimestamp?: string };
  spec: {
    owner: string;
    running: boolean;
    shell?: string;
    collaborators?: string[];
    persistence?: { size: string; mountPath?: string };
    pod?: { spec?: { containers?: ContainerSpec[] } };
  };
  status?: {
    phase?: string;
    endpoint?: string;
    message?: string;
    hibernatedAt?: string;
    retryCount?: number;
    lastFailureAt?: string;
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
export const listDevPods = () => req<{ items: DevPod[]; pods: Record<string, PodInfo> }>("GET", "/api/devpods");
export const auditPresence = (all = false) =>
  req<{ items: Record<string, Presence> }>("GET", `/api/audit/presence${all ? "?scope=all" : ""}`);
export const getDevPod = (n: string) => req<{ devpod: DevPod; binding?: BindingInfo }>("GET", `/api/devpods/${n}`);
export const createDevPod = (body: unknown) => req<DevPod>("POST", "/api/devpods", body);
export const patchDevPod = (n: string, running: boolean) => req<DevPod>("PATCH", `/api/devpods/${n}`, { running });
export const deleteDevPod = (n: string) => req<void>("DELETE", `/api/devpods/${n}`);
export const getEvents = (n: string) => req<{ items: unknown[] }>("GET", `/api/devpods/${n}/events`);
export const listTemplates = () => req<{ items: Template[] }>("GET", "/api/templates");
export const getPubkeys = () => req<{ pubkeys: string[] | null }>("GET", "/api/me/pubkeys");
export const putPubkeys = (pubkeys: string[]) => req<{ pubkeys: string[] }>("PUT", "/api/me/pubkeys", { pubkeys });

// sse opens an EventSource with backoff-reconnect; onResync fires on
// each (re)connect so callers can refetch anything missed offline.
function sse(url: string, onMessage: (data: string) => void, onResync?: () => void): () => void {
  let es: EventSource | null = null;
  let stopped = false;
  let delay = 1000;
  const connect = () => {
    if (stopped) return;
    es = new EventSource(url);
    es.onopen = () => {
      delay = 1000;
      onResync?.();
    };
    es.onmessage = (m) => onMessage(m.data);
    es.onerror = () => {
      es?.close();
      if (!stopped) {
        setTimeout(connect, delay);
        delay = Math.min(delay * 2, 30000);
      }
    };
  };
  connect();
  return () => {
    stopped = true;
    es?.close();
  };
}

// watchDevPods streams the caller's DevPod changes (list page).
export function watchDevPods(onEvent: (type: string, dp: DevPod, pod?: PodInfo) => void, onResync: () => void): () => void {
  return sse(
    "/api/devpods?watch=true",
    (data) => {
      const ev = JSON.parse(data) as { type: string; devpod: DevPod; pod?: PodInfo };
      onEvent(ev.type, ev.devpod, ev.pod);
    },
    onResync,
  );
}

export type DevPodDetail = { devpod: DevPod; binding?: BindingInfo; pod?: PodInfo };

// watchDevPod opens ONE SSE connection carrying both this DevPod's
// status (detail messages, with binding readback) and its k8s Events.
// The server replays the current DevPod and event backlog on connect,
// so no separate initial fetch is needed. Keeping the detail page to a
// single connection avoids the browser's 6-conn HTTP/1.1 per-origin
// cap.
export function watchDevPod(
  name: string,
  handlers: { onDetail: (d: DevPodDetail) => void; onEvent: (type: string, ev: K8sEvent) => void },
): () => void {
  return sse(`/api/devpods/${name}/stream`, (data) => {
    const m = JSON.parse(data) as {
      kind: string;
      type: string;
      detail?: DevPodDetail;
      event?: K8sEvent;
    };
    if (m.kind === "devpod" && m.detail) handlers.onDetail(m.detail);
    else if (m.kind === "event" && m.event) handlers.onEvent(m.type, m.event);
  });
}

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
export const rawURL = (id: string, ch: number, stream: "in" | "out" | "err") =>
  `/api/audit/sessions/${id}/channels/${ch}/raw?stream=${stream}`;
export const fileContentURL = (eventId: string) => `/api/audit/files/${eventId}/content`;

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
