import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { recentConfigs, rememberConfig } from "./recentConfigs";

// node has no localStorage: a small one for the test
const store = new Map<string, string>();
beforeEach(() => {
  store.clear();
  (globalThis as { localStorage?: unknown }).localStorage = {
    getItem: (k: string) => store.get(k) ?? null,
    setItem: (k: string, v: string) => void store.set(k, v),
    removeItem: (k: string) => void store.delete(k),
  };
});
afterEach(() => delete (globalThis as { localStorage?: unknown }).localStorage);

describe("recent configs", () => {
  it("the last used first, each once", () => {
    rememberConfig("alice", "a", 1);
    rememberConfig("alice", "b", 2);
    rememberConfig("alice", "a", 3);
    expect(recentConfigs("alice")).toEqual([
      { name: "a", at: 3 },
      { name: "b", at: 2 },
    ]);
  });

  it("each user their own", () => {
    rememberConfig("alice", "a", 1);
    expect(recentConfigs("bob")).toEqual([]);
  });

  it("at most 50 kept", () => {
    for (let i = 0; i < 60; i++) rememberConfig("alice", `c${i}`, i);
    const r = recentConfigs("alice");
    expect(r).toHaveLength(50);
    expect(r[0].name).toBe("c59");
  });

  it("what cannot be read is nothing; a storage that throws is no matter", () => {
    store.set("devpod.recentConfigs.alice", "not json");
    expect(recentConfigs("alice")).toEqual([]);
    (globalThis as { localStorage?: unknown }).localStorage = {
      getItem: () => {
        throw new Error("denied");
      },
      setItem: () => {
        throw new Error("denied");
      },
    };
    expect(recentConfigs("alice")).toEqual([]);
    expect(() => rememberConfig("alice", "a")).not.toThrow();
  });
});
