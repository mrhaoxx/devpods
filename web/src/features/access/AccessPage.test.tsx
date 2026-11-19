import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { AccessPage } from "./AccessPage";
import { renderWithProviders, stubFetch, stubMe } from "../../test/render";
import type { AccessPolicy } from "./api";

const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

const policy: AccessPolicy = {
  rules: [
    { match: "cluster = yc and machine in (c500-8-65-*) and group in (infer)", effect: { allow: true, privileges: { hostPID: true, capabilities: ["SYS_PTRACE"] } } },
    { match: "cluster = yc", effect: { allow: true, needsReservation: true, hostPaths: [{ path: "/data" }] } },
    { match: "", effect: { allow: false } },
  ],
  default: { allow: true, privileges: { privileged: true, capabilities: ["*"], hostPID: true, hostIPC: true, hostNetwork: true }, hostPaths: [{ path: "/", writable: true }] },
  resourceVersion: "7",
  problems: [],
};
const preview = {
  clusters: [{ name: "yc", maybe: false }],
  machines: [{ cluster: "yc", count: 2, maybe: 2, names: ["g1", "g2"], unreservable: 1 }],
  users: { count: 3, maybe: 0, total: 10, names: ["a", "b", "c"] },
  hasMachine: true,
};
const page = () => renderWithProviders(<AccessPage />, { me: stubMe({ admin: true }), route: "/admin/access" });

describe("AccessPage", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("shows each rule's condition and the privileges it grants", async () => {
    stubFetch({ "GET /api/admin/access": policy, "/api/admin/groups": { items: [] } });
    page();
    const rows = await screen.findAllByTestId("access-rule");
    expect(rows).toHaveLength(4);
    expect(rows[0].textContent).toContain("cluster = yc and machine in (c500-8-65-*)");
    expect(rows[0].textContent).toContain("主机 PID");
    expect(rows[0].textContent).toContain("cap SYS_PTRACE");
    expect(rows[1].textContent).toContain("需预约");
    expect(rows[1].textContent).toContain("/data（只读）");
    expect(rows[2].textContent).toContain("全部");
    expect(rows[2].textContent).toContain("拒绝");
    expect(rows[3].textContent).toContain("默认");
    expect(rows[3].textContent).toContain("特权容器");
    expect(rows[3].textContent).toContain("任意 capability");
  });

  it("warns about rules that deny everyone", async () => {
    stubFetch({ "GET /api/admin/access": { ...policy, problems: [{ rule: 1, message: "点名了不存在的组 lab" }] }, "/api/admin/groups": { items: [] } });
    page();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("第 2 条");
    expect(alert.textContent).toContain("点名了不存在的组 lab");
  });

  it("moving a rule up and saving sends the whole list in its new order, with the version read", async () => {
    const calls = stubFetch({ "GET /api/admin/access": policy, "/api/admin/groups": { items: [] }, "PUT /api/admin/access": { ...policy, resourceVersion: "8" } });
    page();
    fireEvent.click(await screen.findByRole("button", { name: "上移 规则 2" }));
    fireEvent.click(screen.getByRole("button", { name: "保存" }));
    await waitFor(() => expect(calls.some((c) => c.method === "PUT")).toBe(true));
    const sent = JSON.parse(calls.find((c) => c.method === "PUT")!.body!);
    expect(sent.rules[0].match).toBe("cluster = yc");
    expect(sent.rules[1].match).toBe(policy.rules[0].match);
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

  it("previews a condition as it is typed", async () => {
    const calls = stubFetch({ "GET /api/admin/access": policy, "/api/admin/groups": { items: [] }, "POST /api/admin/access/preview": preview });
    page();
    fireEvent.click(await screen.findByRole("button", { name: "新增规则" }));
    const drawer = within(await screen.findByRole("dialog"));
    fireEvent.change(drawer.getByLabelText("条件"), { target: { value: "machine in (g*)" } });
    // asked once typing pauses (the empty condition is previewed on opening)
    await waitFor(() => {
      const sent = calls.filter((c) => c.method === "POST" && c.url.pathname === "/api/admin/access/preview");
      expect(JSON.parse(sent[sent.length - 1].body!)).toEqual({ match: "machine in (g*)" });
    });
    expect(await drawer.findByText(/2 台机器/)).toBeTruthy();
    expect(drawer.getByText(/可能命中/)).toBeTruthy();
    expect(drawer.getByText(/3 \/ 10 个用户/)).toBeTruthy();
  });

  it("previews the machines cluster by cluster", async () => {
    stubFetch({
      "GET /api/admin/access": policy,
      "/api/admin/groups": { items: [] },
      "POST /api/admin/access/preview": {
        ...preview,
        machines: [
          { cluster: "yc", count: 2, maybe: 0, names: ["g1", "g2"], unreservable: 0 },
          { cluster: "st", count: 1, maybe: 1, names: ["s1"], unreservable: 0 },
        ],
      },
    });
    page();
    fireEvent.click(await screen.findByRole("button", { name: "新增规则" }));
    const drawer = within(await screen.findByRole("dialog"));
    expect(await drawer.findByText(/yc 2 台/)).toBeTruthy();
    expect(drawer.getByText(/st 1 台/)).toBeTruthy();
  });

  it("shows where a condition is wrong and does not save it", async () => {
    stubFetch({
      "GET /api/admin/access": policy,
      "/api/admin/groups": { items: [] },
      "POST /api/admin/access/preview": { error: { offset: 11, message: "in 后面要一个括号列表，如 (a, b)" } },
    });
    page();
    fireEvent.click(await screen.findByRole("button", { name: "新增规则" }));
    const drawer = within(await screen.findByRole("dialog"));
    fireEvent.change(drawer.getByLabelText("条件"), { target: { value: "cluster in st" } });
    expect(await drawer.findByText(/第 12 个字符/)).toBeTruthy();
    // once the condition typed is the one checked
    await waitFor(() => expect((drawer.getByRole("button", { name: "确定" }) as HTMLButtonElement).disabled).toBe(true));
  });

  it("sends the privileges ticked", async () => {
    const calls = stubFetch({
      "GET /api/admin/access": policy,
      "/api/admin/groups": { items: [] },
      "POST /api/admin/access/preview": preview,
      "PUT /api/admin/access": { ...policy, resourceVersion: "8" },
    });
    page();
    fireEvent.click(await screen.findByRole("button", { name: "新增规则" }));
    const drawer = within(await screen.findByRole("dialog"));
    fireEvent.change(drawer.getByLabelText("条件"), { target: { value: "everyone" } });
    fireEvent.click(drawer.getByLabelText("允许"));
    fireEvent.click(drawer.getByLabelText("主机 PID"));
    fireEvent.change(drawer.getByLabelText("可加的 capability"), { target: { value: "sys_ptrace" } });
    await drawer.findByText(/2 台机器/);
    fireEvent.click(drawer.getByRole("button", { name: "确定" }));
    fireEvent.click(await screen.findByRole("button", { name: "保存" }));
    await waitFor(() => expect(calls.some((c) => c.method === "PUT")).toBe(true));
    const sent = JSON.parse(calls.find((c) => c.method === "PUT")!.body!);
    expect(sent.rules[3]).toEqual({ match: "everyone", effect: { allow: true, privileges: { hostPID: true, capabilities: ["sys_ptrace"] } } });
  });

  it("greys the capabilities out when privileged containers are granted", async () => {
    stubFetch({ "GET /api/admin/access": policy, "/api/admin/groups": { items: [] }, "POST /api/admin/access/preview": preview });
    page();
    fireEvent.click(await screen.findByRole("button", { name: "新增规则" }));
    const drawer = within(await screen.findByRole("dialog"));
    fireEvent.click(drawer.getByLabelText("允许"));
    fireEvent.click(drawer.getByLabelText("特权容器"));
    expect((drawer.getByLabelText("可加的 capability") as HTMLInputElement).disabled).toBe(true);
    expect(drawer.getByText(/已含全部 capability/)).toBeTruthy();
  });

  it("warns that a writable host path is no boundary, and when some machines a reservation rule names are not reservable", async () => {
    stubFetch({ "GET /api/admin/access": policy, "/api/admin/groups": { items: [] }, "POST /api/admin/access/preview": preview });
    page();
    fireEvent.click(await screen.findByRole("button", { name: "新增规则" }));
    const drawer = await screen.findByRole("dialog");
    fireEvent.change(within(drawer).getByLabelText("条件"), { target: { value: "machine in (g*)" } });
    fireEvent.click(within(drawer).getByLabelText("允许"));
    fireEvent.click(within(drawer).getByLabelText("需预约"));
    expect(await within(drawer).findByText(/其中 1 台没标可预约/)).toBeTruthy();
    fireEvent.click(within(drawer).getByRole("button", { name: "添加主机路径" }));
    fireEvent.click(within(drawer).getByLabelText("可写"));
    expect(within(drawer).getByText(/可写路径不是安全边界/)).toBeTruthy();
  });
});
