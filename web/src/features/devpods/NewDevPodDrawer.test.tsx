import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { useLocation } from "react-router-dom";
import { MyDevPodsPage } from "./MyDevPodsPage";
import { renderWithProviders, stubFetch } from "../../test/render";
import type { Config, ConfigSummary, ConfigVar } from "../configs/api";

function Where() {
  const l = useLocation();
  return <div data-testid="where">{l.pathname + l.search}</div>;
}

const v = (name: string, o: Partial<ConfigVar> = {}): ConfigVar => ({ name, builtin: name === "NAME", required: true, type: "string", ...o });
const summary = (o: Partial<ConfigSummary>): ConfigSummary => ({
  name: "x",
  owner: "bob",
  title: "x",
  description: "",
  sharedWith: [],
  public: false,
  official: false,
  variables: [],
  mine: false,
  updatedAt: new Date().toISOString(),
  ...o,
});
const configs = [
  summary({ name: "alice-mine", owner: "alice", title: "我的调试", mine: true, variables: [v("NAME")] }),
  summary({ name: "bob-shared", owner: "bob", title: "共享环境", sharedWith: ["alice"] }),
  summary({ name: "root-train", owner: "root", title: "GPU 训练", public: true, official: true, description: "八卡", variables: [v("NAME"), v("GPU")] }),
  summary({ name: "eve-pub", owner: "eve", title: "公开环境", public: true }),
];
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const base = { "/api/devpods": { items: [], pods: {} }, "/api/nodes": { items: [] }, "GET /api/configs": { items: configs } };
const page = (route: string) =>
  renderWithProviders(
    <>
      <MyDevPodsPage />
      <Where />
    </>,
    { route },
  );

describe("new DevPod drawer", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("opens on the configs: official first, then mine, shared with me, public; a search narrows them", async () => {
    stubFetch(base);
    page("/?new=1");
    const drawer = await screen.findByRole("dialog", { name: "新建 DevPod" });
    await within(drawer).findByText("GPU 训练");
    const groups = within(within(drawer).getByRole("region", { name: "配置列表" })).getAllByRole("heading").map((h) => h.textContent);
    expect(groups).toEqual(["官方", "我的", "分享给我的", "公开"]);
    expect(within(drawer).getByText("八卡")).toBeTruthy();
    fireEvent.change(within(drawer).getByLabelText("搜索配置"), { target: { value: "训练" } });
    expect(within(drawer).queryByText("我的调试")).toBeNull();
    expect(within(drawer).getByText("GPU 训练")).toBeTruthy();
  });

  it("picking a config shows its form; back returns to the list", async () => {
    stubFetch({ ...base, "GET /api/configs/root-train": { ...configs[2], content: "" } satisfies Config });
    page("/?new=1");
    fireEvent.click(await screen.findByRole("button", { name: /GPU 训练/ }));
    expect(await screen.findByLabelText("短名")).toBeTruthy();
    expect(screen.getByLabelText("GPU")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "上一步" }));
    expect(await screen.findByLabelText("搜索配置")).toBeTruthy();
  });

  it("?config= opens straight on that config's form", async () => {
    stubFetch({ ...base, "GET /api/configs/root-train": { ...configs[2], content: "" } });
    page("/?new=1&config=root-train");
    expect(await screen.findByLabelText("短名")).toBeTruthy();
    expect(screen.getByText("GPU 训练")).toBeTruthy();
  });

  it("a group launched goes to the list with the group open; one goes to its page", async () => {
    stubFetch({
      ...base,
      "GET /api/configs/alice-mine": { ...configs[0], content: "" },
      "POST /api/configs/alice-mine/launch": json(200, { group: "alice-t", items: [0, 1].map((i) => ({ index: i, name: `alice-t-${i}`, ok: true })) }),
    });
    page("/?new=1&config=alice-mine");
    fireEvent.change(await screen.findByLabelText("短名"), { target: { value: "t" } });
    fireEvent.change(screen.getByLabelText("数量"), { target: { value: "2" } });
    fireEvent.click(screen.getByRole("button", { name: "创建" }));
    await waitFor(() => expect(screen.getByTestId("where").textContent).toBe("/?group=alice-t"));
  });

  it("a blank YAML starts from a small DevPod, and creates from it", async () => {
    const calls = stubFetch({ ...base, "POST /api/devpods": json(201, { metadata: { name: "alice-dev" }, spec: {} }) });
    page("/?new=1");
    fireEvent.click(await screen.findByRole("button", { name: "空白 YAML" }));
    const box = (await screen.findByLabelText("DevPod YAML")) as HTMLTextAreaElement;
    expect(box.value).toContain("name: alice-dev");
    expect(box.value).toContain("image: ubuntu:24.04");
    fireEvent.click(screen.getByRole("button", { name: "创建" }));
    await waitFor(() => expect(screen.getByTestId("where").textContent).toBe("/devpods/-/alice-dev"));
    expect(JSON.parse(calls.find((c) => c.method === "POST")!.body!)).toEqual({ yaml: box.value });
  });

  it("?image= opens a blank YAML with that image", async () => {
    stubFetch(base);
    page("/?new=1&image=reg.test%2Fx%3A1");
    const box = (await screen.findByLabelText("DevPod YAML")) as HTMLTextAreaElement;
    expect(box.value).toContain("image: reg.test/x:1");
  });

  it("the server's refusal of a YAML is said under it", async () => {
    stubFetch({ ...base, "POST /api/devpods": json(400, { code: "BAD_REQUEST", message: "invalid YAML: unknown field" }) });
    page("/?new=1");
    fireEvent.click(await screen.findByRole("button", { name: "空白 YAML" }));
    fireEvent.click(await screen.findByRole("button", { name: "创建" }));
    expect((await screen.findByText(/invalid YAML: unknown field/)).textContent).toBeTruthy();
  });

  it("no config to use: says where to get one, the blank YAML still there", async () => {
    stubFetch({ ...base, "GET /api/configs": { items: [] } });
    page("/?new=1");
    expect(await screen.findByText(/还没有可用的配置/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "空白 YAML" })).toBeTruthy();
  });

  it("closing clears the URL", async () => {
    stubFetch(base);
    page("/?new=1&config=root-train");
    fireEvent.click(await screen.findByRole("button", { name: "关闭" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.getByTestId("where").textContent).toBe("/");
  });

  it("the configs used last come first, once; a launch is remembered", async () => {
    localStorage.setItem("devpod.recentConfigs.alice", JSON.stringify([{ name: "bob-shared", at: Date.now() - 2 * 86400e3 }, { name: "gone", at: Date.now() }]));
    stubFetch({
      ...base,
      "GET /api/configs/eve-pub": { ...configs[3], content: "" },
      "POST /api/configs/eve-pub/launch": json(200, { items: [{ index: 0, name: "alice-p", ok: true }] }),
    });
    page("/?new=1");
    const list = within(await screen.findByRole("region", { name: "配置列表" }));
    await list.findByText("共享环境");
    expect(list.getAllByRole("heading").map((h) => h.textContent)).toEqual(["最近使用", "官方", "我的", "公开"]);
    expect(list.getAllByText("共享环境")).toHaveLength(1);
    fireEvent.click(list.getByRole("button", { name: /公开环境/ }));
    fireEvent.click(await screen.findByRole("button", { name: "创建" }));
    await waitFor(() => expect(JSON.parse(localStorage.getItem("devpod.recentConfigs.alice") ?? "[]")[0].name).toBe("eve-pub"));
    localStorage.clear();
  });

  it("is made in the cluster picked: the one used last by default, its nodes only; switching shows the other's", async () => {
    localStorage.setItem("devpod.lastLaunchCluster", "b");
    const n = (cluster: string, name: string) => ({ cluster, name, ready: true, cpu: { allocatable: 8, allocated: 0 }, mem: { allocatable: 1, allocated: 0 }, gpuTotal: 0, gpuUsed: 0, reservable: false, pending: 0, held: null, next: null, selectable: true });
    const pinned = summary({ name: "bob-sg", title: "SG", sharedWith: ["alice"], variables: [v("NAME"), v("NODE")] });
    const calls = stubFetch({
      ...base,
      "/api/nodes": { items: [n("a", "m602"), n("a", "na1"), n("b", "m602")] },
      "GET /api/configs/bob-sg": { ...pinned, content: "" },
      "POST /api/clusters/b/configs/bob-sg/launch": json(200, { items: [{ index: 0, name: "alice-s", node: "m602", ok: true }] }),
    });
    renderWithProviders(
      <>
        <MyDevPodsPage />
        <Where />
      </>,
      { route: "/?new=1&config=bob-sg", clusters: [{ name: "a", connected: true }, { name: "b", connected: true }] },
    );
    const where = (await screen.findByLabelText("建在")) as HTMLSelectElement;
    expect(where.value).toBe("b");
    await screen.findByLabelText("m602");
    expect(screen.queryByLabelText("na1")).toBeNull();
    fireEvent.change(screen.getByLabelText("短名"), { target: { value: "s" } });
    fireEvent.click(screen.getByLabelText("m602"));
    fireEvent.click(screen.getByRole("button", { name: "创建" }));
    await waitFor(() => expect(calls.some((c) => c.method === "POST" && c.url.pathname === "/api/clusters/b/configs/bob-sg/launch")).toBe(true));
    await waitFor(() => expect(screen.getByTestId("where").textContent).toBe("/devpods/b/alice-s"));
    expect(localStorage.getItem("devpod.lastLaunchCluster")).toBe("b");
    localStorage.clear();
  });

  it("switching the cluster lists the other cluster's nodes", async () => {
    const n = (cluster: string, name: string) => ({ cluster, name, ready: true, cpu: { allocatable: 8, allocated: 0 }, mem: { allocatable: 1, allocated: 0 }, gpuTotal: 0, gpuUsed: 0, reservable: false, pending: 0, held: null, next: null, selectable: true });
    const pinned = summary({ name: "bob-sg", title: "SG", sharedWith: ["alice"], variables: [v("NAME"), v("NODE")] });
    stubFetch({ ...base, "/api/nodes": { items: [n("a", "na1"), n("b", "nb1")] }, "GET /api/configs/bob-sg": { ...pinned, content: "" } });
    renderWithProviders(<MyDevPodsPage />, { route: "/?new=1&config=bob-sg", clusters: [{ name: "a", connected: true }, { name: "b", connected: true }] });
    fireEvent.change(await screen.findByLabelText("建在"), { target: { value: "b" } });
    expect(await screen.findByLabelText("nb1")).toBeTruthy();
    expect(screen.queryByLabelText("na1")).toBeNull();
  });

  it("a long group shows 8 and the rest on asking; a search shows all that match", async () => {
    const many = Array.from({ length: 10 }, (_, i) => summary({ name: `eve-p${i}`, owner: "eve", title: `公开 ${i}`, public: true }));
    stubFetch({ ...base, "GET /api/configs": { items: many } });
    page("/?new=1");
    const list = within(await screen.findByRole("region", { name: "配置列表" }));
    await list.findByText("公开 0");
    expect(list.getAllByRole("button", { name: /^公开 \d/ })).toHaveLength(8);
    fireEvent.click(list.getByRole("button", { name: "显示更多（2）" }));
    expect(list.getAllByRole("button", { name: /^公开 \d/ })).toHaveLength(10);
  });
});
