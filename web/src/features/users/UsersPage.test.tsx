import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { UsersPage } from "./UsersPage";
import { renderWithProviders, stubFetch, stubMe } from "../../test/render";

const user = (name: string, cpu: string, extra: Record<string, unknown> = {}) => ({
  name,
  admin: false,
  hasPassword: true,
  devpods: 1,
  running: 1,
  usage: { cpu },
  source: "password",
  customPolicy: false,
  pubkeys: 1,
  lastSession: "2026-11-09T00:00:00Z",
  ...extra,
});

const users = {
  items: [user("alice", "7"), user("bob", "1", { lastSession: undefined })],
  defaultQuota: { maxDevPods: 5, compute: { cpu: "8", memory: "16Gi" } },
};

const names = () =>
  screen
    .getAllByRole("row")
    .slice(1)
    .map((r) => within(r).queryByTestId("user-name")?.textContent)
    .filter(Boolean);

function setup(route = "/admin/users", extra: Record<string, unknown> = {}) {
  const calls = stubFetch({
    "/api/admin/users": users,
    "/api/devpods": { items: [], pods: {} },
    "/api/audit/sessions": { items: [], next: "" },
    "/api/admin/users/alice/audit-policy": { default: {}, user: null, effective: {}, warnings: [] },
    ...extra,
  });
  renderWithProviders(<UsersPage />, { me: stubMe({ admin: true, user: "root" }), route });
  return calls;
}

describe("users page", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("配额 >80% filter uses meterLevel", async () => {
    setup("/admin/users?high=1");
    await screen.findByText("alice");
    expect(names()).toEqual(["alice"]);
  });

  it("batch reset shows passwords once and offers CSV", async () => {
    setup("/admin/users", {
      "POST /api/admin/users:batch": { results: [{ name: "alice", ok: true, password: "Xy12Ab34Cd56Ef78" }] },
    });
    await screen.findByText("alice");
    fireEvent.click(screen.getByLabelText("选择 alice"));
    fireEvent.click(screen.getByRole("button", { name: "重置密码" }));
    fireEvent.click(screen.getByRole("button", { name: "确认重置" }));
    expect(await screen.findByText("Xy12Ab34Cd56Ef78")).toBeTruthy();
    expect(screen.getByRole("button", { name: "下载 CSV" })).toBeTruthy();
    expect(screen.getByText(/只显示这一次/)).toBeTruthy();
  });

  it("import preview lists per-row errors and disables confirm when nothing is valid", async () => {
    setup("/admin/users", {
      "POST /api/admin/users:import": { rows: [{ line: 1, username: "Bad_Name", ok: false, error: "not a valid DNS-1123 label" }], created: 0 },
    });
    await screen.findByText("alice");
    fireEvent.click(screen.getByRole("button", { name: "导入 CSV" }));
    fireEvent.change(screen.getByLabelText("CSV 内容"), { target: { value: "Bad_Name,X" } });
    fireEvent.click(screen.getByRole("button", { name: "预览" }));
    expect(await screen.findByText(/not a valid DNS-1123 label/)).toBeTruthy();
    expect((screen.getByRole("button", { name: "确认导入" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("drawer opens from ?user=alice", async () => {
    setup("/admin/users?user=alice");
    const dlg = await screen.findByRole("dialog", { name: /alice/ });
    expect(within(dlg).getByText("配额")).toBeTruthy();
  });

  it("with several clusters a quota can be set for one of them", async () => {
    const calls = stubFetch({
      "/api/admin/users": {
        ...users,
        items: [user("alice", "7", { quotas: { st2: { maxDevPods: 2 } } }), user("bob", "1")],
      },
      "/api/devpods": { items: [], pods: {} },
      "/api/audit/sessions": { items: [], next: "" },
      "/api/admin/users/alice/audit-policy": { default: {}, user: null, effective: {}, warnings: [] },
      "PATCH /api/admin/users/alice": new Response(null, { status: 204 }),
    });
    renderWithProviders(<UsersPage />, {
      me: stubMe({ admin: true, user: "root" }),
      route: "/admin/users?user=alice",
      clusters: [
        { name: "mulan", connected: true },
        { name: "st2", connected: true },
      ],
    });
    const dlg = within(await screen.findByRole("dialog", { name: /alice/ }));
    const where = (await dlg.findByLabelText("配额作用于")) as HTMLSelectElement;
    expect(Array.from(where.options).map((o) => o.textContent)).toEqual(["所有集群", "mulan", "st2"]);
    // the cluster's own entry is what the fields show
    fireEvent.change(where, { target: { value: "st2" } });
    expect((dlg.getByLabelText("配额 DevPod 数") as HTMLInputElement).value).toBe("2");
    fireEvent.change(where, { target: { value: "mulan" } });
    expect((dlg.getByLabelText("配额 DevPod 数") as HTMLInputElement).value).toBe("");
    fireEvent.change(dlg.getByLabelText("配额 CPU"), { target: { value: "64" } });
    fireEvent.click(dlg.getByRole("button", { name: "保存配额" }));
    await waitFor(() => expect(calls.find((c) => c.method === "PATCH")?.body).toBe('{"quotas":{"mulan":{"maxDevPods":null,"cpu":"64"}}}'));
  });

  it("without clusters there is no choice of where a quota applies", async () => {
    setup("/admin/users?user=alice");
    const dlg = within(await screen.findByRole("dialog", { name: /alice/ }));
    await dlg.findByText("配额");
    expect(dlg.queryByLabelText("配额作用于")).toBeNull();
  });

  it("delete user requires confirmation", async () => {
    const calls = setup("/admin/users?user=alice", { "DELETE /api/admin/users/alice": new Response(null, { status: 204 }) });
    const dlg = await screen.findByRole("dialog", { name: /alice/ });
    fireEvent.click(within(dlg).getByRole("button", { name: "删除用户…" }));
    const confirm = screen.getByRole("button", { name: "确认删除" }) as HTMLButtonElement;
    expect(confirm.disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("输入 alice 以确认"), { target: { value: "alice" } });
    fireEvent.click(confirm);
    await waitFor(() => expect(calls.some((c) => c.method === "DELETE" && c.url.pathname === "/api/admin/users/alice")).toBe(true));
  });
});
