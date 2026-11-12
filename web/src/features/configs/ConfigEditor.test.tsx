import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { Navigate, Route, Routes, useLocation } from "react-router-dom";
import { renderWithProviders, stubFetch } from "../../test/render";
import type { Config } from "./api";
import { ConfigEditor } from "./ConfigEditor";

const cfg = (o: Partial<Config> = {}): Config => ({
  name: "alice-sg",
  owner: "alice",
  title: "SGLang",
  description: "八卡",
  content: "name: ${NAME}\nimage: ${IMAGE:-ubuntu:24.04}\n",
  sharedWith: ["bob"],
  public: false,
  official: false,
  variables: [],
  mine: true,
  updatedAt: new Date().toISOString(),
  ...o,
});

function Where() {
  const l = useLocation();
  return <div data-testid="where">{l.pathname + JSON.stringify(l.state ?? null)}</div>;
}
const app = (
  <Routes>
    <Route path="/configs/new" element={<ConfigEditor />} />
    <Route path="/configs/:name" element={<ConfigEditor />} />
    <Route path="/configs" element={<Where />} />
  </Routes>
);
const vars = () => within(screen.getByRole("list", { name: "变量" }));
const type = (label: string, value: string) => fireEvent.change(screen.getByLabelText(label), { target: { value } });

describe("config editor", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("takes up to 256 KiB of YAML", () => {
    stubFetch({});
    renderWithProviders(app, { route: "/configs/new" });
    type("YAML", "a".repeat(200 << 10));
    expect(screen.queryByText("内容超过 256 KiB")).toBeNull();
    type("YAML", "a".repeat((256 << 10) + 1));
    expect(screen.getByText("内容超过 256 KiB")).toBeTruthy();
  });

  it("a new config: the variables are listed as they are typed, then it is saved", async () => {
    const calls = stubFetch({ "POST /api/configs": new Response(JSON.stringify(cfg({ name: "alice-gpu" })), { status: 201 }) });
    renderWithProviders(app, { route: "/configs/new" });
    expect(screen.getByText("还没有变量")).toBeTruthy();
    type("YAML", "name: ${NAME}\ngpu: '${GPU:-8}'\nowner: ${OWNER}\necho ${home}");
    expect(vars().getByText("GPU")).toBeTruthy();
    expect(vars().getByText(/默认 8/)).toBeTruthy();
    expect(vars().getByText("NAME")).toBeTruthy();
    expect(vars().getByText(/自动填入当前用户/)).toBeTruthy();
    expect(vars().queryByText("home")).toBeNull();
    // nothing to save without an id and a title
    expect((screen.getByRole("button", { name: "保存" }) as HTMLButtonElement).disabled).toBe(true);
    type("标识", "gpu");
    type("标题", "GPU 配置");
    type("分享给", "bob, carol bob");
    fireEvent.click(screen.getByLabelText("公开"));
    fireEvent.click(screen.getByRole("button", { name: "保存" }));
    await waitFor(() =>
      expect(JSON.parse(calls.find((c) => c.method === "POST" && c.url.pathname === "/api/configs")?.body ?? "{}")).toEqual({
        id: "gpu",
        title: "GPU 配置",
        description: "",
        content: "name: ${NAME}\ngpu: '${GPU:-8}'\nowner: ${OWNER}\necho ${home}",
        sharedWith: ["bob", "carol"],
        public: true,
      }),
    );
    await waitFor(() => expect(screen.getByTestId("where").textContent).toContain("/configs"));
  });

  it("a new config may start from content handed over", async () => {
    stubFetch({});
    renderWithProviders(
      <Routes>
        <Route path="/go" element={<Navigate to="/configs/new" state={{ content: "a: ${X}", title: "来自 DevPod" }} />} />
        <Route path="/configs/new" element={<ConfigEditor />} />
      </Routes>,
      { route: "/go" },
    );
    expect(((await screen.findByLabelText("YAML")) as HTMLTextAreaElement).value).toBe("a: ${X}");
    expect((screen.getByLabelText("标题") as HTMLInputElement).value).toBe("来自 DevPod");
    expect(vars().getByText("X")).toBeTruthy();
  });

  it("a bad id is said before saving", () => {
    stubFetch({});
    renderWithProviders(app, { route: "/configs/new" });
    type("标识", "Bad_Id");
    expect(screen.getByText(/小写字母、数字和连字符/)).toBeTruthy();
    type("标题", "t");
    type("YAML", "a: b");
    expect((screen.getByRole("button", { name: "保存" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("my config: the id is fixed, a save sends what changed", async () => {
    const calls = stubFetch({ "GET /api/configs/alice-sg": cfg(), "PATCH /api/configs/alice-sg": cfg({ title: "SGLang 8" }) });
    renderWithProviders(app, { route: "/configs/alice-sg" });
    const id = (await screen.findByLabelText("标识")) as HTMLInputElement;
    expect(id.value).toBe("sg");
    expect(id.readOnly).toBe(true);
    expect((screen.getByLabelText("分享给") as HTMLInputElement).value).toBe("bob");
    expect(vars().getByText("IMAGE")).toBeTruthy();
    type("标题", "SGLang 8");
    fireEvent.click(screen.getByRole("button", { name: "保存" }));
    await waitFor(() => expect(JSON.parse(calls.find((c) => c.method === "PATCH")?.body ?? "{}")).toEqual({ title: "SGLang 8" }));
  });

  it("the server's refusal is shown", async () => {
    stubFetch({
      "GET /api/configs/alice-sg": cfg(),
      "PATCH /api/configs/alice-sg": new Response(JSON.stringify({ code: "BAD_REQUEST", message: '"Not A User" is not a user name' }), { status: 400 }),
    });
    renderWithProviders(app, { route: "/configs/alice-sg" });
    await screen.findByLabelText("标识");
    type("分享给", "Not A User");
    fireEvent.click(screen.getByRole("button", { name: "保存" }));
    expect((await screen.findByRole("alert")).textContent).toContain("is not a user name");
  });

  it("someone else's config is read-only, with a way to copy it", async () => {
    stubFetch({ "GET /api/configs/bob-x": cfg({ name: "bob-x", owner: "bob", mine: false, sharedWith: [] }) });
    renderWithProviders(
      <Routes>
        <Route path="/configs/new" element={<Where />} />
        <Route path="/configs/:name" element={<ConfigEditor />} />
      </Routes>,
      { route: "/configs/bob-x" },
    );
    const yaml = (await screen.findByLabelText("YAML")) as HTMLTextAreaElement;
    expect(yaml.readOnly).toBe(true);
    expect(screen.queryByRole("button", { name: "保存" })).toBeNull();
    expect(screen.queryByLabelText("分享给")).toBeNull();
    expect(screen.getByText(/bob 的配置/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "另存为副本" }));
    const where = (await screen.findByTestId("where")).textContent ?? "";
    expect(where).toContain("/configs/new");
    expect(where).toContain("${IMAGE:-ubuntu:24.04}");
    expect(where).toContain("SGLang");
  });

  it("a config that is not there", async () => {
    stubFetch({});
    renderWithProviders(app, { route: "/configs/alice-gone" });
    expect(await screen.findByText("没有这个配置，或它没有分享给你")).toBeTruthy();
  });

  it("a save is checked first; what the check finds is said, and may be saved anyway", async () => {
    const calls = stubFetch({
      "GET /api/configs/alice-sg": cfg(),
      "POST /api/configs:check": { problems: [{ line: 2, code: "BAD_YAML", message: "unknown field runnning" }] },
      "PATCH /api/configs/alice-sg": cfg({ title: "SGLang 8" }),
    });
    renderWithProviders(app, { route: "/configs/alice-sg" });
    await screen.findByLabelText("标识");
    type("标题", "SGLang 8");
    fireEvent.click(screen.getByRole("button", { name: "保存" }));
    const found = await screen.findByRole("region", { name: "保存前检查" });
    expect(found.textContent).toContain("第 2 行");
    expect(found.textContent).toContain("unknown field runnning");
    expect(calls.some((c) => c.method === "PATCH")).toBe(false);
    expect(JSON.parse(calls.find((c) => c.url.pathname === "/api/configs:check")!.body!).content).toBe(cfg().content);
    fireEvent.click(within(found).getByRole("button", { name: "仍然保存" }));
    await waitFor(() => expect(JSON.parse(calls.find((c) => c.method === "PATCH")?.body ?? "{}")).toEqual({ title: "SGLang 8" }));
  });

  it("what is said of a variable is saved with the config", async () => {
    const calls = stubFetch({ "GET /api/configs/alice-sg": cfg(), "POST /api/configs:check": { problems: [] }, "PATCH /api/configs/alice-sg": cfg() });
    renderWithProviders(app, { route: "/configs/alice-sg" });
    await screen.findByLabelText("标识");
    fireEvent.change(screen.getByLabelText("IMAGE 的类型"), { target: { value: "image" } });
    fireEvent.click(screen.getByRole("button", { name: "保存" }));
    await waitFor(() => expect(JSON.parse(calls.find((c) => c.method === "PATCH")?.body ?? "{}")).toEqual({ variables: [{ name: "IMAGE", type: "image" }] }));
  });

  it("lists the DevPods made from it", async () => {
    const dp = (name: string, config?: string) => ({ metadata: { name, labels: config ? { "devpod.io/config": config } : {} }, spec: { owner: "alice", running: true }, status: { phase: "Running" } });
    stubFetch({ "GET /api/configs/alice-sg": cfg(), "/api/devpods": { items: [dp("alice-a", "alice-sg"), dp("alice-b", "other"), dp("alice-c")], pods: {} } });
    renderWithProviders(app, { route: "/configs/alice-sg" });
    const made = within(await screen.findByRole("region", { name: "由它建出的 DevPod" }));
    expect(await made.findByRole("link", { name: "alice-a" })).toBeTruthy();
    expect(made.queryByRole("link", { name: "alice-b" })).toBeNull();
    expect(made.queryByRole("link", { name: "alice-c" })).toBeNull();
  });

  it("what is said of a variable no longer in the text is not saved", async () => {
    const calls = stubFetch({ "GET /api/configs/alice-sg": cfg(), "POST /api/configs:check": { problems: [] }, "PATCH /api/configs/alice-sg": cfg() });
    renderWithProviders(app, { route: "/configs/alice-sg" });
    await screen.findByLabelText("标识");
    fireEvent.change(screen.getByLabelText("IMAGE 的说明"), { target: { value: "基础镜像" } });
    type("YAML", "name: ${NAME}\n");
    fireEvent.click(screen.getByRole("button", { name: "保存" }));
    await waitFor(() => expect(JSON.parse(calls.find((c) => c.method === "PATCH")?.body ?? "{}")).toEqual({ content: "name: ${NAME}\n" }));
  });
});
