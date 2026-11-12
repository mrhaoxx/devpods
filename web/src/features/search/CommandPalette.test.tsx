import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { useLocation } from "react-router-dom";
import { CommandPalette } from "./CommandPalette";
import { renderWithProviders, stubFetch, stubMe } from "../../test/render";

function Where() {
  const l = useLocation();
  return <div data-testid="where">{l.pathname + l.search}</div>;
}

const results = {
  users: [{ name: "alice", displayName: "Alice" }],
  devpods: [
    { name: "alice-train", owner: "alice", phase: "Running" },
    { name: "alice-trainer", owner: "alice", phase: "Stopped" },
  ],
  sessions: [{ id: "s1", user: "alice", devpod: "alice-train", openedAt: "2026-11-10T10:00:00Z" }],
  commands: [{ sessionId: "s1", ts: "2026-11-10T10:01:00Z", user: "alice", devpod: "alice-train", cmd: "python train.py" }],
};

function setup(res: unknown = results, admin = false) {
  stubFetch({ "/api/search": res });
  renderWithProviders(
    <>
      <CommandPalette />
      <Where />
    </>,
    { me: stubMe({ admin }) },
  );
}

const open = () => fireEvent.keyDown(window, { key: "k", ctrlKey: true });

describe("command palette", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("Ctrl+K opens the palette", () => {
    setup();
    expect(screen.queryByPlaceholderText(/搜索/)).toBeNull();
    open();
    expect(screen.getByPlaceholderText(/搜索/)).toBeTruthy();
  });

  it("results are grouped with headings", async () => {
    setup(results, true);
    open();
    fireEvent.change(screen.getByPlaceholderText(/搜索/), { target: { value: "train" } });
    await screen.findByText("python train.py");
    for (const h of ["用户", "DevPod", "会话", "命令"]) expect(screen.getByText(h)).toBeTruthy();
  });

  it("arrow keys and Enter navigate to the DevPod", async () => {
    setup({ ...results, users: [] });
    open();
    const input = screen.getByPlaceholderText(/搜索/);
    fireEvent.change(input, { target: { value: "train" } });
    await screen.findByText("alice-trainer");
    fireEvent.keyDown(input, { key: "ArrowDown" });
    fireEvent.keyDown(input, { key: "ArrowUp" });
    fireEvent.keyDown(input, { key: "Enter" });
    await waitFor(() => expect(screen.getByTestId("where").textContent).toBe("/devpods/-/alice-train"));
  });

  it("DevPod action 打开监控 goes to ?tab=monitor", async () => {
    setup();
    open();
    fireEvent.change(screen.getByPlaceholderText(/搜索/), { target: { value: "train" } });
    fireEvent.click(await screen.findByRole("button", { name: "alice-train 打开监控" }));
    expect(screen.getByTestId("where").textContent).toBe("/devpods/-/alice-train?tab=monitor");
  });

  it("auditError shows a note instead of the 会话/命令 groups", async () => {
    setup({ ...results, sessions: [], commands: [], auditError: "audit search unavailable: down" });
    open();
    fireEvent.change(screen.getByPlaceholderText(/搜索/), { target: { value: "train" } });
    expect(await screen.findByText(/审计搜索暂不可用/)).toBeTruthy();
    expect(screen.queryByText("会话")).toBeNull();
  });
});
