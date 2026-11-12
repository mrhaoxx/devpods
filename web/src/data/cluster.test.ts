import { afterEach, describe, expect, it } from "vitest";
import { apiPath, currentCluster, noteStale, pickCluster, rememberCluster, staleSince } from "./cluster";

describe("cluster scope of API paths", () => {
  afterEach(() => {
    rememberCluster("");
    noteStale(null);
  });

  it("leaves every path alone while no cluster is chosen", () => {
    expect(apiPath("/api/devpods")).toBe("/api/devpods");
    expect(apiPath("/api/nodes?x=1")).toBe("/api/nodes?x=1");
  });

  it("puts what is a cluster's under the chosen cluster", () => {
    rememberCluster("mulan");
    expect(currentCluster()).toBe("mulan");
    for (const p of [
      "/api/devpods",
      "/api/devpods?watch=true",
      "/api/devpods/alice-x/pod",
      "/api/nodes",
      "/api/admin/nodes/n1",
      "/api/admin/devpods",
      "/api/reservations?from=a&to=b",
      "/api/snapshots/s1/progress",
      "/api/templates",
      "/api/kore/topology",
      "/api/live/devpods",
      "/api/admin/live/overview",
      "/api/configs/alice-sg/launch",
    ]) {
      expect(apiPath(p)).toBe("/api/clusters/mulan" + p.slice(4));
    }
  });

  it("leaves what is the console's where it is", () => {
    rememberCluster("mulan");
    for (const p of [
      "/api/me",
      "/api/me/registries/x",
      "/api/auth/config",
      "/api/clusters",
      "/api/admin/clusters/mulan",
      "/api/admin/users",
      "/api/configs",
      "/api/configs/alice-sg",
      "/api/audit/sessions",
      "/api/search?q=x",
      "/auth/logout",
    ]) {
      expect(apiPath(p)).toBe(p);
    }
  });

  it("does not prefix twice", () => {
    rememberCluster("mulan");
    expect(apiPath("/api/clusters/st2/devpods")).toBe("/api/clusters/st2/devpods");
  });

  it("picks a cluster: the remembered one if it still exists, else a connected one", () => {
    const list = [
      { name: "a", connected: false },
      { name: "b", connected: true },
    ];
    expect(pickCluster([], "old")).toBe(""); // a console of one cluster: nothing to choose
    expect(pickCluster(list, "a")).toBe("a");
    expect(pickCluster(list, "gone")).toBe("b");
    expect(pickCluster(list, "")).toBe("b");
    expect(pickCluster([{ name: "a", connected: false }], "")).toBe("a");
  });

  it("remembers since when a cluster's answers are stale", () => {
    expect(staleSince()).toBeNull();
    noteStale("2026-11-12T08:00:00Z");
    expect(staleSince()).toBe("2026-11-12T08:00:00Z");
    noteStale(null);
    expect(staleSince()).toBeNull();
  });
});
