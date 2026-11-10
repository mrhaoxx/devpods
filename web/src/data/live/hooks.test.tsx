import { describe, expect, it, vi } from "vitest";
import { act, render, screen } from "@testing-library/react";
import { LiveHub } from "./LiveHub";
import { LiveHubProvider, useLiveRow } from "./hooks";
import { devpodsStream } from "./streams";
import type { SseHandlers } from "../sse";

function Cell({ name }: { name: string }) {
  const { row, status } = useLiveRow<{ cpu: number }>(devpodsStream("mine"), name);
  return <span data-testid={name}>{row ? row.value.cpu : "—"}:{status}</span>;
}

describe("live hooks", () => {
  it("does not open a stream when disabled", () => {
    const open = vi.fn(() => () => {});
    render(
      <LiveHubProvider hub={new LiveHub({ open })} enabled={false}>
        <Cell name="a" />
      </LiveHubProvider>,
    );
    expect(open).not.toHaveBeenCalled();
    expect(screen.getByTestId("a").textContent).toBe("—:off");
  });

  it("renders the row and follows updates", () => {
    let h: SseHandlers | undefined;
    const hub = new LiveHub({
      open: (_u, handlers) => {
        h = handlers;
        return () => {};
      },
    });
    render(
      <LiveHubProvider hub={hub} enabled>
        <Cell name="a" />
      </LiveHubProvider>,
    );
    act(() => h!.onMessage({ ts: "", items: [{ name: "a", owner: "x", cpu: 1.5 }] }));
    expect(screen.getByTestId("a").textContent).toBe("1.5:live");
  });
});
