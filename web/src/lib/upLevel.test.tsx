import { afterEach, describe, expect, it } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { parentOf, useUpLevel, useUpTarget } from "./upLevel";

describe("up a level (Cmd+↑)", () => {
  it("each page's parent: a DevPod's is my list, a config's the configs, an admin page's the overview; a top page has none", () => {
    expect(parentOf("/devpods/st/alice-a")).toBe("/");
    expect(parentOf("/configs/alice-sg")).toBe("/configs");
    expect(parentOf("/configs/new")).toBe("/configs");
    expect(parentOf("/sessions/abc")).toBe("/sessions");
    expect(parentOf("/admin/nodes")).toBe("/admin");
    expect(parentOf("/admin")).toBeNull();
    expect(parentOf("/")).toBeNull();
    expect(parentOf("/configs")).toBeNull();
  });

  function Where() {
    const l = useLocation();
    return <div data-testid="where">{l.pathname}</div>;
  }
  function Page({ up }: { up?: string }) {
    useUpLevel();
    useUpTarget(up ?? null);
    return (
      <>
        <input aria-label="field" />
        <Where />
      </>
    );
  }
  const at = (path: string, up?: string) =>
    render(
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="*" element={<Page up={up} />} />
        </Routes>
      </MemoryRouter>,
    );
  const isMac = /Mac|iPhone|iPad/.test(navigator.platform);
  const up = (target: Element | Document = document.body) => fireEvent.keyDown(target, { key: "ArrowUp", metaKey: isMac, altKey: !isMac });
  afterEach(() => document.body.replaceChildren());

  it("goes to the parent; not while typing in a field", () => {
    at("/configs/alice-sg");
    up(screen.getByLabelText("field"));
    expect(screen.getByTestId("where").textContent).toBe("/configs/alice-sg"); // the field's own (caret to the start)
    up();
    expect(screen.getByTestId("where").textContent).toBe("/configs");
    up();
    expect(screen.getByTestId("where").textContent).toBe("/configs"); // a top page: nowhere further
  });

  it("a page may say its parent (someone else's DevPod: everyone's list)", () => {
    at("/devpods/st/bob-x", "/admin/devpods");
    up();
    expect(screen.getByTestId("where").textContent).toBe("/admin/devpods");
  });
});
