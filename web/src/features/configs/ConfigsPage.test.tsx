import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { renderWithProviders, stubFetch, stubMe } from "../../test/render";
import type { ConfigSummary } from "./api";
import { ConfigsPage } from "./ConfigsPage";

const cfg = (o: Partial<ConfigSummary>): ConfigSummary => ({
  name: "alice-a",
  owner: "alice",
  title: "我的配置",
  description: "",
  sharedWith: [],
  public: false,
  official: false,
  variables: [
    { name: "NAME", builtin: true, required: true, type: "string" },
    { name: "IMAGE", builtin: false, required: false, default: "ubuntu:24.04", type: "string" },
  ],
  mine: true,
  updatedAt: new Date().toISOString(),
  ...o,
});

const mine = cfg({});
const shared = cfg({ name: "bob-s", owner: "bob", title: "共享的", mine: false });
const pub = cfg({ name: "carol-p", owner: "carol", title: "公开的", mine: false, public: true });
const minePublic = cfg({ name: "alice-pub", title: "我公开的", public: true });

const lists = (extra: Record<string, unknown> = {}) =>
  stubFetch({
    "GET /api/configs": (url: URL) => {
      const scope = url.searchParams.get("scope");
      return { items: scope === "mine" ? [mine, minePublic] : scope === "shared" ? [shared] : scope === "public" ? [pub, minePublic] : [] };
    },
    ...extra,
  });
const row = (title: string) => screen.getByText(title).closest("tr") as HTMLElement;

describe("配置 page", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("lists my configs, those shared with me and the public ones", async () => {
    lists();
    renderWithProviders(<ConfigsPage />);
    const my = within(await screen.findByRole("region", { name: "我的" }));
    await my.findByText("我的配置");
    expect(my.getByText("我公开的")).toBeTruthy();
    const sh = within(screen.getByRole("region", { name: "分享给我的" }));
    expect(await sh.findByText("共享的")).toBeTruthy();
    const pu = within(screen.getByRole("region", { name: "公开" }));
    expect(await pu.findByText("公开的")).toBeTruthy();
    // my own public config is listed with mine, once
    expect(pu.queryByText("我公开的")).toBeNull();
    expect(within(row("我的配置")).getByText("NAME")).toBeTruthy();
    expect(within(row("我公开的")).getByText("公开")).toBeTruthy();
    expect(within(row("共享的")).getByText("bob")).toBeTruthy();
  });

  it("someone else's config can be used and copied, not edited or deleted", async () => {
    lists();
    renderWithProviders(<ConfigsPage />);
    await screen.findByText("共享的");
    const r = within(row("共享的"));
    expect(r.getByRole("button", { name: "用此配置创建" })).toBeTruthy();
    expect(r.getByRole("button", { name: "另存为副本" })).toBeTruthy();
    expect(r.queryByRole("link", { name: "编辑" })).toBeNull();
    expect(r.queryByRole("button", { name: /删除/ })).toBeNull();
    expect(r.queryByRole("button", { name: "取消公开" })).toBeNull();
    const own = within(row("我的配置"));
    expect(own.getByRole("link", { name: "编辑" })).toBeTruthy();
    expect(own.getByRole("button", { name: /删除/ })).toBeTruthy();
  });

  it("the owner deletes after confirming", async () => {
    const calls = lists({ "DELETE /api/configs/alice-a": new Response(null, { status: 204 }) });
    renderWithProviders(<ConfigsPage />);
    await screen.findByText("我的配置");
    fireEvent.click(within(row("我的配置")).getByRole("button", { name: /删除/ }));
    fireEvent.click(await screen.findByRole("button", { name: "删除配置" }));
    await waitFor(() => expect(calls.some((c) => c.method === "DELETE" && c.url.pathname === "/api/configs/alice-a")).toBe(true));
  });

  it("an admin may take someone's public config down or delete it", async () => {
    const calls = lists({ "PATCH /api/configs/carol-p": { ...pub, public: false } });
    renderWithProviders(<ConfigsPage />, { me: stubMe({ admin: true }) });
    await screen.findByText("公开的");
    const r = within(row("公开的"));
    expect(r.getByRole("button", { name: /删除/ })).toBeTruthy();
    expect(r.queryByRole("link", { name: "编辑" })).toBeNull();
    fireEvent.click(r.getByRole("button", { name: "取消公开" }));
    await waitFor(() => expect(calls.find((c) => c.method === "PATCH")?.body).toBe('{"public":false}'));
  });

  it("用此配置创建 opens the launch dialog for that config", async () => {
    lists({ "/api/nodes": { items: [] } });
    renderWithProviders(<ConfigsPage />);
    await screen.findByText("共享的");
    fireEvent.click(within(row("共享的")).getByRole("button", { name: "用此配置创建" }));
    expect(await screen.findByRole("dialog", { name: /用配置创建.*共享的/ })).toBeTruthy();
  });

  it("says so when there is nothing", async () => {
    stubFetch({ "GET /api/configs": { items: [] } });
    renderWithProviders(<ConfigsPage />);
    expect(await screen.findByText("还没有保存过配置")).toBeTruthy();
  });
});
