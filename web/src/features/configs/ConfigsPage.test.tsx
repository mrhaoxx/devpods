import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { renderWithProviders, stubFetch, stubMe } from "../../test/render";
import type { ConfigSummary } from "./api";
import { ConfigsPage } from "./ConfigsPage";
import { useLocation } from "react-router-dom";

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
const shared = cfg({ name: "bob-s", owner: "bob", title: "共享的", mine: false, variables: [{ name: "GPUS", builtin: false, required: true, type: "int" }] });
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
const rowTitles = () =>
  screen
    .getAllByRole("row")
    .slice(1)
    .map((r) => r.querySelector("[data-testid=config-title]")?.textContent)
    .filter(Boolean);
const menu = async (title: string) => {
  fireEvent.keyDown(within(row(title)).getByRole("button", { name: `${title} 更多操作` }), { key: "Enter" });
  return within(await screen.findByRole("menu"));
};

function Where() {
  const l = useLocation();
  return <div data-testid="where">{l.pathname + l.search}</div>;
}

describe("配置 page", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("one table: every config once, official first; scopes with counts narrow it; a search finds by variable", async () => {
    lists();
    renderWithProviders(<ConfigsPage />);
    await screen.findByText("共享的");
    expect(rowTitles()[0]).toBe("官方的");
    expect(rowTitles()).toHaveLength(5);
    expect(within(row("共享的")).getByText("bob")).toBeTruthy();
    expect(within(row("我的配置")).getByText("IMAGE")).toBeTruthy();
    expect(within(row("我公开的")).getByText("公开")).toBeTruthy();
    fireEvent.click(screen.getByRole("tab", { name: /我的\s*2/ }));
    await waitFor(() => expect(rowTitles().sort()).toEqual(["我公开的", "我的配置"]));
    fireEvent.click(screen.getByRole("tab", { name: /全部\s*5/ }));
    fireEvent.change(screen.getByLabelText("搜索配置"), { target: { value: "GPUS" } });
    await waitFor(() => expect(rowTitles()).toEqual(["共享的"]));
  });

  it("sorts by title", async () => {
    lists();
    renderWithProviders(<ConfigsPage />, { route: "/configs?sort=title" });
    await screen.findByText("共享的");
    const t = rowTitles() as string[];
    expect(t).toEqual([...t].sort((a, b) => a.localeCompare(b)));
  });

  it("someone else's config is used, viewed or copied; mine is edited or deleted after confirming", async () => {
    const calls = lists({ "DELETE /api/configs/alice-a": new Response(null, { status: 204 }) });
    renderWithProviders(
      <>
        <ConfigsPage />
        <Where />
      </>,
    );
    await screen.findByText("共享的");
    expect(within(row("共享的")).getByRole("link", { name: "用此配置创建" }).getAttribute("href")).toBe("/?new=1&config=bob-s");
    const theirs = await menu("共享的");
    expect(theirs.getByRole("menuitem", { name: "查看" })).toBeTruthy();
    expect(theirs.getByRole("menuitem", { name: "另存为副本" })).toBeTruthy();
    expect(theirs.queryByRole("menuitem", { name: "删除" })).toBeNull();
    fireEvent.keyDown(screen.getByRole("menu"), { key: "Escape" });
    const own = await menu("我的配置");
    expect(own.getByRole("menuitem", { name: "编辑" })).toBeTruthy();
    fireEvent.click(own.getByRole("menuitem", { name: "删除" }));
    fireEvent.click(await screen.findByRole("button", { name: "删除配置" }));
    await waitFor(() => expect(calls.some((c) => c.method === "DELETE" && c.url.pathname === "/api/configs/alice-a")).toBe(true));
    fireEvent.click(within(row("公开的")).getByText("carol"));
    await waitFor(() => expect(screen.getByTestId("where").textContent).toBe("/configs/carol-p"));
  });

  it("an admin marks a public config official or takes it down; nobody else may", async () => {
    const calls = lists({ "PATCH /api/configs/carol-p": cfg({ ...pub, official: true }) });
    const first = renderWithProviders(<ConfigsPage />, { me: stubMe({ admin: true, user: "root2" }) });
    await screen.findByText("公开的");
    fireEvent.click((await menu("公开的")).getByRole("menuitem", { name: "设为官方" }));
    await waitFor(() => expect(calls.find((c) => c.method === "PATCH")?.body).toBe('{"official":true}'));
    fireEvent.click((await menu("公开的")).getByRole("menuitem", { name: "取消公开" }));
    await waitFor(() => expect(calls.filter((c) => c.method === "PATCH").at(-1)?.body).toBe('{"public":false}'));
    first.unmount();
    lists();
    renderWithProviders(<ConfigsPage />);
    await screen.findByText("公开的");
    expect((await menu("公开的")).queryByRole("menuitem", { name: "设为官方" })).toBeNull();
  });

  it("says so when there is nothing", async () => {
    stubFetch({ "GET /api/configs": { items: [] } });
    renderWithProviders(<ConfigsPage />);
    expect(await screen.findByText("还没有保存过配置")).toBeTruthy();
  });
});
