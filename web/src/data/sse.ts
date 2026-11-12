import { apiPath } from "./cluster";
// Server-sent events with reconnect (UI spec §6.1: every stream sends a
// full snapshot each second, so a reconnect needs no catch-up).
import type { DevPod, DevPodDetail, K8sEvent, PodInfo } from "./api";

export type SseHandlers = {
  onMessage(data: unknown): void;
  onNamed(event: string, data: unknown): void;
  onOpen(): void;
  onError(): void;
};
export type OpenSSE = (url: string, h: SseHandlers) => () => void;

/** Named SSE events the server sends besides plain data. */
const NAMED = ["liveStatus"];

function parse(data: string): { ok: true; v: unknown } | { ok: false } {
  try {
    return { ok: true, v: JSON.parse(data) };
  } catch {
    return { ok: false };
  }
}

/** makeOpenSSE builds an opener over an EventSource implementation;
 *  reconnects back off 1 s → 30 s and reset once a connection opens. */
export function makeOpenSSE(ES: typeof EventSource): OpenSSE {
  return (url, h) => {
    let es: EventSource | null = null;
    let stopped = false;
    let delay = 1000;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const connect = () => {
      if (stopped) return;
      es = new ES(url);
      es.onopen = () => {
        delay = 1000;
        h.onOpen();
      };
      es.onmessage = (m) => {
        const p = parse(m.data);
        if (p.ok) h.onMessage(p.v);
      };
      for (const ev of NAMED) {
        es.addEventListener(ev, (m) => {
          const p = parse((m as MessageEvent).data);
          if (p.ok) h.onNamed(ev, p.v);
        });
      }
      es.onerror = () => {
        es?.close();
        h.onError();
        if (stopped) return;
        timer = setTimeout(connect, delay);
        delay = Math.min(delay * 2, 30_000);
      };
    };
    connect();
    return () => {
      stopped = true;
      clearTimeout(timer);
      es?.close();
    };
  };
}

export const openSSE: OpenSSE = (url, h) => makeOpenSSE(EventSource)(apiPath(url), h);

const quiet: Omit<SseHandlers, "onMessage"> = { onNamed() {}, onOpen() {}, onError() {} };

/** watchDevPods streams the caller's DevPod changes; onResync fires on
 *  every (re)connect so callers refetch what they missed. */
export function watchDevPods(
  onEvent: (type: string, dp: DevPod, pod?: PodInfo) => void,
  onResync: () => void,
  scope: "mine" | "all" = "mine",
): () => void {
  return openSSE(`/api/devpods?watch=true${scope === "all" ? "&scope=all" : ""}`, {
    ...quiet,
    onOpen: onResync,
    onMessage: (d) => {
      const ev = d as { type: string; devpod: DevPod; pod?: PodInfo };
      onEvent(ev.type, ev.devpod, ev.pod);
    },
  });
}

/** watchDevPod: one connection carrying a DevPod's status and its k8s
 *  Events (the server replays both on connect). */
export function watchDevPod(
  name: string,
  handlers: { onDetail: (d: DevPodDetail) => void; onEvent: (type: string, ev: K8sEvent) => void },
): () => void {
  return openSSE(`/api/devpods/${name}/stream`, {
    ...quiet,
    onMessage: (d) => {
      const m = d as { kind: string; type: string; detail?: DevPodDetail; event?: K8sEvent };
      if (m.kind === "devpod" && m.detail) handlers.onDetail(m.detail);
      else if (m.kind === "event" && m.event) handlers.onEvent(m.type, m.event);
    },
  });
}
