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
