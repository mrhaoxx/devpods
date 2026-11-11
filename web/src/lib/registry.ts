// Registry hosts: which registry an image reference points at, and
// tidying what a user pastes as a registry address (snapshot spec v2 §5).

const HUB = "docker.io";
const HUB_ALIASES = new Set(["docker.io", "index.docker.io", "registry-1.docker.io"]);

/** imageRegistryHost: the registry of an image reference, lower-cased.
 *  The part before the first "/" is a registry when it has a "." or a
 *  ":" or is "localhost"; anything else is a Docker Hub path. */
export function imageRegistryHost(ref: string): string {
  const slash = ref.indexOf("/");
  if (slash < 0) return HUB;
  const first = ref.slice(0, slash).toLowerCase();
  return first.includes(".") || first.includes(":") || first === "localhost" ? first : HUB;
}

/** sameRegistry: host equality, with Docker Hub's names counted as one. */
export function sameRegistry(a: string, b: string): boolean {
  const x = a.toLowerCase();
  const y = b.toLowerCase();
  return x === y || (HUB_ALIASES.has(x) && HUB_ALIASES.has(y));
}

/** savedRegistryFor: the saved registry an image would be pushed with. */
export function savedRegistryFor<T extends { host: string }>(image: string, saved: readonly T[]): T | undefined {
  if (!image.includes("/")) return undefined; // not an address yet
  const host = imageRegistryHost(image);
  return saved.find((r) => sameRegistry(r.host, host));
}

/** cleanRegistryHost: what a pasted address means as host[:port] — the
 *  scheme, any path and surrounding space dropped, lower-cased. A hint
 *  only; the server validates. */
export function cleanRegistryHost(input: string): string {
  let s = input.trim().toLowerCase();
  s = s.replace(/^[a-z][a-z0-9+.-]*:\/\//, "");
  const slash = s.indexOf("/");
  if (slash >= 0) s = s.slice(0, slash);
  return s;
}

/** A plausible host[:port]. */
export const REGISTRY_HOST_RE = /^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?$/;
