import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { PortsPanel, portRows } from "./PortsPanel";
import { renderWithProviders, stubFetch } from "../../../test/render";

describe("PortsPanel", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("portRows lists TCP listeners without sshd, flags loopback-only ones, keeps public ports", () => {
    expect(
      portRows(
        [
          { port: 22, proto: "tcp", addr: "0.0.0.0" },
          { port: 8888, proto: "tcp", addr: "127.0.0.1" },
          { port: 8080, proto: "tcp6", addr: "::" },
          { port: 8080, proto: "tcp", addr: "127.0.0.1" },
          { port: 53, proto: "udp", addr: "0.0.0.0" },
        ],
        [9000],
      ),
    ).toEqual([
      { port: 8080, local: false, listening: true },
      { port: 8888, local: true, listening: true },
      { port: 9000, local: false, listening: false },
    ]);
  });

  it("links use the random id, and 公开 saves the port", async () => {
    const view = { id: "abcdefgh2345", urlTemplate: "https://abcdefgh2345-{port}-devpod.apps.test", publicPorts: [] as number[] };
    const calls = stubFetch({
      "/api/devpods/alice-a/http": view,
      "/api/devpods/alice-a/info": { info: { ts: "", cpu_limit: 1, mem_limit: 1 }, infoTs: "", snapshot: { ts: "", procs: 1, threads: 1, zombies: 0, listen: [{ port: 8080, proto: "tcp", addr: "0.0.0.0" }] } },
      "PUT /api/devpods/alice-a/http": { ...view, publicPorts: [8080] },
    });
    renderWithProviders(<PortsPanel name="alice-a" />);
    const open = await screen.findByRole("link", { name: /打开/ });
    expect(open.getAttribute("href")).toBe("https://abcdefgh2345-8080-devpod.apps.test");
    expect(screen.getByText("仅本人和协作者")).toBeTruthy();
    fireEvent.click(screen.getByLabelText("公开端口 8080"));
    await waitFor(() => expect(calls.some((c) => c.method === "PUT" && c.body === '{"publicPorts":[8080]}')).toBe(true));
    expect(await screen.findByText("公开")).toBeTruthy();
  });
});
