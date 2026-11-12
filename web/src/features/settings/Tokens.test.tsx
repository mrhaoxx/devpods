import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { Tokens } from "./Tokens";
import { renderWithProviders, stubFetch } from "../../test/render";

const scopes = [
  { id: "devpods:read", group: "DevPod", label: "查看 DevPod" },
  { id: "devpods:power", group: "DevPod", label: "启动、休眠" },
];

describe("Tokens", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("creates a token with the picked scopes and shows it once", async () => {
    const calls = stubFetch({
      "/api/me/tokens": { items: [], scopes },
      "/api/devpods": { items: [], pods: {} },
      "POST /api/me/tokens": { token: "dpt_abc_secret", info: {} },
    });
    renderWithProviders(<Tokens />);
    fireEvent.click(await screen.findByRole("button", { name: "新建令牌" }));
    const create = screen.getByRole("button", { name: "创建令牌" }) as HTMLButtonElement;
    expect(create.disabled).toBe(true); // needs a name
    fireEvent.change(screen.getByLabelText("名称"), { target: { value: "ci" } });
    fireEvent.click(screen.getByLabelText("devpods:power"));
    fireEvent.click(create);
    await waitFor(() => expect(calls.find((c) => c.method === "POST")?.body).toBe('{"name":"ci","scopes":["devpods:read","devpods:power"],"devpods":[],"expiresInDays":30}'));
    expect(await screen.findByText(/只显示这一次/)).toBeTruthy();
    expect(screen.getByText("dpt_abc_secret")).toBeTruthy();
  });

  it("with several clusters a token can be held to some of them", async () => {
    const calls = stubFetch({
      "/api/me/tokens": { items: [{ id: "t1", name: "old", scopes: ["devpods:read"], devpods: [], clusters: ["st2"], createdAt: "2026-11-01T00:00:00Z", expiresAt: null, expired: false }], scopes },
      "/api/devpods": { items: [], pods: {} },
      "POST /api/me/tokens": { token: "dpt_abc_secret", info: {} },
    });
    renderWithProviders(<Tokens />, {
      clusters: [
        { name: "mulan", displayName: "木兰", connected: true },
        { name: "st2", connected: true },
      ],
    });
    // an existing token says where it reaches
    expect(await screen.findByText("st2")).toBeTruthy();
    fireEvent.click(await screen.findByRole("button", { name: "新建令牌" }));
    fireEvent.change(screen.getByLabelText("名称"), { target: { value: "ci" } });
    const create = screen.getByRole("button", { name: "创建令牌" }) as HTMLButtonElement;
    fireEvent.click(screen.getByLabelText("只允许访问指定的集群"));
    expect(create.disabled).toBe(true); // none picked yet
    fireEvent.click(screen.getByLabelText("集群 mulan"));
    fireEvent.click(create);
    await waitFor(() =>
      expect(calls.find((c) => c.method === "POST")?.body).toBe('{"name":"ci","scopes":["devpods:read"],"devpods":[],"expiresInDays":30,"clusters":["mulan"]}'),
    );
  });

  it("lists tokens without secrets and revokes after a confirmation", async () => {
    const calls = stubFetch({
      "/api/me/tokens": { items: [{ id: "abc", name: "ci", scopes: ["devpods:read"], devpods: ["alice-a"], createdAt: "2026-11-01T00:00:00Z", expiresAt: null, expired: false }], scopes },
      "DELETE /api/me/tokens/abc": new Response(null, { status: 204 }),
    });
    renderWithProviders(<Tokens />);
    expect(await screen.findByText("dpt_abc_…")).toBeTruthy();
    expect(screen.getByText("alice-a")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "撤销 ci" }));
    expect(calls.some((c) => c.method === "DELETE")).toBe(false);
    fireEvent.click(screen.getByRole("button", { name: "撤销" }));
    await waitFor(() => expect(calls.some((c) => c.method === "DELETE" && c.url.pathname === "/api/me/tokens/abc")).toBe(true));
  });
});
