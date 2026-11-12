import { describe, expect, it } from "vitest";
import { provenance } from "./provenance";
import type { DevPod } from "../../data/api";

const dp = (labels?: Record<string, string>, annotations?: Record<string, string>): DevPod => ({
  metadata: { name: "alice-t-1", labels, annotations },
  spec: { owner: "alice", running: true },
});

describe("provenance", () => {
  it("reads where a DevPod came from", () => {
    expect(
      provenance(
        dp(
          { "devpod.io/config": "root-train", "devpod.io/group": "alice-t" },
          { "devpod.io/group-index": "1", "devpod.io/group-size": "3", "devpod.io/group-leader": "alice-t-0" },
        ),
      ),
    ).toEqual({ config: "root-train", group: "alice-t", index: 1, size: 3, leader: "alice-t-0" });
  });

  it("has nothing to say of a DevPod made by hand", () => {
    expect(provenance(dp())).toEqual({});
  });

  it("a number that is not one is left out", () => {
    expect(provenance(dp({ "devpod.io/group": "g" }, { "devpod.io/group-index": "x", "devpod.io/group-size": "" }))).toEqual({ group: "g" });
  });
});
