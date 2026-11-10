import { describe, expect, it } from "vitest";
import { matchesFilter, parseFilter } from "./filter";

describe("filter syntax", () => {
  it("parses keys, comparisons and text", () => {
    expect(parseFilter("user:alice node:g206 gpu>0 train")).toEqual({
      user: "alice",
      node: "g206",
      gpu: { op: ">", n: 0 },
      text: ["train"],
    });
  });
  it('quoted text "a b" is one term', () => {
    expect(parseFilter('"my box" user:bob').text).toEqual(["my box"]);
  });
  it("unknown key foo:bar is text", () => {
    expect(parseFilter("foo:bar").text).toEqual(["foo:bar"]);
  });
  it("gpu>=2", () => {
    expect(parseFilter("gpu>=2").gpu).toEqual({ op: ">=", n: 2 });
  });
  it("empty → {text:[]}", () => {
    expect(parseFilter("   ")).toEqual({ text: [] });
  });
  const row = { name: "alice-train", owner: "alice", node: "g206", phase: "Running", image: "pytorch:2.6", gpu: 2 };
  it("phase:running matches Running", () => {
    expect(matchesFilter(row, parseFilter("phase:running"))).toBe(true);
    expect(matchesFilter(row, parseFilter("phase:stopped"))).toBe(false);
  });
  it("matchesFilter ANDs all terms", () => {
    expect(matchesFilter(row, parseFilter("user:alice gpu>1 pytorch"))).toBe(true);
    expect(matchesFilter(row, parseFilter("user:alice gpu>2"))).toBe(false);
    expect(matchesFilter(row, parseFilter("train g206"))).toBe(true);
    expect(matchesFilter(row, parseFilter("train g999"))).toBe(false);
  });
});
