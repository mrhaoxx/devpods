import { describe, expect, it } from "vitest";
import { matchesFilter, parseFilter } from "./filter";

const train = { name: "alice-train", owner: "alice", node: "g206", phase: "Running", image: "pytorch:2.6", gpu: 2, cluster: "zjusct", endpoint: "10.0.0.7:2222" };
const idle = { name: "bob-box", owner: "bob", node: "", phase: "Stopped", image: "ubuntu:24.04", gpu: 0, cluster: "zjusct-vk8s", endpoint: "" };
const hits = (q: string) => [train, idle].filter((r) => matchesFilter(r, parseFilter(q))).map((r) => r.name);

describe("filter syntax", () => {
  it("every term must hold; a + only separates", () => {
    expect(hits("Running + gpu>0")).toEqual(["alice-train"]);
    expect(hits("running gpu>2")).toEqual([]);
    expect(hits("   ")).toEqual(["alice-train", "bob-box"]);
  });

  it("a bare phase word is the phase, exactly", () => {
    expect(hits("stopped")).toEqual(["bob-box"]);
    expect(hits("phase:RUNNING")).toEqual(["alice-train"]);
    expect(hits("phase:run")).toEqual([]);
  });

  it("gpu compares the count", () => {
    expect(hits("gpu=0")).toEqual(["bob-box"]);
    expect(hits("gpu>=2")).toEqual(["alice-train"]);
    expect(hits("gpu<1")).toEqual(["bob-box"]);
  });

  it("field:value is a substring of that field, any case; user: is owner:", () => {
    expect(hits("owner:ALI")).toEqual(["alice-train"]);
    expect(hits("user:bo")).toEqual(["bob-box"]);
    expect(hits("name:box")).toEqual(["bob-box"]);
    expect(hits("image:pytorch")).toEqual(["alice-train"]);
    expect(hits("node:g20")).toEqual(["alice-train"]);
    expect(hits("cluster:vk8s")).toEqual(["bob-box"]);
    expect(hits("endpoint:10.0.0")).toEqual(["alice-train"]);
    // two of a key both hold
    expect(hits("owner:ali owner:bob")).toEqual([]);
  });

  it("a bare word is in the name, the owner, the node, the image or the cluster", () => {
    expect(hits("train")).toEqual(["alice-train"]);
    expect(hits("bob")).toEqual(["bob-box"]);
    expect(hits("pytorch")).toEqual(["alice-train"]);
    expect(hits("g206")).toEqual(["alice-train"]);
    expect(hits("vk8s")).toEqual(["bob-box"]);
    expect(hits("10.0.0")).toEqual([]); // the endpoint only by endpoint:
  });

  it("what fits no rule is a word; quotes keep spaces", () => {
    expect(hits("gpu>x")).toEqual([]); // looked for as text, not dropped
    expect(parseFilter('"alice tr"')).toHaveLength(1);
    expect(hits("foo:bar")).toEqual([]);
  });
});
