import { apiPath } from "../../data/cluster";
// Saved configs (internal/webui/api_configs.go, config_launch.go): DevPod
// YAML with variables, kept per user, shared or public, launched as one
// DevPod or several.
import { ApiFailure, req, type ApiError } from "../../data/api";

/** What a variable is: it picks the launch form's control and how a value is checked. */
export type VarType = "string" | "int" | "choice" | "node" | "image";
/** A variable as the text has it (vars.ts reads the same). */
export interface ParsedVar {
  name: string;
  builtin: boolean;
  /** the person launching has to give it */
  required: boolean;
  default?: string;
}
/** A variable with what the config says of it. */
export interface ConfigVar extends ParsedVar {
  description?: string;
  type: VarType;
  /** a choice's options */
  options?: string[];
}
/** What a config says of one of its variables (saved with it). */
export type VarDecl = { name: string; description?: string; type?: VarType; options?: string[] };
export interface ConfigSummary {
  name: string;
  owner: string;
  title: string;
  description: string;
  /** empty unless the config is mine (or I am an admin) */
  sharedWith: string[];
  public: boolean;
  /** marked by an admin; listed first */
  official: boolean;
  variables: ConfigVar[];
  mine: boolean;
  updatedAt: string;
}
export interface Config extends ConfigSummary {
  content: string;
}
export type ConfigScope = "mine" | "shared" | "public" | "official";
export type ConfigCreate = {
  id: string;
  title: string;
  description?: string;
  content: string;
  sharedWith?: string[];
  public?: boolean;
  variables?: VarDecl[];
  official?: boolean;
};
export type ConfigPatch = Partial<Pick<Config, "title" | "description" | "content" | "sharedWith" | "public" | "official">> & { variables?: VarDecl[] };

const path = (name: string) => `/api/configs/${encodeURIComponent(name)}`;
export const listConfigs = (scope?: ConfigScope) => req<{ items: ConfigSummary[] }>("GET", `/api/configs${scope ? `?scope=${scope}` : ""}`);
export const getConfig = (name: string) => req<Config>("GET", path(name));
export const createConfig = (body: ConfigCreate) => req<Config>("POST", "/api/configs", body);
export const patchConfig = (name: string, body: ConfigPatch) => req<Config>("PATCH", path(name), body);
export const deleteConfig = (name: string) => req<void>("DELETE", path(name));

/** checkConfig: what is wrong with a config's text, before it is saved (nothing: an empty list). */
export type CheckProblem = { line?: number; code: string; message: string };
export const checkConfig = (body: { content: string; variables?: VarDecl[]; vars?: Record<string, string> }) =>
  req<{ problems: CheckProblem[] }>("POST", "/api/configs:check", body);

export type LaunchItem = { index: number; name: string; node?: string; ok: boolean; code?: string; message?: string; yaml?: string };
export type LaunchBody = { vars: Record<string, string>; nodes?: string[]; count?: number; dryRun?: boolean };
/** 200: all created (or, on a dry run, would be). 207: some were. 422:
 *  the check refused the launch and nothing was created. */
export type LaunchResult = { status: 200 | 207 | 422; items: LaunchItem[]; group?: string };

/** launchConfig creates DevPods from a config. The three answers that
 *  carry per-DevPod outcomes are returned; anything else throws. */
export async function launchConfig(name: string, body: LaunchBody): Promise<LaunchResult> {
  const resp = await fetch(apiPath(`${path(name)}/launch`), { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
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
  const group = (parsed as { group?: string } | null)?.group;
  if ((resp.status === 200 || resp.status === 207 || resp.status === 422) && Array.isArray(items)) return { status: resp.status, items, ...(group ? { group } : {}) };
  const err = parsed as ApiError | null;
  throw new ApiFailure(resp.status, err && typeof err.code === "string" ? err : { code: `HTTP_${resp.status}`, message: `${resp.status} ${resp.statusText}`.trim() });
}
