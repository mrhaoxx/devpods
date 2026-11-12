// The configs I used last, kept in this browser (per user): the new-
// DevPod drawer lists them first. A storage that cannot be read or
// written (a private window) is as if nothing was used.
export type Recent = { name: string; at: number };

const MAX = 50;
const key = (user: string) => `devpod.recentConfigs.${user}`;

export function recentConfigs(user: string): Recent[] {
  try {
    const raw = JSON.parse(localStorage.getItem(key(user)) ?? "[]");
    return Array.isArray(raw) ? raw.filter((r): r is Recent => typeof r?.name === "string" && typeof r?.at === "number") : [];
  } catch {
    return [];
  }
}

/** rememberConfig: name was just used. */
export function rememberConfig(user: string, name: string, at = Date.now()): void {
  const next = [{ name, at }, ...recentConfigs(user).filter((r) => r.name !== name)].slice(0, MAX);
  try {
    localStorage.setItem(key(user), JSON.stringify(next));
  } catch {
    // not kept: no matter
  }
}
