import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { GrantsPanel } from "./GrantsPanel";
import { renderWithProviders, stubFetch, stubMe } from "../../test/render";

const grants = {
  items: [
    { name: "g1", match: "user = a", permissions: ["viewAudit"], resourceVersion: "1", problem: "" },
    { name: "g2", match: "group = gone", permissions: ["setReservable"], resourceVersion: "2", problem: "点名了不存在的组 gone" },
  ],
};
const preview = { clusters: [], machines: [], users: { count: 1, maybe: 0, total: 3, names: ["a"] }, hasMachine: false };
const panel = () => renderWithProviders(<GrantsPanel groups={[]} />, { me: stubMe({ admin: true }), route: "/admin/access" });

describe("GrantsPanel", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("lists the grants, with the broken one said", async () => {
    stubFetch({ "GET /api/admin/grants": grants });
    panel();
    const rows = await screen.findAllByTestId("grant");
    expect(rows).toHaveLength(2);
    expect(rows[0].textContent).toContain("user = a");
    expect(rows[0].textContent).toContain("看审计");
    expect(rows[1].textContent).toContain("切换可预约");
    const alert = screen.getByRole("alert");
    expect(alert.textContent).toContain("g2");
    expect(alert.textContent).toContain("点名了不存在的组 gone");
  });

  it("ticking disconnect ticks view; unticking view unticks disconnect", async () => {
    stubFetch({ "GET /api/admin/grants": grants, "POST /api/admin/access/preview": preview });
    panel();
    fireEvent.click(await screen.findByRole("button", { name: "新增授权" }));
    const drawer = within(await screen.findByRole("dialog"));
    const view = drawer.getByLabelText("看审计") as HTMLInputElement;
    const cut = drawer.getByLabelText("断开会话") as HTMLInputElement;
    fireEvent.click(cut);
    expect(view.checked).toBe(true);
    expect(cut.checked).toBe(true);
    fireEvent.click(view);
    expect(view.checked).toBe(false);
    expect(cut.checked).toBe(false);
  });

  it("saves one grant on its own", async () => {
    const calls = stubFetch({
      "GET /api/admin/grants": grants,
      "POST /api/admin/access/preview": preview,
      "POST /api/admin/grants": { name: "g3", match: "user = a", permissions: ["viewAudit"], resourceVersion: "3", problem: "" },
    });
    panel();
    fireEvent.click(await screen.findByRole("button", { name: "新增授权" }));
    const drawer = within(await screen.findByRole("dialog"));
    fireEvent.change(drawer.getByLabelText("名字"), { target: { value: "g3" } });
    fireEvent.change(drawer.getByLabelText("条件"), { target: { value: "user = a" } });
    fireEvent.click(drawer.getByLabelText("看审计"));
    fireEvent.click(drawer.getByRole("button", { name: "确定" }));
    await waitFor(() => expect(calls.some((c) => c.method === "POST" && c.url.pathname === "/api/admin/grants")).toBe(true));
    const sent = JSON.parse(calls.find((c) => c.method === "POST" && c.url.pathname === "/api/admin/grants")!.body!);
    expect(sent).toEqual({ name: "g3", match: "user = a", permissions: ["viewAudit"] });
    expect(calls.some((c) => c.method === "PUT" && c.url.pathname === "/api/admin/access")).toBe(false);
  });
});
