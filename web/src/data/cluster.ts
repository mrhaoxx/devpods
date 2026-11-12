// Which cluster a thing is in.
//
// A console of several clusters keeps users, tokens and saved configs
// itself; DevPods, nodes, reservations and snapshots are a cluster's.
// Nothing is "the cluster in use": a list covers every cluster (the
// server answers for all of them when the path names none, each item
// saying its cluster), and what is done to one object goes to the
// object's own cluster. Which clusters a page shows is a filter
// (clusterFilter.tsx), not a place requests are sent.

/** Ref names an object: its cluster (absent in a console of one) and name. */
export type Ref = { cluster?: string; name: string };

/** apiIn is path in cluster c: /api/clusters/<c>/…, or path itself without a cluster. */
export function apiIn(c: string | undefined, path: string): string {
  if (!c) return path;
  return "/api/clusters/" + encodeURIComponent(c) + path.slice("/api".length);
}

/** keyOf: an object's key in lists and caches — <cluster>/<name>, or the name. */
export const keyOf = (r: { cluster?: string; name: string }) => (r.cluster ? `${r.cluster}/${r.name}` : r.name);

/** refOfKey reads a key back (a name never holds "/"). */
export function refOfKey(k: string): Ref {
  const i = k.indexOf("/");
  return i < 0 ? { name: k } : { cluster: k.slice(0, i), name: k.slice(i + 1) };
}

/** clusterSeg: a cluster as an address segment ("-": none). */
export const clusterSeg = (c?: string) => c || "-";
/** segCluster reads a segment back. */
export const segCluster = (seg?: string) => (seg && seg !== "-" ? seg : undefined);

/** devpodHref: a DevPod's page. */
export const devpodHref = (r: Ref, tab?: string) => `/devpods/${clusterSeg(r.cluster)}/${r.name}${tab ? `?tab=${tab}` : ""}`;

const LAUNCH = "devpod.lastLaunchCluster";
/** launchCluster: where a new DevPod goes by default — the cluster last
 *  launched in, else the first connected one (none in a console of one). */
export function launchCluster(clusters: { name: string; connected: boolean }[], prefer?: string): string | undefined {
  if (clusters.length === 0) return undefined;
  let last = "";
  try {
    last = window.localStorage.getItem(LAUNCH) ?? "";
  } catch {
    /* no storage */
  }
  if (clusters.some((c) => c.name === last)) return last;
  if (prefer && clusters.some((c) => c.name === prefer)) return prefer;
  return (clusters.find((c) => c.connected) ?? clusters[0]).name;
}
export function rememberLaunchCluster(c: string | undefined) {
  try {
    if (c) window.localStorage.setItem(LAUNCH, c);
  } catch {
    /* not kept: no matter */
  }
}

// the cluster that was "in use" before clusters were all shown at once
try {
  window.localStorage.removeItem("devpod.cluster");
} catch {
  /* no storage: nothing to forget */
}
