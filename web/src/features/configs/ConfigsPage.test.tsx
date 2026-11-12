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
const official = cfg({ name: "root-o", owner: "root", title: "官方的", mine: false, public: true, official: true });

const lists = (extra: Record<string, unknown> = {}) =>
  stubFetch({
    "GET /api/configs": (url: URL) => {
      const scope = url.searchParams.get("scope");
      return {
        items:
          scope === "mine" ? [mine, minePublic] : scope === "shared" ? [shared] : scope === "public" ? [official, pub, minePublic] : scope === "official" ? [official] : [],
      };
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
    expect(r.getByRole("link", { name: "用此配置创建" })).toBeTruthy();
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

  it("用此配置创建 opens the new-DevPod drawer on that config", async () => {
    lists();
    renderWithProviders(<ConfigsPage />);
    await screen.findByText("共享的");
    const link = within(row("共享的")).getByRole("link", { name: "用此配置创建" });
    expect(link.getAttribute("href")).toMatch(/^\/\?new=1&config=[a-z0-9-]+$/);
  });

  it("says so when there is nothing", async () => {
    stubFetch({ "GET /api/configs": { items: [] } });
    renderWithProviders(<ConfigsPage />);
    expect(await screen.findByText("还没有保存过配置")).toBeTruthy();
  });

  it("official configs come first, listed once", async () => {
    lists();
    renderWithProviders(<ConfigsPage />);
    const off = within(await screen.findByRole("region", { name: "官方" }));
    expect(await off.findByText("官方的")).toBeTruthy();
    expect(within(row("官方的")).getByText("官方")).toBeTruthy(); // the badge
    expect(within(screen.getByRole("region", { name: "公开" })).queryByText("官方的")).toBeNull();
    const regions = screen.getAllByRole("region").map((r) => r.getAttribute("aria-label"));
    expect(regions[0]).toBe("官方");
  });

  it("an admin marks a public config official, or not; nobody else may", async () => {
    const calls = lists({ "PATCH /api/configs/carol-p": cfg({ ...pub, official: true }), "PATCH /api/configs/root-o": cfg({ ...official, official: false }) });
    renderWithProviders(<ConfigsPage />, { me: stubMe({ admin: true, user: "root2" }) });
    await screen.findByText("公开的");
    fireEvent.click(within(row("公开的")).getByRole("button", { name: "设为官方" }));
    await waitFor(() => expect(calls.find((c) => c.method === "PATCH" && c.url.pathname.endsWith("carol-p"))?.body).toBe('{"official":true}'));
    fireEvent.click(within(row("官方的")).getByRole("button", { name: "取消官方" }));
    await waitFor(() => expect(calls.find((c) => c.method === "PATCH" && c.url.pathname.endsWith("root-o"))?.body).toBe('{"official":false}'));
  });

  it("not an admin: no official switch", async () => {
    lists();
    renderWithProviders(<ConfigsPage />);
    await screen.findByText("公开的");
    expect(screen.queryByRole("button", { name: "设为官方" })).toBeNull();
    expect(screen.queryByRole("button", { name: "取消官方" })).toBeNull();
  });
});
