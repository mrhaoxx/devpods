import { ApiFailure, req } from "../../data/api";

/** GET /api/clusters: what everyone may know of a cluster. */
export interface Cluster {
  name: string;
  displayName?: string;
  connected: boolean;
  paused?: boolean;
  lastSeen?: string;
  capabilities?: string[];
}

/** GET /api/admin/clusters adds what an admin manages. */
export interface AdminCluster extends Cluster {
  joined: boolean;
  tokenExpires?: string;
  agentVersion?: string;
  controllerVersion?: string;
  kubernetesVersion?: string;
  createdAt: string;
}

/** A join token is answered once, with the command to run in the cluster. */
export interface Joining {
  cluster: AdminCluster;
  joinToken: string;
  command: string;
}

/** A console that does not know the route (an older replica during an
 *  upgrade) is a console of one cluster. */
export const listClusters = () =>
  req<{ items: Cluster[] }>("GET", "/api/clusters").catch((e: unknown) => {
    if (e instanceof ApiFailure && e.status === 404) return { items: [] as Cluster[] };
    throw e;
  });
export const listAdminClusters = () => req<{ items: AdminCluster[] }>("GET", "/api/admin/clusters");
export const createCluster = (name: string, displayName: string) => req<Joining>("POST", "/api/admin/clusters", { name, displayName });
export const reissueClusterToken = (name: string) => req<Joining>("POST", `/api/admin/clusters/${name}/token`);
export const patchCluster = (name: string, body: { displayName?: string; paused?: boolean }) =>
  req<AdminCluster>("PATCH", `/api/admin/clusters/${name}`, body);
export const deleteCluster = (name: string) => req<void>("DELETE", `/api/admin/clusters/${name}`);

/** The rule the server holds a cluster's name to. */
export const validClusterName = (n: string) => n !== "devpod" && n.length <= 32 && /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(n);
