// The DevPod table's filter box (UI spec §5.1): `user:` `node:` `phase:`
// keys, `gpu` comparisons, and free text; all terms must match.
export type Cmp = ">" | ">=" | "<" | "<=" | "=";
export type Filter = { text: string[]; user?: string; node?: string; phase?: string; gpu?: { op: Cmp; n: number } };

function tokens(s: string): string[] {
  const out: string[] = [];
  const re = /"([^"]*)"|(\S+)/g;
  for (let m = re.exec(s); m; m = re.exec(s)) out.push(m[1] ?? m[2]);
  return out.filter((t) => t !== "");
}

export function parseFilter(s: string): Filter {
  const f: Filter = { text: [] };
  for (const t of tokens(s)) {
    const kv = /^(user|node|phase):(.+)$/i.exec(t);
    if (kv) {
      f[kv[1].toLowerCase() as "user" | "node" | "phase"] = kv[2];
      continue;
    }
    const gpu = /^gpu(>=|<=|>|<|=)(\d+)$/i.exec(t);
    if (gpu) {
      f.gpu = { op: gpu[1] as Cmp, n: Number(gpu[2]) };
      continue;
    }
    f.text.push(t);
  }
  return f;
}

function cmp(a: number, op: Cmp, b: number): boolean {
  switch (op) {
    case ">":
      return a > b;
    case ">=":
      return a >= b;
    case "<":
      return a < b;
    case "<=":
      return a <= b;
    default:
      return a === b;
  }
}

const has = (hay: string | undefined, needle: string) => (hay ?? "").toLowerCase().includes(needle.toLowerCase());

export function matchesFilter(
  r: { name: string; owner: string; node?: string; phase?: string; image?: string; gpu: number },
  f: Filter,
): boolean {
  if (f.user && r.owner.toLowerCase() !== f.user.toLowerCase()) return false;
  if (f.node && !has(r.node, f.node)) return false;
  if (f.phase && (r.phase ?? "").toLowerCase() !== f.phase.toLowerCase()) return false;
  if (f.gpu && !cmp(r.gpu, f.gpu.op, f.gpu.n)) return false;
  return f.text.every((t) => has(r.name, t) || has(r.owner, t) || has(r.node, t) || has(r.image, t));
}
