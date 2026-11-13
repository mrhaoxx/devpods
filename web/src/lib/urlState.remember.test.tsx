import { describe, expect, it } from "vitest";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { useUrlState, type Schema } from "./urlState";

const schema = {
  q: { kind: "string", default: "" },
  phase: { kind: "list", default: [] as string[] },
  open: { kind: "string", default: "" }, // a drawer: not remembered
} satisfies Schema;

function Page() {
  const [v, set] = useUrlState(schema, { key: "test", omit: ["open"] });
  return (
    <div>
      <span data-testid="q">{v.q}</span>
      <span data-testid="phase">{v.phase.join(",")}</span>
      <span data-testid="search">{useLocation().search}</span>
      <button onClick={() => set({ q: "gpu", phase: ["Running"], open: "dev1" })}>pick</button>
      <button onClick={() => set({ q: "", phase: [] })}>clear</button>
    </div>
  );
}
const at = (url: string) =>
  render(
    <MemoryRouter initialEntries={[url]} future={{ v7_startTransition: true, v7_relativeSplatPath: true }}>
      <Routes>
        <Route path="/" element={<Page />} />
      </Routes>
    </MemoryRouter>,
  );
const text = (id: string) => screen.getByTestId(id).textContent;

describe("a list page's remembered choices", () => {
  it("come back to an address without them, and the address then says them", async () => {
    const first = at("/");
    act(() => fireEvent.click(screen.getByText("pick")));
    first.unmount();

    at("/");
    expect(text("q")).toBe("gpu"); // at once, not after a redraw
    expect(text("phase")).toBe("Running");
    await waitFor(() => expect(text("search")).toBe("?q=gpu&phase=Running")); // not the drawer
  });

  it("give way to a link that says any of them, and are forgotten once cleared", () => {
    const first = at("/");
    act(() => fireEvent.click(screen.getByText("pick")));
    first.unmount();

    const link = at("/?phase=Failed");
    expect(text("q")).toBe("");
    expect(text("phase")).toBe("Failed");
    act(() => fireEvent.click(screen.getByText("clear")));
    link.unmount();

    at("/");
    expect(text("q")).toBe("");
    expect(text("search")).toBe("");
  });
});
