import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import { parseVars, usesVar } from "./vars";

// The same cases as the server's parser (internal/configvars): what the
// editor lists must be what a launch will ask for.
type Case = { name: string; content: string; vars: { name: string; builtin: boolean; required: boolean; default?: string }[] };
const cases = JSON.parse(readFileSync(new URL("../../../../internal/configvars/testdata/cases.json", import.meta.url), "utf8")) as Case[];

describe("config variables", () => {
  it("has the shared cases", () => {
    expect(cases.length).toBeGreaterThanOrEqual(10);
  });
  for (const c of cases) {
    it(`parses like the server: ${c.name}`, () => {
      expect(parseVars(c.content)).toEqual(c.vars);
      for (const v of c.vars) expect(usesVar(c.content, v.name)).toBe(true);
      expect(usesVar(c.content, "NOT_THERE")).toBe(false);
    });
  }
});
