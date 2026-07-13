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
  features: { pubkeySelfService: boolean; kore: boolean; passwordAuth: boolean };
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
  usage: { cpu?: string; memory?: string; storage?: string };
  quota?: UserQuota;
};
export type QuotaPatch = { maxDevPods?: number | null; cpu?: string; memory?: string; storage?: string };
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
export const listAllDevPods = () => req<{ items: AdminDevPod[] }>("GET", "/api/admin/devpods");

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
export type DevPod = {
  metadata: { name: string };
  spec: { owner: string; running: boolean; shell?: string; persistence?: { size: string } };
  status?: { phase?: string; endpoint?: string; message?: string };
};

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
export const listDevPods = () => req<{ items: DevPod[] }>("GET", "/api/devpods");
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
export function watchDevPods(onEvent: (type: string, dp: DevPod) => void, onResync: () => void): () => void {
  return sse(
    "/api/devpods?watch=true",
    (data) => {
      const ev = JSON.parse(data) as { type: string; devpod: DevPod };
      onEvent(ev.type, ev.devpod);
    },
    onResync,
  );
}

export type DevPodDetail = { devpod: DevPod; binding?: BindingInfo };

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
