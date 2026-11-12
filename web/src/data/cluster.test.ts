import { describe, expect, it } from "vitest";
import { apiIn, devpodHref, keyOf } from "./cluster";

describe("addressing", () => {
  it("an object's cluster is in its paths, keys and page", () => {
    expect(apiIn("a", "/api/devpods/x")).toBe("/api/clusters/a/devpods/x");
    expect(apiIn(undefined, "/api/devpods/x")).toBe("/api/devpods/x");
    expect(keyOf({ cluster: "a", name: "x" })).toBe("a/x");
    expect(keyOf({ name: "x" })).toBe("x");
    expect(devpodHref({ cluster: "a", name: "x" }, "yaml")).toBe("/devpods/a/x?tab=yaml");
    expect(devpodHref({ name: "x" })).toBe("/devpods/-/x");
  });
});
