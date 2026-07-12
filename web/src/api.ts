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
}

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

// watchDevPods opens the SSE stream; reconnects with backoff and
// calls onResync after each (re)connect so the caller re-lists
// (events may have been missed while disconnected).
export function watchDevPods(onEvent: (type: string, dp: DevPod) => void, onResync: () => void): () => void {
  let es: EventSource | null = null;
  let stopped = false;
  let delay = 1000;
  const connect = () => {
    if (stopped) return;
    es = new EventSource("/api/devpods?watch=true");
    es.onopen = () => {
      delay = 1000;
      onResync();
    };
    es.onmessage = (m) => {
      const ev = JSON.parse(m.data) as { type: string; devpod: DevPod };
      onEvent(ev.type, ev.devpod);
    };
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
