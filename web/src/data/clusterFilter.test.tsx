import { afterEach, describe, expect, it } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { useLocation } from "react-router-dom";
import { useClusterFilter } from "./clusterFilter";
import { renderWithProviders, stubFetch } from "../test/render";

function Probe() {
  const f = useClusterFilter();
  const l = useLocation();
  return (
    <>
      <div data-testid="where">{l.search}</div>
      <div data-testid="filter">{f.filter || "all"}</div>
      <button onClick={() => f.setFilter("a")}>a</button>
    </>
  );
}
const two = [{ name: "a", connected: true }, { name: "b", connected: true }];

describe("cluster filter", () => {
  afterEach(() => localStorage.clear());

  it("the address keeps it; one without it gets the remembered one", async () => {
    stubFetch({});
    localStorage.setItem("devpod.clusterFilter", "b");
    renderWithProviders(<Probe />, { clusters: two, route: "/images" });
    await waitFor(() => expect(screen.getByTestId("where").textContent).toBe("?cluster=b"));
    fireEvent.click(screen.getByRole("button", { name: "a" }));
    expect(screen.getByTestId("where").textContent).toBe("?cluster=a");
    expect(localStorage.getItem("devpod.clusterFilter")).toBe("a");
  });

  it("a cluster that is gone is all of them", () => {
    stubFetch({});
    localStorage.setItem("devpod.clusterFilter", "c");
    renderWithProviders(<Probe />, { clusters: two, route: "/images?cluster=c" });
    expect(screen.getByTestId("filter").textContent).toBe("all");
    expect(localStorage.getItem("devpod.clusterFilter")).toBeNull();
  });
});
