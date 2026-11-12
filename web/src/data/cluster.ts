// Which cluster the console is looking at.
//
// A console of several clusters keeps users, tokens and saved configs
// itself; DevPods, nodes, reservations and snapshots are a
// cluster's. One cluster is in use at a time (the switcher in the top
// bar): every request for something that is a cluster's goes to
// /api/clusters/<cluster>/…, so the pages need not know. In a console of
// one cluster nothing is chosen and no path changes.
import { useSyncExternalStore } from "react";

const KEY = "devpod.cluster";

function load(): string {
  try {
    return window.localStorage.getItem(KEY) ?? "";
  } catch {
    return "";
  }
}

let current = load();
let stale: string | null = null;
const listeners = new Set<() => void>();
const emit = () => listeners.forEach((l) => l());
const subscribe = (l: () => void) => {
  listeners.add(l);
  return () => void listeners.delete(l);
};

/** currentCluster: the cluster in use; "" in a console of one cluster. */
export const currentCluster = () => current;

/** rememberCluster makes name the cluster in use, here and after a reload. */
export function rememberCluster(name: string) {
  if (name === current) return;
  current = name;
  stale = null;
  try {
    if (name) window.localStorage.setItem(KEY, name);
    else window.localStorage.removeItem(KEY);
  } catch {
    /* private mode: the choice lasts until the reload */
  }
  emit();
}

export const useCurrentCluster = () => useSyncExternalStore(subscribe, currentCluster);

/** What is a cluster's. Everything else is the console's and is never prefixed. */
const owned = [
  "/api/devpods",
  "/api/nodes",
  "/api/reservations",
  "/api/snapshots",
  "/api/kore",
  "/api/live",
  "/api/admin/nodes",
  "/api/admin/devpods",
  "/api/admin/live",
];
const launch = /^\/api\/configs\/[^/?]+\/launch(?:[/?]|$)/;

export function isClusterPath(path: string): boolean {
  return launch.test(path) || owned.some((p) => path === p || path.startsWith(p + "/") || path.startsWith(p + "?"));
}

/** apiPath puts a path that is a cluster's under the cluster in use. */
export function apiPath(path: string): string {
  if (!current || !isClusterPath(path)) return path;
  return "/api/clusters/" + encodeURIComponent(current) + path.slice("/api".length);
}

export type ClusterChoice = { name: string; connected: boolean };

/** pickCluster: the remembered cluster if it still exists, else one that
 *  is connected, else the first; "" when there is nothing to choose. */
export function pickCluster(list: ClusterChoice[], remembered: string): string {
  if (list.length === 0) return "";
  if (list.some((c) => c.name === remembered)) return remembered;
  return (list.find((c) => c.connected) ?? list[0]).name;
}

/** noteStale records what the last answer of the cluster in use said:
 *  since when it is the last known state (its agent is away), or null. */
export function noteStale(since: string | null) {
  if (since === stale) return;
  stale = since;
  emit();
}
export const staleSince = () => stale;
export const useStaleSince = () => useSyncExternalStore(subscribe, staleSince);
