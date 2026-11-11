// Saved configs (internal/webui/api_configs.go, config_launch.go): DevPod
// YAML with variables, kept per user, shared or public, launched as one
// DevPod or several.
import { ApiFailure, req, type ApiError } from "../../data/api";

export interface ConfigVar {
  name: string;
  builtin: boolean;
  /** the person launching has to give it */
  required: boolean;
  default?: string;
}
export interface ConfigSummary {
  name: string;
  owner: string;
  title: string;
  description: string;
  /** empty unless the config is mine (or I am an admin) */
  sharedWith: string[];
  public: boolean;
  variables: ConfigVar[];
  mine: boolean;
  updatedAt: string;
}
export interface Config extends ConfigSummary {
  content: string;
}
export type ConfigScope = "mine" | "shared" | "public";
export type ConfigCreate = { id: string; title: string; description?: string; content: string; sharedWith?: string[]; public?: boolean };
export type ConfigPatch = Partial<Pick<Config, "title" | "description" | "content" | "sharedWith" | "public">>;

const path = (name: string) => `/api/configs/${encodeURIComponent(name)}`;
export const listConfigs = (scope?: ConfigScope) => req<{ items: ConfigSummary[] }>("GET", `/api/configs${scope ? `?scope=${scope}` : ""}`);
export const getConfig = (name: string) => req<Config>("GET", path(name));
export const createConfig = (body: ConfigCreate) => req<Config>("POST", "/api/configs", body);
export const patchConfig = (name: string, body: ConfigPatch) => req<Config>("PATCH", path(name), body);
export const deleteConfig = (name: string) => req<void>("DELETE", path(name));

export type LaunchItem = { index: number; name: string; node?: string; ok: boolean; code?: string; message?: string; yaml?: string };
export type LaunchBody = { vars: Record<string, string>; nodes?: string[]; count?: number; dryRun?: boolean };
/** 200: all created (or, on a dry run, would be). 207: some were. 422:
 *  the check refused the launch and nothing was created. */
export type LaunchResult = { status: 200 | 207 | 422; items: LaunchItem[] };

/** launchConfig creates DevPods from a config. The three answers that
 *  carry per-DevPod outcomes are returned; anything else throws. */
export async function launchConfig(name: string, body: LaunchBody): Promise<LaunchResult> {
  const resp = await fetch(`${path(name)}/launch`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
  if (resp.status === 401) {
    window.location.href = "/login";
    throw new ApiFailure(401, { code: "UNAUTHORIZED", message: "not logged in" });
  }
  let parsed: unknown;
  try {
    parsed = await resp.json();
  } catch {
    parsed = null;
  }
  const items = (parsed as { items?: LaunchItem[] } | null)?.items;
  if ((resp.status === 200 || resp.status === 207 || resp.status === 422) && Array.isArray(items)) return { status: resp.status, items };
  const err = parsed as ApiError | null;
  throw new ApiFailure(resp.status, err && typeof err.code === "string" ? err : { code: `HTTP_${resp.status}`, message: `${resp.status} ${resp.statusText}`.trim() });
}
