// Filters, sort, tab, time range and the open drawer live in the URL
// query (UI spec §3, §7.3). A schema converts both ways; defaults are
// left out of the URL and unknown keys are kept. A list page also keeps
// its choices in the browser (remember): an address that says none of
// them gets them back, so leaving the page and coming back finds it as it
// was; a link that says some is taken as it is.
import { useCallback, useEffect, useMemo, useRef } from "react";
import { useLocation, useNavigate, useSearchParams } from "react-router-dom";

export type Field =
  | { kind: "string"; default: string }
  | { kind: "number"; default: number }
  | { kind: "bool"; default: boolean }
  | { kind: "list"; default: string[] };
export type Schema = Record<string, Field>;
type ValueOf<F extends Field> = F["kind"] extends "string"
  ? string
  : F["kind"] extends "number"
    ? number
    : F["kind"] extends "bool"
      ? boolean
      : string[];
export type Values<S extends Schema> = { [K in keyof S]: ValueOf<S[K]> };

export function decodeQuery<S extends Schema>(p: URLSearchParams, s: S): Values<S> {
  const out = {} as Record<string, unknown>;
  for (const [k, f] of Object.entries(s)) {
    const raw = p.get(k);
    if (raw === null) {
      out[k] = f.default;
      continue;
    }
    switch (f.kind) {
      case "string":
        out[k] = raw;
        break;
      case "number": {
        const n = Number(raw);
        out[k] = raw.trim() !== "" && Number.isFinite(n) ? n : f.default;
        break;
      }
      case "bool":
        out[k] = raw === "1" || raw === "true";
        break;
      case "list":
        out[k] = raw === "" ? [] : raw.split(",");
        break;
    }
  }
  return out as Values<S>;
}

function isDefault(f: Field, v: unknown): boolean {
  if (f.kind === "list") {
    const a = v as string[];
    return a.length === f.default.length && a.every((x, i) => x === f.default[i]);
  }
  return v === f.default;
}

export function encodeQuery<S extends Schema>(v: Values<S>, s: S, base?: URLSearchParams): URLSearchParams {
  const p = new URLSearchParams(base);
  for (const [k, f] of Object.entries(s)) {
    const val = (v as Record<string, unknown>)[k];
    if (val === undefined || isDefault(f, val)) {
      p.delete(k);
      continue;
    }
    switch (f.kind) {
      case "bool":
        p.set(k, "1");
        break;
      case "list":
        p.set(k, (val as string[]).join(","));
        break;
      default:
        p.set(k, String(val));
    }
  }
  return p;
}

/** Remember: keep a page's choices in this browser under key — all the
 *  schema's fields but omit (an open drawer, a group shown open: what
 *  one visit is about, not how the page is liked). */
export type Remember = { key: string; omit?: string[] };

export const viewKey = (k: string) => `devpod.view.${k}`;
function recall(key: string): URLSearchParams | undefined {
  try {
    const v = localStorage.getItem(viewKey(key));
    return v ? new URLSearchParams(v) : undefined;
  } catch {
    return undefined;
  }
}
function keep(key: string, p: URLSearchParams) {
  try {
    if (p.toString() === "") localStorage.removeItem(viewKey(key));
    else localStorage.setItem(viewKey(key), p.toString());
  } catch {
    // not kept: no matter
  }
}

/** withRemembered: p, or — when it says none of the remembered fields —
 *  p with what was remembered. */
export function withRemembered(p: URLSearchParams, fields: string[], key?: string): URLSearchParams {
  if (!key || fields.some((k) => p.has(k))) return p;
  const saved = recall(key);
  if (!saved) return p;
  const out = new URLSearchParams(p);
  for (const k of fields) {
    const v = saved.get(k);
    if (v !== null) out.set(k, v);
  }
  return out;
}

/** How a change enters the history. A filter replaces the entry (typing is
 *  no step back). "push": a step of its own — a drawer opened, a tab
 *  chosen — so back returns from it. "back": undo such a step (close the
 *  drawer): go back when this entry is one we pushed, so forward reopens
 *  it and no second entry is left; replace otherwise (a link opened it). */
export type How = "push" | "back";
const pushed = new Set<string>(); // the history entries our pushes made

/** useUrlState reads the schema's values from the URL; set() merges a
 *  patch and replaces the history entry (or see How). With remember, the
 *  values are also kept in the browser and come back to an address
 *  without them. */
export function useUrlState<S extends Schema>(s: S, remember?: Remember): [Values<S>, (patch: Partial<Values<S>>, how?: How) => void] {
  const [params, setParams] = useSearchParams();
  const nav = useNavigate();
  const here = useLocation().key;
  const hereRef = useRef(here);
  hereRef.current = here;
  const pushing = useRef(false);
  useEffect(() => {
    if (!pushing.current) return;
    pushing.current = false;
    pushed.add(here);
  }, [here]);
  // by value: callers may pass a new object each render
  const key = remember?.key;
  const omit = remember?.omit?.join(",") ?? "";
  const fields = useMemo(() => Object.keys(s).filter((k) => !omit.split(",").includes(k)), [s, omit]);
  const effective = useMemo(() => withRemembered(params, fields, key), [params, fields, key]);
  const values = useMemo(() => decodeQuery(effective, s), [effective, s]);
  // the address says what is shown (a link copied now keeps it)
  const restored = effective !== params;
  useEffect(() => {
    if (restored) setParams(effective, { replace: true });
  }, [restored, effective, setParams]);
  const set = useCallback(
    (patch: Partial<Values<S>>, how?: How) => {
      if (how === "back" && pushed.has(hereRef.current)) return nav(-1);
      if (how === "push") pushing.current = true;
      setParams(
        (cur) => {
          const base = withRemembered(cur, fields, key);
          const next = encodeQuery({ ...decodeQuery(base, s), ...patch }, s, base);
          if (key) keep(key, new URLSearchParams([...next].filter(([k]) => fields.includes(k))));
          return next;
        },
        { replace: how !== "push" },
      );
    },
    [setParams, nav, s, fields, key],
  );
  return [values, set];
}
