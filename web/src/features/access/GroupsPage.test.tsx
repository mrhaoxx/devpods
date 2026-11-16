import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { GroupsPage } from "./GroupsPage";
import { renderWithProviders, stubFetch, stubMe } from "../../test/render";

describe("GroupsPage", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("lists the groups and creates one with its members", async () => {
    const calls = stubFetch({
      "GET /api/admin/groups": { items: [{ name: "infer", displayName: "推理", members: ["wyc", "qiulei"] }] },
      "POST /api/admin/groups": { name: "ops", members: ["star"] },
    });
    renderWithProviders(<GroupsPage />, { me: stubMe({ admin: true }), route: "/admin/groups" });
    expect(await screen.findByText("推理")).toBeTruthy();
    expect(screen.getByText(/wyc, qiulei/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "新建组" }));
    const dlg = await screen.findByRole("dialog");
    fireEvent.change(within(dlg).getByLabelText("组名"), { target: { value: "ops" } });
    fireEvent.change(within(dlg).getByLabelText("成员"), { target: { value: "star, mrhaoxx" } });
    fireEvent.click(within(dlg).getByRole("button", { name: "保存" }));
    await waitFor(() => expect(calls.some((c) => c.method === "POST")).toBe(true));
    expect(JSON.parse(calls.find((c) => c.method === "POST")!.body!)).toEqual({ name: "ops", displayName: "", members: ["star", "mrhaoxx"] });
  });
});
