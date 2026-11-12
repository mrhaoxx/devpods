import { describe, expect, it } from "vitest";
import { applyWatchEvent } from "./queries";
import type { DevPod, DevPodList } from "./api";

const dp = (name: string, cluster?: string): DevPod => ({ metadata: { name }, spec: { owner: "alice", running: true }, ...(cluster ? { cluster } : {}) });
const pod = (node: string) => ({ node, ip: "10.0.0.1", startedAt: "2026-11-10T10:00:00Z" });

describe("applyWatchEvent", () => {
  it("in a console of several clusters: a same-named DevPod in another cluster is another item", () => {
    const list: DevPodList = { items: [dp("alice-a", "zjusct"), dp("alice-a", "vk8s")], pods: { "zjusct/alice-a": pod("g206") } };
    const next = applyWatchEvent(list, "MODIFIED", dp("alice-a", "zjusct"), pod("g207"));
    expect(next.items.map((d) => d.cluster)).toEqual(["zjusct", "vk8s"]);
    expect(next.pods).toEqual({ "zjusct/alice-a": pod("g207") });
    const gone = applyWatchEvent(next, "DELETED", dp("alice-a", "zjusct"));
    expect(gone.items.map((d) => d.cluster)).toEqual(["vk8s"]);
    expect(gone.pods).toEqual({});
  });

  it("a console of one cluster: by name", () => {
    const next = applyWatchEvent({ items: [], pods: {} }, "ADDED", dp("alice-a"), pod("g206"));
    expect(next.pods).toEqual({ "alice-a": pod("g206") });
  });
});
