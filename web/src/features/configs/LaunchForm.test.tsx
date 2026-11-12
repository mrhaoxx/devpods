import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { renderWithProviders, stubFetch } from "../../test/render";
import type { NodeChoice } from "../../data/api";
import type { ConfigVar } from "./api";
import { LaunchForm, launchNames } from "./LaunchForm";

const v = (name: string, o: Partial<ConfigVar> = {}): ConfigVar => ({ name, builtin: ["NAME", "NODE", "OWNER", "INDEX", "COUNT", "LEADER"].includes(name), required: !["OWNER", "INDEX", "COUNT", "LEADER"].includes(name), type: "string", ...o });
const node = (name: string, o: Partial<NodeChoice> = {}): NodeChoice => ({
  name,
  ready: true,
  cpu: { allocatable: 64, allocated: 0 },
  mem: { allocatable: 1 << 30, allocated: 0 },
  gpuTotal: 0,
  gpuUsed: 0,
  reservable: false,
  pending: 0,
  held: null,
  next: null,
  selectable: true,
  ...o,
});
const pinned = { name: "bob-sg", owner: "bob", title: "SGLang", variables: [v("NAME"), v("OWNER"), v("NODE"), v("IMAGE", { required: false, default: "x" })] };
const plain = { name: "bob-w", owner: "bob", title: "Worker", variables: [v("NAME"), v("GPU")] };
const nodes = { items: [node("m601"), node("m602", { gpuModel: "A100", gpuTotal: 2, gpuUsed: 1 }), node("m603", { selectable: false, held: { user: "carol", until: new Date().toISOString() } })] };
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const type = (label: string, value: string) => fireEvent.change(screen.getByLabelText(label), { target: { value } });
const button = (name: string) => screen.getByRole("button", { name }) as HTMLButtonElement;
const posted = (calls: { method: string; body?: string }[], i = 0) => JSON.parse(calls.filter((c) => c.method === "POST")[i]?.body ?? "null");

describe("launch names", () => {
  it("one keeps the name; several get the index unless the config places it", () => {
    expect(launchNames("alice", "sg", 1, false)).toEqual(["alice-sg"]);
    expect(launchNames("alice", "sg", 3, false)).toEqual(["alice-sg-0", "alice-sg-1", "alice-sg-2"]);
    expect(launchNames("alice", "sg", 2, true)).toEqual(["alice-sg", "alice-sg"]);
  });
});

describe("launch form", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("a pinned config: a field per variable, nodes in click order, one DevPod each", async () => {
    const calls = stubFetch({
      "/api/nodes": nodes,
      "POST /api/configs/bob-sg/launch": json(200, {
        items: [
          { index: 0, name: "alice-sg-0", node: "m602", ok: true },
          { index: 1, name: "alice-sg-1", node: "m601", ok: true },
        ],
      }),
    });
    const done = vi.fn();
    renderWithProviders(<LaunchForm config={pinned} onDone={done} />);
    expect(await screen.findByText("SGLang")).toBeTruthy();
    expect(screen.getByRole("link", { name: "打开配置" }).getAttribute("href")).toBe("/configs/bob-sg");
    expect((screen.getByLabelText("IMAGE") as HTMLInputElement).value).toBe("x");
    expect(screen.queryByLabelText("OWNER")).toBeNull();
    expect(screen.queryByLabelText("NODE")).toBeNull();
    expect(screen.queryByLabelText("数量")).toBeNull();
    expect(button("创建").disabled).toBe(true); // no name, no node
    type("短名", "sg");
    // each node says its GPUs
    expect((await screen.findByRole("button", { name: /m602/ })).textContent).toContain("A100 1/2");
    fireEvent.click(screen.getByRole("button", { name: /m602/ }));
    fireEvent.click(screen.getByRole("button", { name: /m601/ }));
    expect((screen.getByRole("button", { name: /m603/ }) as HTMLButtonElement).disabled).toBe(true);
    const preview = within(screen.getByRole("list", { name: "将创建" })).getAllByRole("listitem");
    expect(preview.map((li) => li.textContent)).toEqual(["alice-sg-0 → m602", "alice-sg-1 → m601"]);
    fireEvent.click(button("创建"));
    await waitFor(() => expect(posted(calls)).toEqual({ vars: { NAME: "sg", IMAGE: "x" }, nodes: ["m602", "m601"] }));
    const results = within(await screen.findByRole("list", { name: "结果" })).getAllByRole("listitem");
    expect(results).toHaveLength(2);
    expect(screen.getByRole("status").textContent).toMatch(/已创建 2 个/);
    expect(screen.getByRole("link", { name: "alice-sg-0" }).getAttribute("href")).toBe("/devpods/alice-sg-0");
    expect(done).toHaveBeenCalledWith({ items: expect.any(Array), group: undefined });
  });

  it("a config without a node takes a count", async () => {
    const calls = stubFetch({ "POST /api/configs/bob-w/launch": json(200, { items: [0, 1, 2].map((i) => ({ index: i, name: `alice-w-${i}`, ok: true })), group: "alice-w" }) });
    renderWithProviders(<LaunchForm config={plain} onDone={() => {}} />);
    expect((screen.getByLabelText("数量") as HTMLInputElement).value).toBe("1");
    type("短名", "w");
    expect(button("创建").disabled).toBe(true); // GPU has no default
    type("GPU", "2");
    expect(within(screen.getByRole("list", { name: "将创建" })).getAllByRole("listitem").map((li) => li.textContent)).toEqual(["alice-w"]);
    type("数量", "3");
    expect(within(screen.getByRole("list", { name: "将创建" })).getAllByRole("listitem")).toHaveLength(3);
    type("数量", "33");
    expect(button("创建").disabled).toBe(true);
    expect(screen.getByRole("alert").textContent).toMatch(/1 到 32/);
    type("数量", "3");
    fireEvent.click(button("创建"));
    await waitFor(() => expect(posted(calls)).toEqual({ vars: { NAME: "w", GPU: "2" }, count: 3 }));
  });

  it("a preview shows each DevPod's YAML and creates nothing", async () => {
    const calls = stubFetch({
      "POST /api/configs/bob-w/launch": json(200, { items: [{ index: 0, name: "alice-w", ok: true, yaml: "kind: DevPod\nmetadata:\n  name: alice-w\n" }] }),
    });
    renderWithProviders(<LaunchForm config={plain} onDone={() => {}} />);
    type("短名", "w");
    type("GPU", "2");
    fireEvent.click(button("预览"));
    await waitFor(() => expect(posted(calls)).toEqual({ vars: { NAME: "w", GPU: "2" }, count: 1, dryRun: true }));
    expect((await screen.findByText(/name: alice-w/)).tagName).toBe("PRE");
    expect(screen.getByRole("status").textContent).toMatch(/检查通过/);
    expect(button("创建").disabled).toBe(false); // still to be created
    // what was checked is no longer what the form says: the result goes
    type("GPU", "4");
    expect(screen.queryByRole("status")).toBeNull();
    expect(screen.queryByText(/name: alice-w/)).toBeNull();
  });

  it("a rejected launch marks the DevPods that cannot be created: nothing was", async () => {
    stubFetch({
      "POST /api/configs/bob-w/launch": json(422, {
        code: "LAUNCH_REJECTED",
        message: "nothing was created",
        items: [
          { index: 0, name: "alice-w-0", ok: true },
          { index: 1, name: "alice-w-1", ok: false, code: "QUOTA_EXCEEDED", message: "cpu over quota", yaml: "kind: DevPod\n" },
        ],
      }),
    });
    renderWithProviders(<LaunchForm config={plain} onDone={() => {}} />);
    type("短名", "w");
    type("GPU", "2");
    type("数量", "2");
    fireEvent.click(button("创建"));
    expect((await screen.findByRole("alert")).textContent).toMatch(/没有创建任何 DevPod/);
    const items = within(screen.getByRole("list", { name: "结果" })).getAllByRole("listitem");
    expect(items[1].textContent).toContain("QUOTA_EXCEEDED");
    expect(items[1].textContent).toContain("cpu over quota");
    expect(screen.queryByRole("link", { name: "alice-w-0" })).toBeNull(); // not created: no link
    expect(button("创建").disabled).toBe(false); // fix the values and try again
  });

  it("a launch that half worked says which", async () => {
    stubFetch({
      "POST /api/configs/bob-w/launch": json(207, {
        items: [
          { index: 0, name: "alice-w-0", ok: true },
          { index: 1, name: "alice-w-1", ok: false, code: "NAME_TAKEN", message: "exists" },
        ],
      }),
    });
    renderWithProviders(<LaunchForm config={plain} onDone={() => {}} />);
    type("短名", "w");
    type("GPU", "2");
    type("数量", "2");
    fireEvent.click(button("创建"));
    expect((await screen.findByRole("status")).textContent).toMatch(/已创建 1 个，失败 1 个/);
    expect(screen.getByRole("link", { name: "alice-w-0" })).toBeTruthy();
    expect(within(screen.getByRole("list", { name: "结果" })).getAllByRole("listitem")[1].textContent).toContain("NAME_TAKEN");
  });

  it("a wrong set of variables is the server's message", async () => {
    stubFetch({ "POST /api/configs/bob-w/launch": json(400, { code: "BAD_VARIABLES", message: "missing variables: CPU", detail: ["CPU"] }) });
    renderWithProviders(<LaunchForm config={plain} onDone={() => {}} />);
    type("短名", "w");
    type("GPU", "2");
    fireEvent.click(button("创建"));
    expect((await screen.findByRole("alert")).textContent).toContain("missing variables: CPU");
    expect(button("创建").disabled).toBe(false);
  });

  it("a config taken away meanwhile closes the form", async () => {
    stubFetch({ "POST /api/configs/bob-w/launch": json(404, { code: "NOT_FOUND", message: "no such config" }) });
    renderWithProviders(<LaunchForm config={plain} onDone={() => {}} />);
    type("短名", "w");
    type("GPU", "2");
    fireEvent.click(button("创建"));
    expect((await screen.findByRole("alert")).textContent).toContain("配置已不可用");
    expect(button("创建").disabled).toBe(true);
    expect(button("预览").disabled).toBe(true);
    expect((screen.getByLabelText("短名") as HTMLInputElement).disabled).toBe(true);
  });

  it("a config that places the index itself, or has no name, says how names come about", () => {
    stubFetch({});
    renderWithProviders(<LaunchForm config={{ name: "bob-i", owner: "bob", title: "I", variables: [v("NAME"), v("INDEX")] }} onDone={() => {}} />);
    type("短名", "x");
    type("数量", "2");
    expect(screen.getByText(/序号由配置里的 \$\{INDEX\} 决定/)).toBeTruthy();
    expect(screen.queryByLabelText("INDEX")).toBeNull();
  });

  it("a config with a fixed name needs no short name", async () => {
    const calls = stubFetch({ "POST /api/configs/bob-f/launch": json(200, { items: [{ index: 0, name: "alice-fixed", ok: true }] }) });
    renderWithProviders(<LaunchForm config={{ name: "bob-f", owner: "bob", title: "F", variables: [] }} onDone={() => {}} />);
    expect(screen.queryByLabelText("短名")).toBeNull();
    fireEvent.click(button("创建"));
    await waitFor(() => expect(posted(calls)).toEqual({ vars: {}, count: 1 }));
  });

  it("only the variables the text has: a config with just ${NAME} asks for a name and a count", () => {
    stubFetch({});
    renderWithProviders(<LaunchForm config={{ name: "bob-n", owner: "bob", title: "N", variables: [v("NAME")] }} onDone={() => {}} />);
    expect(screen.getByLabelText("短名")).toBeTruthy();
    expect(screen.getByLabelText("数量")).toBeTruthy();
    expect(screen.queryByLabelText("GPU")).toBeNull();
    expect(screen.queryByRole("group", { name: /节点/ })).toBeNull();
  });

  it("a control by type; the server's own variables are never asked for", () => {
    stubFetch({});
    const typed = {
      name: "bob-t",
      owner: "bob",
      title: "T",
      variables: [
        v("NAME"),
        v("GPU", { type: "choice", options: ["1", "2", "4"], required: false, default: "2", description: "每个 DevPod 的 GPU 数" }),
        v("N", { type: "int" }),
        v("INDEX"),
        v("COUNT"),
        v("LEADER"),
        v("OWNER"),
      ],
    };
    renderWithProviders(<LaunchForm config={typed} onDone={() => {}} />);
    const gpu = screen.getByLabelText("GPU") as HTMLSelectElement;
    expect(gpu.tagName).toBe("SELECT");
    expect(gpu.value).toBe("2");
    expect([...gpu.options].map((o) => o.value)).toContain("4");
    expect(screen.getByText("每个 DevPod 的 GPU 数")).toBeTruthy();
    expect((screen.getByLabelText("N") as HTMLInputElement).type).toBe("number");
    for (const name of ["INDEX", "COUNT", "LEADER", "OWNER"]) expect(screen.queryByLabelText(name)).toBeNull();
  });

  it("after a preview: what it asks for, and one instance's YAML can be edited for this time only", async () => {
    const yaml = (n: string) => `kind: DevPod\nmetadata:\n  name: ${n}\nspec:\n  pod:\n    spec:\n      containers:\n        - name: dev\n          resources:\n            limits: {cpu: "2", memory: 4Gi, nvidia.com/gpu: 1}\n`;
    stubFetch({
      "POST /api/configs/bob-w/launch": json(200, { items: [0, 1].map((i) => ({ index: i, name: `alice-w-${i}`, ok: true, yaml: yaml(`alice-w-${i}`) })) }),
    });
    const editOnce = vi.fn();
    renderWithProviders(<LaunchForm config={plain} onDone={() => {}} onEditOnce={editOnce} />);
    type("短名", "w");
    type("GPU", "2");
    type("数量", "2");
    fireEvent.click(button("预览"));
    expect((await screen.findByLabelText("本次申请")).textContent).toMatch(/4 核.*8\.0G.*2 GPU/);
    fireEvent.click(screen.getAllByRole("button", { name: "只为这次改一改" })[1]);
    expect(editOnce).toHaveBeenCalledWith(yaml("alice-w-1"));
  });
});
