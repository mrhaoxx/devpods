// The DevPod table's filter box (UI spec §5.1), terms separated by spaces,
// every one must hold ("+" only separates: "Running + gpu>0"):
//   gpu>N gpu>=N gpu<N gpu<=N gpu=N      the GPU count
//   phase:xx, or a bare Running / Pending / Stopped / Failed   the phase, exactly
//   name: owner: (user:) node: image: cluster: endpoint:   a substring of that field
//   any other word                       a substring of the name, owner, node, image or cluster
// Case never matters; "quoted text" is one term; a term that fits no rule
// (gpu>x, foo:bar) is a word like any other — looked for, never dropped.
export type Cmp = ">" | ">=" | "<" | "<=" | "=";
type Field = "name" | "owner" | "node" | "image" | "cluster" | "endpoint";
type Term = { gpu: { op: Cmp; n: number } } | { phase: string } | { field: Field; v: string } | { word: string };
export type Filter = Term[];

/** What a row says of itself, to be filtered. */
export type Filterable = { name: string; owner: string; node?: string; phase?: string; image?: string; gpu: number; cluster?: string; endpoint?: string };

const PHASES = new Set(["running", "pending", "stopped", "failed"]);

function tokens(s: string): string[] {
  const out: string[] = [];
  const re = /"([^"]*)"|(\S+)/g;
  for (let m = re.exec(s); m; m = re.exec(s)) out.push(m[1] ?? m[2]);
  return out.filter((t) => t !== "" && t !== "+");
}

export function parseFilter(s: string): Filter {
  return tokens(s).map((t): Term => {
    const gpu = /^gpu(>=|<=|>|<|=)(\d+)$/i.exec(t);
    if (gpu) return { gpu: { op: gpu[1] as Cmp, n: Number(gpu[2]) } };
    const kv = /^(name|owner|user|node|image|cluster|endpoint|phase):(.+)$/i.exec(t);
    if (kv) {
      const key = kv[1].toLowerCase();
      if (key === "phase") return { phase: kv[2].toLowerCase() };
      return { field: (key === "user" ? "owner" : key) as Field, v: kv[2].toLowerCase() };
    }
    if (PHASES.has(t.toLowerCase())) return { phase: t.toLowerCase() };
    return { word: t.toLowerCase() };
  });
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

const has = (hay: string | undefined, needle: string) => (hay ?? "").toLowerCase().includes(needle);

export function matchesFilter(r: Filterable, f: Filter): boolean {
  return f.every((t) => {
    if ("gpu" in t) return cmp(r.gpu, t.gpu.op, t.gpu.n);
    if ("phase" in t) return (r.phase ?? "").toLowerCase() === t.phase;
    if ("field" in t) return has(r[t.field], t.v);
    return has(r.name, t.word) || has(r.owner, t.word) || has(r.node, t.word) || has(r.image, t.word) || has(r.cluster, t.word);
  });
}
