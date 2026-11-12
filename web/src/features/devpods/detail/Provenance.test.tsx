import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { Provenance } from "./Provenance";
import { renderWithProviders, stubFetch } from "../../../test/render";
import type { DevPod } from "../../../data/api";

const member = (i: number): DevPod => ({
  metadata: {
    name: `alice-t-${i}`,
    labels: { "devpod.io/config": "root-train", "devpod.io/group": "alice-t" },
    annotations: { "devpod.io/group-index": String(i), "devpod.io/group-size": "2", "devpod.io/group-leader": "alice-t-0" },
  },
  spec: { owner: "alice", running: true },
});
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const base = {
  "GET /api/configs/root-train": { name: "root-train", title: "GPU 训练", owner: "root" },
  "GET /api/devpods/alice-t-1/yaml": { yaml: "image: ubuntu:24.04\n", running: true },
  "/api/devpods": { items: [member(0), member(1)], pods: {} },
};
const rebuilds = (calls: { method: string; url: URL; body?: string }[]) => calls.filter((c) => c.method === "POST" && c.url.pathname.endsWith("/rebuild-from-config"));

describe("Provenance", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("says where the DevPod came from, with links", async () => {
    stubFetch(base);
    renderWithProviders(<Provenance dp={member(1)} />);
    const config = await screen.findByRole("link", { name: "GPU 训练" });
    expect(config.getAttribute("href")).toBe("/configs/root-train");
    expect(screen.getByRole("link", { name: "alice-t" }).getAttribute("href")).toBe("/?group=alice-t");
    expect(screen.getByText(/第 1\/2 个/)).toBeTruthy();
    expect(screen.queryByText("配置已更新")).toBeNull();
  });

  it("a DevPod made by hand says nothing", () => {
    stubFetch({});
    const { container } = renderWithProviders(<Provenance dp={{ metadata: { name: "alice-x" }, spec: { owner: "alice", running: true } }} />);
    expect(container.textContent).toBe("");
  });

  it("outdated: a diff first, then a rebuild of this one or of the whole group", async () => {
    const calls = stubFetch({
      ...base,
      "POST /api/devpods/alice-t-1/rebuild-from-config": (_u: URL, init?: RequestInit) =>
        JSON.parse(String(init?.body ?? "{}")).dryRun ? { yaml: "image: debian:12\n", rebuild: true } : { devpod: member(1), rebuild: true },
      "POST /api/devpods/alice-t-0/rebuild-from-config": { devpod: member(0), rebuild: true },
    });
    renderWithProviders(<Provenance dp={member(1)} outdated />);
    expect(await screen.findByText("配置已更新")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "按新配置重建" }));
    // the dry run's YAML beside the present one
    expect(await screen.findByText(/debian:12/)).toBeTruthy();
    expect(screen.getByText(/ubuntu:24.04/)).toBeTruthy();
    expect(screen.getByText(/会话会断开/)).toBeTruthy();
    fireEvent.click(screen.getByLabelText("整组重建（2 个）"));
    fireEvent.click(screen.getByRole("button", { name: "重建" }));
    await waitFor(() => expect(rebuilds(calls).filter((c) => !JSON.parse(c.body ?? "{}").dryRun).map((c) => c.url.pathname).sort()).toEqual([
      "/api/devpods/alice-t-0/rebuild-from-config",
      "/api/devpods/alice-t-1/rebuild-from-config",
    ]));
  });

  it("a config whose variables changed says so", async () => {
    stubFetch({
      ...base,
      "POST /api/devpods/alice-t-1/rebuild-from-config": json(409, { code: "CONFIG_VARS", message: "the config's variables changed: MODE" }),
    });
    renderWithProviders(<Provenance dp={member(1)} outdated />);
    fireEvent.click(await screen.findByRole("button", { name: "按新配置重建" }));
    expect((await screen.findByText(/the config's variables changed: MODE/)).textContent).toBeTruthy();
  });
});
