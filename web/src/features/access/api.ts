// Access control (design 2026-11-16, conditions and privileges one by one
// 2026-11-19): the ordered rules, the user groups they name, and the check
// of the running DevPods.
import { req } from "../../data/api";

export type HostPathGrant = { path: string; writable?: boolean };
/** what beyond an ordinary container a rule lets be asked for, each on its own */
export type AccessPrivileges = { privileged?: boolean; capabilities?: string[]; hostPID?: boolean; hostIPC?: boolean; hostNetwork?: boolean };
export type AccessEffect = { allow: boolean; privileges?: AccessPrivileges; hostPaths?: HostPathGrant[]; needsReservation?: boolean };
/** a rule: a condition on the cluster, machine, user and group ("": everything) and what those it holds for get */
export type AccessRule = { match: string; effect: AccessEffect };
/** a rule that denies everyone everything: it cannot be read, or names a group that is not there */
export type AccessProblem = { rule: number; message: string };
/** The whole ordered list, with the version it was read at (sent back on save). */
export type AccessPolicy = { rules: AccessRule[]; default: AccessEffect; resourceVersion: string; problems?: AccessProblem[] };

export const getAccess = () => req<AccessPolicy>("GET", "/api/admin/access");
export const putAccess = (p: AccessPolicy) => req<AccessPolicy>("PUT", "/api/admin/access", p);

/** what a condition names before it is saved: "maybe" where it depends on what is not known */
export type Preview =
  | { error: { offset: number; message: string } }
  | {
      error?: undefined;
      clusters: { name: string; maybe: boolean }[];
      machines: { cluster?: string; count: number; maybe: number; names: string[]; unreservable: number }[];
      users: { count: number; maybe: number; total: number; names: string[] };
      hasMachine: boolean;
    };
export const previewMatch = (match: string) => req<Preview>("POST", "/api/admin/access/preview", { match });

export type Violation = { cluster?: string; devpod: string; owner: string; node: string; code: string; reason: string; rule: number };
export const checkRunning = () => req<{ items: Violation[] }>("POST", "/api/admin/access/check");

export type Group = { name: string; displayName?: string; members: string[] };
export const listGroups = () => req<{ items: Group[] }>("GET", "/api/admin/groups");
export const createGroup = (g: Group) => req<Group>("POST", "/api/admin/groups", g);
export const updateGroup = (g: Group) => req<Group>("PUT", `/api/admin/groups/${encodeURIComponent(g.name)}`, { displayName: g.displayName ?? "", members: g.members });
export const deleteGroup = (name: string) => req<void>("DELETE", `/api/admin/groups/${encodeURIComponent(name)}`);

/** splitList: "a, b c" → ["a", "b", "c"]. */
export const splitList = (s: string) => s.split(/[\s,，]+/).filter(Boolean);

/** the privileges granted one by one (capabilities apart), and their names on the page */
export const PRIVILEGE_FLAGS = [
  ["privileged", "特权容器"],
  ["hostPID", "主机 PID"],
  ["hostIPC", "主机 IPC"],
  ["hostNetwork", "主机网络"],
] as const;

export const privilegesText = (p?: AccessPrivileges): string[] => {
  if (!p) return [];
  const out: string[] = PRIVILEGE_FLAGS.filter(([k]) => p[k]).map(([, t]) => t);
  const caps = p.capabilities ?? [];
  if (caps.includes("*")) out.push("任意 capability");
  else if (caps.length) out.push(`cap ${caps.join(" ")}`);
  return out;
};

/** cleanPrivileges: only what is granted (no capabilities under a privileged container, which has them all); none: undefined */
export const cleanPrivileges = (p?: AccessPrivileges): AccessPrivileges | undefined => {
  if (!p) return undefined;
  const out: AccessPrivileges = {};
  for (const [k] of PRIVILEGE_FLAGS) if (p[k]) out[k] = true;
  const caps = (p.capabilities ?? []).map((c) => c.trim()).filter(Boolean);
  if (caps.length && !p.privileged) out.capabilities = caps;
  return Object.keys(out).length ? out : undefined;
};

export const effectText = (e: AccessEffect) =>
  !e.allow
    ? "拒绝"
    : [
        "允许",
        ...privilegesText(e.privileges),
        e.needsReservation ? "需预约" : null,
        e.hostPaths?.length ? `主机路径 ${e.hostPaths.map((h) => `${h.path}（${h.writable ? "可写" : "只读"}）`).join("、")}` : null,
      ]
        .filter(Boolean)
        .join(" · ");

/* management grants (design 2026-11-20): each an AccessGrant of its own */
export type GrantPermission = "setReservable" | "viewAudit" | "disconnectSessions" | "manageReservations";
export const GRANT_PERMISSIONS: [GrantPermission, string][] = [
  ["setReservable", "切换可预约"],
  ["viewAudit", "看审计"],
  ["disconnectSessions", "断开会话"],
  ["manageReservations", "管理预约"],
];
export type Grant = { name: string; match: string; permissions: GrantPermission[]; description?: string; resourceVersion?: string; problem?: string };
export const listGrants = () => req<{ items: Grant[] }>("GET", "/api/admin/grants");
export const createGrant = (g: { name: string; match: string; permissions: GrantPermission[]; description?: string }) => req<Grant>("POST", "/api/admin/grants", g);
export const updateGrant = (g: { name: string; match: string; permissions: GrantPermission[]; description?: string; resourceVersion?: string }) =>
  req<Grant>("PUT", `/api/admin/grants/${encodeURIComponent(g.name)}`, { match: g.match, permissions: g.permissions, description: g.description ?? "", resourceVersion: g.resourceVersion ?? "" });
export const deleteGrant = (name: string) => req<void>("DELETE", `/api/admin/grants/${encodeURIComponent(name)}`);
export const grantText = (ps: GrantPermission[]) =>
  GRANT_PERMISSIONS.filter(([p]) => ps.includes(p))
    .map(([, t]) => t)
    .join(" · ");
