import { describe, expect, it } from "vitest";
import { decodeQuery, encodeQuery, type Schema } from "./urlState";

const schema = {
  q: { kind: "string", default: "" },
  page: { kind: "number", default: 1 },
  all: { kind: "bool", default: false },
  phase: { kind: "list", default: [] as string[] },
} satisfies Schema;

describe("URL state", () => {
  it("round-trips every kind", () => {
    const v = { q: "user:alice gpu>0", page: 3, all: true, phase: ["Running", "Failed"] };
    const p = encodeQuery(v, schema);
    expect(p.get("phase")).toBe("Running,Failed");
    expect(decodeQuery(new URLSearchParams(p.toString()), schema)).toEqual(v);
  });
  it("omits defaults", () => {
    const p = encodeQuery({ q: "", page: 1, all: false, phase: [] }, schema);
    expect(p.toString()).toBe("");
  });
  it("keeps unknown keys", () => {
    const p = encodeQuery({ q: "x", page: 1, all: false, phase: [] }, schema, new URLSearchParams("node=g206&q=old"));
    expect(p.get("node")).toBe("g206");
    expect(p.get("q")).toBe("x");
  });
  it("invalid number falls back to the default", () => {
    expect(decodeQuery(new URLSearchParams("page=abc&all=1"), schema)).toEqual({ q: "", page: 1, all: true, phase: [] });
  });
});
