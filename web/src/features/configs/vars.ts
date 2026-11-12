// The variables of a saved config, read exactly as the server reads
// them (internal/configvars — the two share testdata/cases.json):
//
//   ${VAR}            must be given
//   ${VAR:-default}   has a default, which ends at the first }
//   $${               a literal ${
//
// A name is [A-Z][A-Z0-9_]*; any other ${...} is text.
import type { ParsedVar } from "./api";

export const OWNER = "OWNER";
export const NAME = "NAME";
export const NODE = "NODE";
export const INDEX = "INDEX";
export const COUNT = "COUNT";
export const LEADER = "LEADER";
/** serverFilled: the server gives the value, never the person launching. */
export const serverFilled = (n: string) => n === OWNER || n === INDEX || n === COUNT || n === LEADER;
const builtin = (n: string) => n === NAME || n === NODE || serverFilled(n);

type Ref = { name: string; def: string; hasDefault: boolean };

const upper = (c: string) => c >= "A" && c <= "Z";
function nameEnd(s: string, i: number): number {
  if (i >= s.length || !upper(s[i])) return i;
  while (i < s.length && (upper(s[i]) || (s[i] >= "0" && s[i] <= "9") || s[i] === "_")) i++;
  return i;
}

/** scan walks the text once, left to right, and reports each ${VAR}. */
function scan(content: string, variable: (r: Ref) => void) {
  for (let i = 0; i < content.length; ) {
    if (content[i] !== "$") {
      i++;
      continue;
    }
    if (content.startsWith("$${", i)) {
      i += 3;
      continue;
    }
    if (!content.startsWith("${", i)) {
      i++;
      continue;
    }
    const end = nameEnd(content, i + 2);
    if (end === i + 2 || end >= content.length) {
      i++;
      continue;
    }
    const r: Ref = { name: content.slice(i + 2, end), def: "", hasDefault: false };
    if (content[end] === "}") i = end + 1;
    else if (content.startsWith(":-", end)) {
      const close = content.indexOf("}", end);
      if (close < 0) {
        i++;
        continue;
      }
      r.def = content.slice(end + 2, close);
      r.hasDefault = true;
      i = close + 1;
    } else {
      i++;
      continue;
    }
    variable(r);
  }
}

/** parseVars lists the variables in order of first appearance; the first
 *  default a variable carries anywhere is its default everywhere. */
export function parseVars(content: string): ParsedVar[] {
  const order: string[] = [];
  const by = new Map<string, Ref>();
  scan(content, (r) => {
    const have = by.get(r.name);
    if (!have) {
      order.push(r.name);
      by.set(r.name, r);
    } else if (r.hasDefault && !have.hasDefault) {
      have.def = r.def;
      have.hasDefault = true;
    }
  });
  return order.map((n) => {
    const r = by.get(n)!;
    const v: ParsedVar = { name: n, builtin: builtin(n), required: !r.hasDefault && !serverFilled(n) };
    if (r.def !== "") v.default = r.def;
    return v;
  });
}

/** usesVar: the text has the variable. */
export function usesVar(content: string, name: string): boolean {
  let found = false;
  scan(content, (r) => {
    if (r.name === name) found = true;
  });
  return found;
}
