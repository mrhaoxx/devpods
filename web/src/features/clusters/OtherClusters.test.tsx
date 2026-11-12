import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { renderWithProviders, stubFetch } from "../../test/render";
import { currentCluster, rememberCluster } from "../../data/cluster";
import { OtherClusters } from "./OtherClusters";

const dp = (cluster: string, name: string, phase = "Running") => ({ cluster, metadata: { name }, spec: { owner: "alice", running: phase === "Running" }, status: { phase } });
const two = [
  { name: "mulan", displayName: "木兰", connected: true },
  { name: "st2", connected: true },
];

describe("my DevPods in the other clusters", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    rememberCluster("");
  });

  it("is not there in a console of one cluster, and asks nothing", () => {
    const calls = stubFetch({});
    renderWithProviders(<OtherClusters go={() => {}} />);
    expect(screen.queryByRole("region")).toBeNull();
    expect(calls.length).toBe(0);
  });

  it("lists what I have elsewhere, asked of every cluster at once, and switches to it", async () => {
    rememberCluster("mulan");
    const calls = stubFetch({
      "GET /api/devpods": { items: [dp("mulan", "alice-here"), dp("st2", "alice-b", "Stopped"), dp("st2", "alice-a")], pods: {}, stale: { st2: "2026-11-12T08:00:00Z" } },
    });
    const go = vi.fn();
    renderWithProviders(<OtherClusters go={go} />, { clusters: two });
    const panel = within(await screen.findByRole("region", { name: "其它集群里的 DevPod" }));
    await panel.findByText("alice-a");
    // the plain path: every cluster, not the one in use
    expect(calls[0].url.pathname).toBe("/api/devpods");
    expect(panel.queryByText("alice-here")).toBeNull(); // the cluster in use has its own table
    expect(panel.getByText("alice-b")).toBeTruthy();
    expect(panel.getByText(/2 个/)).toBeTruthy();
    expect(panel.getByText(/最后状态/)).toBeTruthy(); // st2 is away
    fireEvent.click(panel.getByRole("button", { name: "切换到 st2" }));
    await waitFor(() => expect(go).toHaveBeenCalledTimes(1));
    expect(currentCluster()).toBe("st2");
  });

  it("is not there when I have nothing elsewhere", async () => {
    rememberCluster("mulan");
    const calls = stubFetch({ "GET /api/devpods": { items: [dp("mulan", "alice-here")], pods: {} } });
    renderWithProviders(<OtherClusters go={() => {}} />, { clusters: two });
    await waitFor(() => expect(calls.length).toBe(1));
    expect(screen.queryByRole("region")).toBeNull();
  });
});
