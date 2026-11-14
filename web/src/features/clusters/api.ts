import { ApiFailure, req } from "../../data/api";

/** GET /api/clusters: what everyone may know of a cluster. */
export interface Cluster {
  name: string;
  displayName?: string;
  connected: boolean;
  paused?: boolean;
  lastSeen?: string;
  capabilities?: string[];
  /** the gateway whose SSH address and port-link domain serve this cluster */
  gateway?: string;
}

/** A link as the end that accepted it sees it: since when, the latest
 *  round trip (absent: the other end does not answer pings), and bytes
 *  received (rx) and sent (tx) — in all and per second. */
export interface LinkStats {
  since: string;
  rttMs?: number;
  rxRate: number;
  txRate: number;
  rx: number;
  tx: number;
  streams: number;
  idle: number;
}

/** GET /api/admin/clusters adds what an admin manages. */
export interface AdminCluster extends Cluster {
  joined: boolean;
  tokenExpires?: string;
  agentVersion?: string;
  controllerVersion?: string;
  kubernetesVersion?: string;
  createdAt: string;
  /** its agent's link to the console replica that answered */
  link?: LinkStats;
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

/** GET /api/admin/gateways: a gateway of several clusters. */
export interface AdminGateway {
  name: string;
  displayName?: string;
  sshAddress?: string;
  httpBaseDomain?: string;
  httpSuffix?: string;
  httpPort?: number;
  agentAddress?: string;
  paused?: boolean;
  joined: boolean;
  tokenExpires?: string;
  lastSeen?: string;
  version?: string;
  clusters?: string[];
  createdAt: string;
  /** its replicas linked to the console replica that answered */
  replicas?: GatewayReplica[];
}

/** An SSH session a gateway replica carries. */
export interface GatewaySession {
  id: string;
  user: string;
  devpod: string;
  cluster?: string;
  client: string;
  kind?: string;
  pubkeyFp?: string;
  since: string;
  up: number;
  down: number;
}
/** What a gateway replica holds now. */
export interface GatewayStatus {
  instance: string;
  version?: string;
  clusters: (LinkStats & { name: string })[];
  sessions: GatewaySession[];
}
export interface GatewayReplica {
  pod: string;
  /** its link to the console replica that answered */
  link: LinkStats;
  status?: GatewayStatus;
  /** why it said nothing (a gateway of before v0.9.23 has no status) */
  error?: string;
}
export interface GatewayJoining {
  gateway: AdminGateway;
  joinToken: string;
  command: string;
}
export type GatewayFields = Partial<Pick<AdminGateway, "displayName" | "sshAddress" | "httpBaseDomain" | "httpSuffix" | "httpPort" | "agentAddress" | "paused">>;

export const listAdminGateways = () => req<{ items: AdminGateway[] }>("GET", "/api/admin/gateways");
export const createGateway = (name: string, fields: GatewayFields) => req<GatewayJoining>("POST", "/api/admin/gateways", { name, ...fields });
export const patchGateway = (name: string, fields: GatewayFields) => req<AdminGateway>("PATCH", `/api/admin/gateways/${name}`, fields);
export const reissueGatewayToken = (name: string) => req<GatewayJoining>("POST", `/api/admin/gateways/${name}/token`);
export const deleteGateway = (name: string) => req<void>("DELETE", `/api/admin/gateways/${name}`);
export const setClusterGateway = (cluster: string, gateway: string) => req<AdminCluster>("PATCH", `/api/admin/clusters/${cluster}`, { gateway });

/** The rule the server holds a cluster's name to. */
export const validClusterName = (n: string) => n !== "devpod" && n.length <= 32 && /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(n);
