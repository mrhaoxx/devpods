import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { AccessPage } from "./AccessPage";
import { renderWithProviders, stubFetch, stubMe } from "../../test/render";
import type { AccessPolicy } from "./api";

const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

const policy: AccessPolicy = {
  rules: [
    { cluster: "yc", machines: ["c500-8-65-*"], subject: { kind: "group", name: "infer" }, effect: { allow: true, privileged: true } },
    { cluster: "yc", subject: { kind: "everyone" }, effect: { allow: true, needsReservation: true, hostPaths: [{ path: "/data" }] } },
  ],
  default: { allow: true, privileged: true, hostPaths: [{ path: "/", writable: true }] },
  resourceVersion: "7",
};
const page = () => renderWithProviders(<AccessPage />, { me: stubMe({ admin: true }), route: "/admin/access" });

describe("AccessPage", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("shows the rules in order and the default last", async () => {
    stubFetch({ "GET /api/admin/access": policy, "/api/admin/groups": { items: [] } });
    page();
    const rows = await screen.findAllByTestId("access-rule");
    expect(rows).toHaveLength(3);
    expect(rows[0].textContent).toContain("c500-8-65-*");
    expect(rows[0].textContent).toContain("组 infer");
    expect(rows[1].textContent).toContain("需预约");
    expect(rows[1].textContent).toContain("/data（只读）");
    expect(rows[2].textContent).toContain("默认");
  });

  it("moving a rule up and saving sends the whole list in its new order, with the version read", async () => {
    const calls = stubFetch({ "GET /api/admin/access": policy, "/api/admin/groups": { items: [] }, "PUT /api/admin/access": { ...policy, resourceVersion: "8" } });
    page();
    fireEvent.click(await screen.findByRole("button", { name: "上移 规则 2" }));
    fireEvent.click(screen.getByRole("button", { name: "保存" }));
    await waitFor(() => expect(calls.some((c) => c.method === "PUT")).toBe(true));
    const sent = JSON.parse(calls.find((c) => c.method === "PUT")!.body!);
    expect(sent.rules[0].subject).toEqual({ kind: "everyone" });
    expect(sent.rules[1].subject).toEqual({ kind: "group", name: "infer" });
    expect(sent.resourceVersion).toBe("7");
  });

  it("says so when someone else changed the rules meanwhile", async () => {
    stubFetch({ "GET /api/admin/access": policy, "/api/admin/groups": { items: [] }, "PUT /api/admin/access": () => json(409, { code: "CONFLICT", message: "changed" }) });
    page();
    fireEvent.click(await screen.findByRole("button", { name: "上移 规则 2" }));
    fireEvent.click(screen.getByRole("button", { name: "保存" }));
    expect(await screen.findByText(/规则已被别人改过/)).toBeTruthy();
  });

  it("checks the running DevPods and stops the ones picked", async () => {
    const calls = stubFetch({
      "GET /api/admin/access": policy,
      "/api/admin/groups": { items: [] },
      "POST /api/admin/access/check": {
        items: [
          { devpod: "bob-a", owner: "bob", node: "g1", code: "ACL_DENIED", reason: "bob may not use node g1", rule: 0 },
          { devpod: "carol-b", owner: "carol", node: "g2", code: "RESERVATION_REQUIRED", reason: "reserve it first", rule: 1 },
        ],
      },
      "PATCH /api/devpods/bob-a": { devpod: {} },
      "PATCH /api/devpods/carol-b": { devpod: {} },
    });
    page();
    fireEvent.click(await screen.findByRole("button", { name: "检查运行中的 DevPod" }));
    const table = await screen.findByRole("table", { name: "不合规的 DevPod" });
    expect(within(table).getByText("bob-a")).toBeTruthy();
    fireEvent.click(within(table).getByLabelText("全选"));
    fireEvent.click(screen.getByRole("button", { name: "停止所选" }));
    await waitFor(() => expect(calls.filter((c) => c.method === "PATCH")).toHaveLength(2));
    expect(calls.filter((c) => c.method === "PATCH").every((c) => c.body === '{"running":false}')).toBe(true);
  });

  it("warns that a writable host path is no boundary, and when some machines a reservation rule names are not reservable", async () => {
    stubFetch({
      "GET /api/admin/access": policy,
      "/api/admin/groups": { items: [] },
      "/api/admin/access/match": { items: [{ count: 3, names: ["g1", "g2", "g3"], unreservable: 1 }] },
    });
    page();
    fireEvent.click(await screen.findByRole("button", { name: "新增规则" }));
    const drawer = await screen.findByRole("dialog");
    fireEvent.change(within(drawer).getByLabelText("机器"), { target: { value: "g*" } });
    fireEvent.click(within(drawer).getByLabelText("允许"));
    fireEvent.click(within(drawer).getByLabelText("需预约"));
    expect(await within(drawer).findByText(/其中 1 台没标可预约/)).toBeTruthy();
    fireEvent.click(within(drawer).getByRole("button", { name: "添加主机路径" }));
    fireEvent.click(within(drawer).getByLabelText("可写"));
    expect(within(drawer).getByText(/可写路径不是安全边界/)).toBeTruthy();
  });
});
