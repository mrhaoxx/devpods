// Access control (design 2026-11-16): the ordered rules, the user groups
// they name, and the check of the running DevPods.
import { req } from "../../data/api";
import { apiIn } from "../../data/cluster";

export type HostPathGrant = { path: string; writable?: boolean };
export type AccessEffect = { allow: boolean; privileged?: boolean; hostPaths?: HostPathGrant[]; needsReservation?: boolean };
export type AccessSubject = { kind: "everyone" | "user" | "group"; name?: string };
export type AccessRule = { cluster?: string; machines?: string[]; subject: AccessSubject; effect: AccessEffect };
/** The whole ordered list, with the version it was read at (sent back on save). */
export type AccessPolicy = { rules: AccessRule[]; default: AccessEffect; resourceVersion: string };

export const getAccess = () => req<AccessPolicy>("GET", "/api/admin/access");
export const putAccess = (p: AccessPolicy) => req<AccessPolicy>("PUT", "/api/admin/access", p);

export type MatchItem = { cluster?: string; count: number; names: string[]; unreservable: number };
/** matchMachines: the machines a selector names, per cluster (one cluster, or all). */
export const matchMachines = (cluster: string | undefined, machines: string[]) =>
  req<{ items: MatchItem[] }>("GET", apiIn(cluster, `/api/admin/access/match?machines=${encodeURIComponent(machines.join(","))}`));

export type Violation = { cluster?: string; devpod: string; owner: string; node: string; code: string; reason: string; rule: number };
export const checkRunning = () => req<{ items: Violation[] }>("POST", "/api/admin/access/check");

export type Group = { name: string; displayName?: string; members: string[] };
export const listGroups = () => req<{ items: Group[] }>("GET", "/api/admin/groups");
export const createGroup = (g: Group) => req<Group>("POST", "/api/admin/groups", g);
export const updateGroup = (g: Group) => req<Group>("PUT", `/api/admin/groups/${encodeURIComponent(g.name)}`, { displayName: g.displayName ?? "", members: g.members });
export const deleteGroup = (name: string) => req<void>("DELETE", `/api/admin/groups/${encodeURIComponent(name)}`);

/** splitList: "a, b c" → ["a", "b", "c"]. */
export const splitList = (s: string) => s.split(/[\s,，]+/).filter(Boolean);

export const resourceText = (r: AccessRule) =>
  [r.cluster ? `集群 ${r.cluster}` : "任何集群", r.machines?.length ? `机器 ${r.machines.join(" ")}` : "全部机器"].join(" · ");

export const subjectText = (s: AccessSubject, groups: Group[] = []) => {
  if (s.kind === "everyone") return "所有人";
  if (s.kind === "user") return `用户 ${s.name}`;
  const g = groups.find((x) => x.name === s.name);
  return `组 ${s.name}${g?.displayName ? `（${g.displayName}）` : ""}`;
};

export const effectText = (e: AccessEffect) =>
  !e.allow
    ? "拒绝"
    : [
        "允许",
        e.privileged ? "特权和主机" : null,
        e.needsReservation ? "需预约" : null,
        e.hostPaths?.length ? `主机路径 ${e.hostPaths.map((h) => `${h.path}（${h.writable ? "可写" : "只读"}）`).join("、")}` : null,
      ]
        .filter(Boolean)
        .join(" · ");
