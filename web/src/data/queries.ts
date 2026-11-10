// Query keys and hooks over the typed client. Server data lives in the
// TanStack Query cache; live metrics never do (they live in LiveHub).
import { useEffect } from "react";
import { useMutation, useQuery, useQueryClient, type QueryClient } from "@tanstack/react-query";
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
  listNodes,
  listTemplates,
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
  devpod: (name: string) => ["devpod", name] as const,
  templates: ["templates"] as const,
  users: ["admin", "users"] as const,
  nodes: ["admin", "nodes"] as const,
  node: (name: string) => ["admin", "node", name] as const,
  metrics: (name: string, range: MetricRange) => ["metrics", name, range] as const,
  info: (name: string) => ["info", name] as const,
  auditSessions: (params: Record<string, string | undefined>) => ["audit", "sessions", params] as const,
  presence: (all: boolean) => ["presence", all] as const,
};

export const useMe = () => useQuery({ queryKey: qk.me, queryFn: me, staleTime: 30_000 });

/** applyWatchEvent folds one watch event into a cached list. */
export function applyWatchEvent(old: DevPodList | undefined, type: string, dp: DevPodList["items"][number], pod?: DevPodList["pods"][string]): DevPodList {
  const items = old?.items ?? [];
  const pods = { ...(old?.pods ?? {}) };
  const name = dp.metadata.name;
  if (type === "DELETED") {
    delete pods[name];
    return { items: items.filter((d) => d.metadata.name !== name), pods };
  }
  if (pod) pods[name] = pod;
  else delete pods[name];
  const idx = items.findIndex((d) => d.metadata.name === name);
  const next = [...items];
  if (idx >= 0) next[idx] = dp;
  else next.push(dp);
  return { items: next, pods };
}

/** useDevPods: the list plus its watch stream folded into the cache. */
export function useDevPods(scope: "mine" | "all" = "mine", enabled = true) {
  const qc = useQueryClient();
  const q = useQuery({ queryKey: qk.devpods(scope), queryFn: () => listDevPods(scope), enabled });
  useEffect(() => {
    if (!enabled) return;
    return watchDevPods(
      (type, dp, pod) => qc.setQueryData<DevPodList>(qk.devpods(scope), (old) => applyWatchEvent(old, type, dp, pod)),
      () => void qc.invalidateQueries({ queryKey: qk.devpods(scope) }),
      scope,
    );
  }, [qc, scope, enabled]);
  return q;
}

export const useDevPod = (name: string) => useQuery({ queryKey: qk.devpod(name), queryFn: () => getDevPod(name) });
export const useTemplates = () => useQuery({ queryKey: qk.templates, queryFn: listTemplates, staleTime: 60_000 });
export const useUsers = (enabled = true) =>
  useQuery({ queryKey: qk.users, queryFn: listAdminUsers, enabled, refetchInterval: 30_000 });
export const useNodes = (enabled = true) => useQuery({ queryKey: qk.nodes, queryFn: listNodes, enabled, refetchInterval: 10_000 });
export const useNode = (name: string | undefined) =>
  useQuery({ queryKey: qk.node(name ?? ""), queryFn: () => getNode(name!), enabled: !!name, refetchInterval: 10_000 });

/** History refreshes faster for the short ranges. */
export const useMetrics = (name: string, range: MetricRange | undefined) =>
  useQuery({
    queryKey: qk.metrics(name, range ?? "15m"),
    queryFn: () => getMetrics(name, range!),
    enabled: !!range,
    refetchInterval: range === "15m" || range === "1h" ? 10_000 : 60_000,
    retry: false,
  });
/** useRecent: sparkline history (15 min) from ClickHouse, refreshed each minute. */
export const useRecent = (p: { scope?: "mine" | "all"; name?: string }, enabled = true) =>
  useQuery({ queryKey: ["recent", p.scope ?? "", p.name ?? ""], queryFn: () => getRecent(p), enabled, refetchInterval: 60_000, retry: false });
/** useNodesRecent: the nodes' sparkline history, shared by every cell. */
export const useNodesRecent = () => useQuery({ queryKey: ["recent", "nodes"], queryFn: getNodesRecent, refetchInterval: 60_000, retry: false });
export const useInfo = (name: string, enabled = true) =>
  useQuery({ queryKey: qk.info(name), queryFn: () => getInfo(name), enabled, retry: false, staleTime: 30_000 });
export const useAuditSessions = (params: Record<string, string | undefined>, opts: { enabled?: boolean; refetchInterval?: number } = {}) =>
  useQuery({ queryKey: qk.auditSessions(params), queryFn: () => listAuditSessions(params), retry: false, ...opts });
export const usePresence = (all: boolean, enabled: boolean) =>
  useQuery({ queryKey: qk.presence(all), queryFn: () => auditPresence(all), enabled, refetchInterval: 10_000 });

function invalidateDevPods(qc: QueryClient, name?: string) {
  void qc.invalidateQueries({ queryKey: ["devpods"] });
  if (name) void qc.invalidateQueries({ queryKey: qk.devpod(name) });
  void qc.invalidateQueries({ queryKey: qk.me });
}

/** DevPod mutations; each invalidates the lists, the DevPod and me. */
export function useDevPodMutations() {
  const qc = useQueryClient();
  const setRunning = useMutation({
    mutationFn: ({ name, running }: { name: string; running: boolean }) => patchDevPod(name, running),
    onSettled: (_d, _e, v) => invalidateDevPods(qc, v.name),
  });
  const patchSpec = useMutation({
    mutationFn: ({ name, body }: { name: string; body: SpecPatch }) => patchDevPodSpec(name, body),
    onSettled: (_d, _e, v) => invalidateDevPods(qc, v.name),
  });
  const remove = useMutation({
    mutationFn: (name: string) => deleteDevPod(name),
    onSettled: (_d, _e, name) => invalidateDevPods(qc, name),
  });
  return { setRunning, patchSpec, remove };
}
