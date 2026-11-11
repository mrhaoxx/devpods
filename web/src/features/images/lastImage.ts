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
