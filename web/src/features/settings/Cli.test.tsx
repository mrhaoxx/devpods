import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { Cli } from "./Cli";
import { stubMe } from "../../test/render";

describe("the SSH CLI on the settings page", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("says how to reach it through the gateway, each line copyable", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
    render(<Cli me={stubMe({ user: "alice", cli: "devpod", ssh: { host: "gw.example", port: 2222 } })} />);
    expect(screen.getByText("ssh -p 2222 alice+devpod@gw.example")).toBeTruthy();
    expect(screen.getByText("ssh -p 2222 alice+devpod@gw.example list")).toBeTruthy();
    expect(screen.getByText(/Host devpod\s+HostName gw\.example\s+Port 2222\s+User alice\+devpod/)).toBeTruthy();
    fireEvent.click(screen.getAllByRole("button", { name: "复制" })[1]);
    await waitFor(() => expect(writeText).toHaveBeenCalledWith("ssh -p 2222 alice+devpod@gw.example list"));
  });

  it("logs in the way a DevPod does: no -p on 22, the route's suffix after the name", () => {
    render(<Cli me={stubMe({ user: "alice", cli: "devpod", ssh: { host: "gw.example", port: 22, loginSuffix: "hpc" } })} />);
    expect(screen.getByText("ssh alice+devpod+hpc@gw.example")).toBeTruthy();
  });

  it("is not there when the deployment has no CLI", () => {
    const { container } = render(<Cli me={stubMe({ user: "alice", ssh: { host: "gw.example", port: 22 } })} />);
    expect(container.innerHTML).toBe("");
  });
});
