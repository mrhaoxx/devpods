import { describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { useLocation } from "react-router-dom";
import { ClusterFilter, OfflineBar } from "./ClusterFilter";
import { renderWithProviders, stubFetch } from "../../test/render";

function Where() {
  return <div data-testid="where">{useLocation().search}</div>;
}

describe("cluster filter in the top bar", () => {
  it("filters by one cluster or all; an offline one is named", async () => {
    localStorage.clear();
    stubFetch({});
    const two = [
      { name: "a", connected: true },
      { name: "b", displayName: "B 区", connected: false, lastSeen: "2026-11-13T08:00:00Z" },
    ];
    renderWithProviders(
      <>
        <ClusterFilter />
        <OfflineBar />
        <Where />
      </>,
      { clusters: two },
    );
    fireEvent.change(await screen.findByLabelText("集群筛选"), { target: { value: "b" } });
    await waitFor(() => expect(screen.getByTestId("where").textContent).toBe("?cluster=b"));
    expect(screen.getByRole("status").textContent).toContain("B 区");
  });

  it("is not there with one cluster", () => {
    stubFetch({});
    renderWithProviders(<ClusterFilter />, { clusters: [{ name: "a", connected: true }] });
    expect(screen.queryByLabelText("集群筛选")).toBeNull();
  });
});
