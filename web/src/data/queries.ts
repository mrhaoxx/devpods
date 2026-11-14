// Query keys and hooks over the typed client. Server data lives in the
// TanStack Query cache; live metrics never do (they live in LiveHub).
import { useEffect } from "react";
import { useMutation, useQuery, useQueryClient, type QueryClient, keepPreviousData } from "@tanstack/react-query";
import { keyOf, type Ref } from "./cluster";
import {
  auditPresence,
  deleteDevPod,
  getDevPod,
  getInfo,
  getMetrics,
  getNode,
  getNodesRecent,
  getRecent,
  listAdminUsers,
  listAuditSessions,
  listDevPods,
  listNodeChoices,
  listNodes,
  listSnapshots,
  me,
  patchDevPod,
  patchDevPodSpec,
  watchDevPods,
  type DevPodList,
  type MetricRange,
  type SpecPatch,
} from "./api";

export const qk = {
  me: ["me"] as const,
  devpods: (scope: "mine" | "all") => ["devpods", scope] as const,
  devpod: (r: Ref) => ["devpod", r.cluster ?? "", r.name] as const,
  users: ["admin", "users"] as const,
  nodes: ["admin", "nodes"] as const,
  node: (r: Ref) => ["admin", "node", r.cluster ?? "", r.name] as const,
  metrics: (r: Ref, range: MetricRange) => ["metrics", r.cluster ?? "", r.name, range] as const,
  info: (r: Ref) => ["info", r.cluster ?? "", r.name] as const,
  auditSessions: (params: Record<string, string | undefined>) => ["audit", "sessions", params] as const,
  presence: (all: boolean) => ["presence", all] as const,
};

export const useMe = () => useQuery({ queryKey: qk.me, queryFn: me, staleTime: 30_000 });

/** applyWatchEvent folds one watch event into a cached list. */
export function applyWatchEvent(old: DevPodList | undefined, type: string, dp: DevPodList["items"][number], pod?: DevPodList["pods"][string]): DevPodList {
  const items = old?.items ?? [];
  const pods = { ...(old?.pods ?? {}) };
  const key = keyOf({ cluster: dp.cluster, name: dp.metadata.name });
  const at = (d: DevPodList["items"][number]) => keyOf({ cluster: d.cluster, name: d.metadata.name }) === key;
  const idx = items.findIndex(at);
  const cur = dp;
  if (type === "DELETED") {
    delete pods[key];
    return { items: items.filter((d) => !at(d)), pods };
  }
  if (pod) pods[key] = pod;
  else delete pods[key];
  const next = [...items];
  if (idx >= 0) next[idx] = cur;
  else next.push(cur);
  return { items: next, pods };
}

const WATCH_BATCH_MS = 150;

/** useDevPods: the list plus its watch stream folded into the cache. */
export function useDevPods(scope: "mine" | "all" = "mine", enabled = true) {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: qk.devpods(scope), queryFn: () => listDevPods(scope), enabled });
  useEffect(() => {
    if (!enabled) return;
    // Events are folded in together, a few times a second: starting 600
    // DevPods sends well over a thousand, and each one applied by itself
    // made the page rebuild, filter and sort the whole list again.
    let queue: Parameters<Parameters<typeof watchDevPods>[0]>[] = [];
    let timer: ReturnType<typeof setTimeout> | undefined;
    const flush = () => {
      timer = undefined;
      const batch = queue;
      queue = [];
      qc.setQueryData<DevPodList>(qk.devpods(scope), (old) => batch.reduce<DevPodList | undefined>((acc, [type, dp, pod]) => applyWatchEvent(acc, type, dp, pod), old));
    };
    const stop = watchDevPods(
      (...ev) => {
        queue.push(ev);
        timer ??= setTimeout(flush, WATCH_BATCH_MS);
      },
      () => void qc.invalidateQueries({ queryKey: qk.devpods(scope) }),
      scope,
    );
    return () => {
      stop();
      if (timer) clearTimeout(timer);
    };
  }, [qc, scope, enabled]);
  return q;
}

export const useDevPod = (r: Ref) => useQuery({ queryKey: qk.devpod(r), queryFn: () => getDevPod(r) });
export const useUsers = (enabled = true) =>
  useQuery({ queryKey: qk.users, queryFn: listAdminUsers, enabled, refetchInterval: 30_000 });
export const useNodes = (enabled = true) => useQuery({ queryKey: qk.nodes, queryFn: listNodes, enabled, refetchInterval: 10_000 });
export const useNode = (r: Ref | undefined) =>
  useQuery({ queryKey: qk.node(r ?? { name: "" }), queryFn: () => getNode(r!), enabled: !!r?.name, refetchInterval: 10_000 });

/** History refreshes faster for the short ranges. */
export const useMetrics = (r: Ref, range: MetricRange | undefined) =>
  useQuery({
    queryKey: qk.metrics(r, range ?? "15m"),
    queryFn: () => getMetrics(r, range!),
    enabled: !!range,
    // a new range keeps the charts of the last until its own arrive
    placeholderData: keepPreviousData,
    refetchInterval: range === "15m" || range === "1h" ? 10_000 : 60_000,
    retry: false,
  });
// Sparkline history is refreshed each minute while the tab shows, and at
// once when it shows again: in the background nothing is fetched and the
// live tail keeps only a minute, so without this a curve came back with
// the time away missing until the next minute's refresh.
const sparkHistory = { refetchInterval: 60_000, refetchOnWindowFocus: "always", retry: false } as const;
/** useRecent: sparkline history (15 min) from ClickHouse. */
export const useRecent = (p: { scope?: "mine" | "all"; ref?: Ref }, enabled = true) =>
  useQuery({ queryKey: ["recent", p.scope ?? "", p.ref?.cluster ?? "", p.ref?.name ?? ""], queryFn: () => getRecent(p), enabled, ...sparkHistory });
/** useNodesRecent: the nodes' sparkline history, shared by every cell. */
export const useNodesRecent = () => useQuery({ queryKey: ["recent", "nodes"], queryFn: getNodesRecent, ...sparkHistory });
/** useNodeChoices: the nodes a DevPod can be pinned to, with reservation state. */
export const useNodeChoices = (enabled = true) => useQuery({ queryKey: ["node-choices"], queryFn: listNodeChoices, enabled, refetchInterval: 30_000 });
/** useSnapshots: saved images; refreshed while any is still being saved. */
export const useSnapshots = (p: { all?: boolean; devpod?: string } = {}, enabled = true) =>
  useQuery({
    queryKey: ["snapshots", p.all ? "all" : "mine", p.devpod ?? ""],
    queryFn: () => listSnapshots(p),
    enabled,
    retry: false,
    refetchInterval: (q) => (q.state.data?.items.some((s) => s.phase === "Pending" || s.phase === "Running") ? 3000 : 30_000),
  });
export const useInfo = (r: Ref, enabled = true) =>
  useQuery({ queryKey: qk.info(r), queryFn: () => getInfo(r), enabled, retry: false, staleTime: 30_000 });
export const useAuditSessions = (params: Record<string, string | undefined>, opts: { enabled?: boolean; refetchInterval?: number } = {}) =>
  useQuery({ queryKey: qk.auditSessions(params), queryFn: () => listAuditSessions(params), retry: false, ...opts });
export const usePresence = (all: boolean, enabled: boolean) =>
  useQuery({ queryKey: qk.presence(all), queryFn: () => auditPresence(all), enabled, refetchInterval: 10_000 });

function invalidateDevPods(qc: QueryClient, r?: Ref) {
  void qc.invalidateQueries({ queryKey: ["devpods"] });
  if (r) void qc.invalidateQueries({ queryKey: qk.devpod(r) });
  void qc.invalidateQueries({ queryKey: qk.me });
}

/** DevPod mutations; each invalidates the lists, the DevPod and me. */
export function useDevPodMutations() {
  const qc = useQueryClient();
  const setRunning = useMutation({
    mutationFn: ({ ref, running }: { ref: Ref; running: boolean }) => patchDevPod(ref, running),
    onSettled: (_d, _e, v) => invalidateDevPods(qc, v.ref),
  });
  const patchSpec = useMutation({
    mutationFn: ({ ref, body }: { ref: Ref; body: SpecPatch }) => patchDevPodSpec(ref, body),
    onSettled: (_d, _e, v) => invalidateDevPods(qc, v.ref),
  });
  const remove = useMutation({
    mutationFn: (ref: Ref) => deleteDevPod(ref),
    onSettled: (_d, _e, ref) => invalidateDevPods(qc, ref),
  });
  return { setRunning, patchSpec, remove };
}
