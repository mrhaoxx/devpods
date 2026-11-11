// The image address last used to save a DevPod (自定义地址), remembered in
// this browser: per DevPod, falling back to the user's last one. Only the
// address is kept — never credentials. Storage may be unavailable
// (private windows, blocked site data): every access is guarded.
const key = (user: string, devpod?: string) => `devpod.snapshot.image.${user}${devpod ? `.${devpod}` : ""}`;

function store(): Storage | undefined {
  try {
    return window.localStorage;
  } catch {
    return undefined;
  }
}

/** lastImage: what this user last saved this DevPod as, else their last address anywhere. */
export function lastImage(user: string, devpod: string): string {
  if (!user) return "";
  try {
    const s = store();
    return s?.getItem(key(user, devpod)) ?? s?.getItem(key(user)) ?? "";
  } catch {
    return "";
  }
}

export function rememberImage(user: string, devpod: string, image: string): void {
  if (!user || !image) return;
  try {
    const s = store();
    s?.setItem(key(user, devpod), image);
    s?.setItem(key(user), image);
  } catch {
    /* not remembered: the field just starts empty next time */
  }
}

/* ---- tags ---- */

/** How the tag of a saved image is chosen: keep the one given (saving to
 *  the same address again overwrites it) or a new one from the clock. */
export type TagMode = "keep" | "new";
const tagModeKey = (user: string) => `devpod.snapshot.tagMode.${user}`;

export function lastTagMode(user: string): TagMode {
  try {
    return user && store()?.getItem(tagModeKey(user)) === "new" ? "new" : "keep";
  } catch {
    return "keep";
  }
}

export function rememberTagMode(user: string, mode: TagMode): void {
  if (!user) return;
  try {
    store()?.setItem(tagModeKey(user), mode);
  } catch {
    /* not remembered */
  }
}

const pad = (n: number) => String(n).padStart(2, "0");
/** timestampTag: 20261111-163012, in the browser's local time. */
export function timestampTag(d: Date): string {
  return `${d.getFullYear()}${pad(d.getMonth() + 1)}${pad(d.getDate())}-${pad(d.getHours())}${pad(d.getMinutes())}${pad(d.getSeconds())}`;
}

/** withTag replaces (or adds) the tag of an image reference; the port of
 *  a registry host is not a tag. */
export function withTag(image: string, tag: string): string {
  const slash = image.lastIndexOf("/");
  const colon = image.indexOf(":", slash + 1);
  return `${colon < 0 ? image : image.slice(0, colon)}:${tag}`;
}
