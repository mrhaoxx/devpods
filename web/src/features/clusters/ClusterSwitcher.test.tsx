import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { renderWithProviders, stubFetch } from "../../test/render";
import { currentCluster, noteStale, rememberCluster } from "../../data/cluster";
import { ClusterGate, ClusterSwitcher, StaleBar } from "./ClusterSwitcher";

const two = {
  items: [
    { name: "mulan", displayName: "木兰", connected: true },
    { name: "st2", connected: false, lastSeen: "2026-11-12T08:00:00Z" },
  ],
};

describe("cluster switcher", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    rememberCluster("");
    noteStale(null);
  });

  it("is not there in a console of one cluster", async () => {
    const calls = stubFetch({ "GET /api/clusters": { items: [] } });
    renderWithProviders(
      <ClusterGate>
        <ClusterSwitcher go={() => {}} />
        <p>content</p>
      </ClusterGate>,
      { clusters: null },
    );
    expect(await screen.findByText("content")).toBeTruthy();
    expect(screen.queryByLabelText("集群")).toBeNull();
    expect(currentCluster()).toBe("");
    expect(calls.length).toBe(1);
  });

  it("is not there with a single cluster either, which is then the one in use", async () => {
    stubFetch({ "GET /api/clusters": { items: [two.items[0]] } });
    renderWithProviders(
      <ClusterGate>
        <ClusterSwitcher go={() => {}} />
        <p>content</p>
      </ClusterGate>,
      { clusters: null },
    );
    expect(await screen.findByText("content")).toBeTruthy();
    expect(screen.queryByLabelText("集群")).toBeNull();
    expect(currentCluster()).toBe("mulan");
  });

  it("holds the page back until a cluster is settled, then lists them", async () => {
    rememberCluster("gone");
    stubFetch({ "GET /api/clusters": two });
    renderWithProviders(
      <ClusterGate>
        <ClusterSwitcher go={() => {}} />
        <p>content</p>
      </ClusterGate>,
      { clusters: null },
    );
    expect(screen.queryByText("content")).toBeNull();
    const sel = (await screen.findByLabelText("集群")) as HTMLSelectElement;
    // the remembered cluster no longer exists: the connected one is used
    expect(currentCluster()).toBe("mulan");
    expect(sel.value).toBe("mulan");
    expect(Array.from(sel.options).map((o) => o.textContent)).toEqual(["木兰", "st2（离线）"]);
  });

  it("switching remembers the choice and reloads the console", async () => {
    stubFetch({ "GET /api/clusters": two });
    const go = vi.fn();
    renderWithProviders(
      <ClusterGate>
        <ClusterSwitcher go={go} />
      </ClusterGate>,
      { clusters: null },
    );
    fireEvent.change(await screen.findByLabelText("集群"), { target: { value: "st2" } });
    await waitFor(() => expect(go).toHaveBeenCalledTimes(1));
    expect(currentCluster()).toBe("st2");
  });

  it("says when what is shown is a cluster's last known state", async () => {
    rememberCluster("st2");
    renderWithProviders(<StaleBar />);
    expect(screen.queryByRole("status")).toBeNull();
    noteStale("2026-11-12T08:00:00Z");
    const bar = await screen.findByRole("status");
    expect(bar.textContent).toContain("st2");
    expect(bar.textContent).toContain("离线");
    expect(bar.textContent).toContain("无法修改");
  });
});
