// devpodToConfig turns a DevPod into the text of a config: its manifest
// without what the system filled in, with the name, the owner and the
// pinned node as variables. Text of the DevPod that happens to look like
// a variable (${HOME} in a script) is escaped so it stays text.
import type { DevPod } from "../../data/api";

const HOSTNAME = "kubernetes.io/hostname";

type Json = null | boolean | number | string | Json[] | { [k: string]: Json };

// (split/join: in a replacement string "$$" would mean one "$")
const literal = (s: string) => s.split("${").join("$${");
const escapeVars = (v: Json): Json => {
  if (typeof v === "string") return literal(v);
  if (Array.isArray(v)) return v.map(escapeVars);
  if (v && typeof v === "object") return Object.fromEntries(Object.entries(v).map(([k, x]) => [literal(k), escapeVars(x)]));
  return v;
};

// A string YAML reads back as that same string without quotes: starts
// with a letter, digit or one of / . _ $, holds nothing YAML gives a
// meaning to, and is not a number, a boolean or null.
const plainRE = /^[A-Za-z0-9_./$][A-Za-z0-9_./${}:@+=, -]*$/;
// (y and n are booleans to a YAML 1.1 reader, which the server's is)
const reservedRE = /^(true|false|yes|no|on|off|y|n|null|~|[-+]?(\d[\d_]*\.?\d*|\.\d+)([eE][-+]?\d+)?|0x[0-9a-fA-F]+|0o[0-7]+|0b[01_]+|[-+]?\.(inf|nan))$/i;
function plain(s: string): boolean {
  return plainRE.test(s) && !reservedRE.test(s) && !s.includes(": ") && !s.includes(" #") && !s.endsWith(":") && !s.endsWith(" ") && !s.includes(", ");
}

/** a script: its lines kept as lines (a block scalar), when every line can be */
function block(s: string): string[] | null {
  if (!s.includes("\n") || s.startsWith(" ") || s.startsWith("\n") || s.endsWith("\n\n") || /[\t\r]/.test(s)) return null;
  const lines = s.split("\n");
  const keep = s.endsWith("\n");
  if (keep) lines.pop();
  if (lines.some((l) => l !== l.trimEnd())) return null;
  return [keep ? "|" : "|-", ...lines];
}

const key = (k: string) => (/^[A-Za-z0-9_./-]+$/.test(k) && !reservedRE.test(k) ? k : JSON.stringify(k));
const empty = (v: Json) => (Array.isArray(v) ? v.length === 0 : !!v && typeof v === "object" && Object.keys(v).length === 0);
const scalar = (v: Json): string => (typeof v === "string" ? (plain(v) ? v : JSON.stringify(v)) : Array.isArray(v) ? "[]" : v && typeof v === "object" ? "{}" : String(v));

/** emit writes v as block YAML; every line is indented by `pad`. */
function emit(v: Json, pad: string, out: string[]) {
  if (Array.isArray(v)) {
    for (const item of v) {
      if (item && typeof item === "object" && !empty(item)) {
        const sub: string[] = [];
        emit(item, pad + "  ", sub);
        out.push(pad + "- " + sub[0].slice(pad.length + 2), ...sub.slice(1));
      } else entry(pad + "- ", pad + "  ", item, out);
    }
    return;
  }
  for (const [k, x] of Object.entries(v as { [k: string]: Json })) {
    if (x && typeof x === "object" && !empty(x)) {
      out.push(`${pad}${key(k)}:`);
      emit(x, pad + "  ", out);
    } else entry(`${pad}${key(k)}: `, pad + "  ", x, out);
  }
}

function entry(head: string, pad: string, v: Json, out: string[]) {
  const b = typeof v === "string" ? block(v) : null;
  if (!b) return void out.push(head + scalar(v));
  out.push(head + b[0], ...b.slice(1).map((l) => (l === "" ? "" : pad + l)));
}

// The CPU-binding annotations are stamped by a template and refused
// when a user writes them: a config carrying them could not be launched.
const STAMPED = "kore.zjusct.io/";
type PodPart = { metadata?: { annotations?: Record<string, Json> } & Record<string, Json> };
function dropStamped(pod: PodPart | undefined) {
  const meta = pod?.metadata;
  if (!pod || !meta?.annotations) return;
  for (const k of Object.keys(meta.annotations)) if (k.startsWith(STAMPED)) delete meta.annotations[k];
  if (Object.keys(meta.annotations).length === 0) delete meta.annotations;
  if (Object.keys(meta).length === 0) delete pod.metadata;
}

// The console's "not on these machines" (access control): written into
// the DevPod each time it starts, recognised by the annotation naming its
// machines; a config made from the DevPod must not carry it.
const EXCLUDED = "devpod.io/access-excluded";
type Term = { matchExpressions?: { key?: string; operator?: string; values?: string[] }[] } & Record<string, Json>;
type Affinity = { nodeAffinity?: { requiredDuringSchedulingIgnoredDuringExecution?: { nodeSelectorTerms?: Term[] } } & Record<string, Json> } & Record<string, Json>;
function dropExclusion(pod: { spec?: { affinity?: Affinity } } | undefined, names: string | undefined) {
  const spec = pod?.spec;
  const sel = spec?.affinity?.nodeAffinity?.requiredDuringSchedulingIgnoredDuringExecution;
  if (!spec || !names || !sel?.nodeSelectorTerms) return;
  const ex = names.split(",");
  const ours = (r: { key?: string; operator?: string; values?: string[] }) =>
    r.key === HOSTNAME && r.operator === "NotIn" && r.values?.length === ex.length && r.values.every((v, i) => v === ex[i]);
  sel.nodeSelectorTerms = sel.nodeSelectorTerms
    .map((t) => ({ ...t, matchExpressions: (t.matchExpressions ?? []).filter((r) => !ours(r)) }))
    .filter((t) => t.matchExpressions.length > 0 || Object.keys(t).some((k) => k !== "matchExpressions"));
  if (sel.nodeSelectorTerms.length === 0) delete spec.affinity!.nodeAffinity!.requiredDuringSchedulingIgnoredDuringExecution;
  if (Object.keys(spec.affinity!.nodeAffinity!).length === 0) delete spec.affinity!.nodeAffinity;
  if (Object.keys(spec.affinity!).length === 0) delete spec.affinity;
}

export function devpodToConfig(dp: DevPod): string {
  const src = dp as unknown as { apiVersion?: string; kind?: string; spec: Record<string, Json> };
  const { owner: _owner, ...rest } = escapeVars(src.spec as Json) as Record<string, Json>;
  void _owner;
  dropStamped(rest.pod as PodPart | undefined);
  dropExclusion(rest.pod as { spec?: { affinity?: Affinity } } | undefined, dp.metadata.annotations?.[EXCLUDED]);
  const sel = ((rest.pod as { spec?: { nodeSelector?: Record<string, Json> } } | undefined)?.spec ?? {}).nodeSelector;
  if (sel && HOSTNAME in sel) sel[HOSTNAME] = "${NODE}";
  const doc: Json = {
    apiVersion: src.apiVersion ?? "devpod.io/v1alpha1",
    kind: src.kind ?? "DevPod",
    metadata: { name: "${NAME}" },
    spec: { owner: "${OWNER}", ...rest },
  };
  const out: string[] = [];
  emit(doc, "", out);
  return out.join("\n") + "\n";
}
