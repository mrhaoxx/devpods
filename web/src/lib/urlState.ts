// Filters, sort, tab, time range and the open drawer live in the URL
// query (UI spec §3, §7.3). A schema converts both ways; defaults are
// left out of the URL and unknown keys are kept.
import { useCallback, useMemo } from "react";
import { useSearchParams } from "react-router-dom";

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

/** useUrlState reads the schema's values from the URL; set() merges a
 *  patch and replaces the history entry. */
export function useUrlState<S extends Schema>(s: S): [Values<S>, (patch: Partial<Values<S>>) => void] {
  const [params, setParams] = useSearchParams();
  const values = useMemo(() => decodeQuery(params, s), [params, s]);
  const set = useCallback(
    (patch: Partial<Values<S>>) =>
      setParams((cur) => encodeQuery({ ...decodeQuery(cur, s), ...patch }, s, cur), { replace: true }),
    [setParams, s],
  );
  return [values, set];
}
