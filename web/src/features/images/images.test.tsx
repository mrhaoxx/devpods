import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { renderWithProviders, stubFetch, stubMe } from "../../test/render";
import type { SnapshotProgress, SnapshotView } from "../../data/api";
import { defaultSnapshotName, fmtElapsed, stepStates, validImageRef } from "./progress";
import { timestampTag, withTag } from "./lastImage";
import { SnapshotDialog } from "./SnapshotDialog";
import { ImagesPage } from "./ImagesPage";
import { MyImagesList } from "./MyImages";

const view = (o: Partial<SnapshotView> = {}): SnapshotView => ({
  id: "alice-a-abc123",
  devpod: "alice-a",
  owner: "alice",
  image: "reg.test/users/alice/a:v1",
  phase: "Running",
  digest: "",
  sizeBytes: 0,
  message: "",
  customRegistry: false,
  createdAt: new Date().toISOString(),
  startedAt: null,
  completedAt: null,
  ...o,
});

/** An SSE body the test feeds event by event. */
function progressStream() {
  let ctl!: ReadableStreamDefaultController<Uint8Array>;
  const body = new ReadableStream<Uint8Array>({ start: (c) => void (ctl = c) });
  const enc = new TextEncoder();
  return {
    response: new Response(body, { status: 200, headers: { "Content-Type": "text/event-stream" } }),
    send: (p: SnapshotProgress) => ctl.enqueue(enc.encode(`data: ${JSON.stringify(p)}\n\n`)),
    end: () => ctl.close(),
  };
}
const ev = (o: Partial<SnapshotProgress>): SnapshotProgress => ({ phase: "Running", stage: "pending", elapsedSeconds: 0, lines: [], ...o });
const features = (o: Record<string, boolean> = {}) => stubMe({ features: { pubkeySelfService: true, kore: false, passwordAuth: true, snapshots: true, snapshotDefaultRegistry: true, ...o } });

describe("snapshots", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    window.localStorage.clear(); // remembered image addresses
  });

  it("pure helpers: steps, elapsed, default name, image reference", () => {
    expect(stepStates("pending")).toEqual(["todo", "todo", "todo"]);
    expect(stepStates("commit")).toEqual(["active", "todo", "todo"]);
    expect(stepStates("push")).toEqual(["done", "active", "todo"]);
    expect(stepStates("done")).toEqual(["done", "done", "done"]);
    expect(stepStates("error", "push")).toEqual(["done", "failed", "todo"]);
    expect(fmtElapsed(75)).toBe("1:15");
    expect(defaultSnapshotName("alice-Train_1", "alice")).toBe("train_1");
    expect(validImageRef("reg.example.com/team/name:tag")).toBe(true);
    expect(validImageRef("reg.example.com/team/name")).toBe(false);
    expect(validImageRef("reg.example.com/team/name@sha256:abc")).toBe(false);
    expect(validImageRef("name:tag")).toBe(false);
  });

  it("save to the default registry: the form becomes live progress, then the result", async () => {
    const stream = progressStream();
    const calls = stubFetch({
      "POST /api/devpods/alice-a/snapshots": new Response(JSON.stringify(view({ phase: "Pending" })), { status: 201 }),
      "/api/snapshots/alice-a-abc123/progress": () => stream.response,
      "/api/snapshots": { items: [] },
    });
    renderWithProviders(<SnapshotDialog devpod="alice-a" owner="alice" onClose={() => {}} />, { me: features() });
    expect(((await screen.findByLabelText("名称")) as HTMLInputElement).value).toBe("a");
    expect(screen.getByText(/不含持久卷/)).toBeTruthy();
    expect((screen.getByRole("checkbox") as HTMLInputElement).checked).toBe(true); // pause by default
    fireEvent.click(screen.getByRole("button", { name: "开始保存" }));
    await waitFor(() => expect(calls.find((c) => c.method === "POST")?.body).toBe('{"name":"a","tag":"latest","pause":true}'));

    await screen.findByRole("list", { name: "进度" });
    stream.send(ev({ stage: "commit", elapsedSeconds: 3, lines: ["committing…"] }));
    await waitFor(() => expect(screen.getByRole("status").textContent).toMatch(/正在提交容器.*0:03/));
    expect(screen.getByLabelText("输出").textContent).toBe("committing…");
    stream.send(ev({ stage: "push", elapsedSeconds: 70, sizeBytes: 2048, lines: ["committing…", "pushing layer 1/3"] }));
    await waitFor(() => expect(screen.getByRole("status").textContent).toMatch(/正在推送.*1:10/));
    expect(screen.queryByRole("progressbar")).toBeNull(); // no upload numbers yet
    stream.send(ev({ stage: "push", elapsedSeconds: 75, sizeBytes: 2048, pushedBytes: 512, totalBytes: 2048, lines: ["committing…"] }));
    await waitFor(() => expect(screen.getByRole("progressbar", { name: "推送进度" }).getAttribute("aria-valuenow")).toBe("25"));
    expect(screen.getByRole("status").textContent).toMatch(/已推送\s*512 B \/ 2\.0 KiB|已推送/);
    const steps = within(screen.getByRole("list", { name: "进度" })).getAllByRole("listitem");
    expect(steps.map((s) => s.getAttribute("aria-current"))).toEqual([null, "step", null]);
    expect(screen.getByRole("status").textContent).toContain("2.0K");
    expect(screen.getByRole("button", { name: "取消保存" })).toBeTruthy();

    stream.send(ev({ phase: "Succeeded", stage: "done", elapsedSeconds: 80, digest: "sha256:feed", lines: ["done"] }));
    stream.end();
    expect(await screen.findByText("镜像已保存。")).toBeTruthy();
    expect(screen.getByText("reg.test/users/alice/a:v1", { selector: "code" })).toBeTruthy();
    expect(screen.getByRole("link", { name: "用它新建 DevPod" }).getAttribute("href")).toBe("/?new=1&image=reg.test%2Fusers%2Falice%2Fa%3Av1");
    expect(screen.queryByRole("button", { name: "取消保存" })).toBeNull();
  });

  it("a custom address takes credentials for one push, and cancel asks first", async () => {
    const stream = progressStream();
    const calls = stubFetch({
      "POST /api/devpods/alice-a/snapshots": new Response(JSON.stringify(view({ phase: "Pending", image: "other.test/team/a:v1", customRegistry: true })), { status: 201 }),
      "/api/snapshots/alice-a-abc123/progress": () => stream.response,
      "DELETE /api/snapshots/alice-a-abc123": new Response(null, { status: 204 }),
      "/api/snapshots": { items: [] },
    });
    renderWithProviders(<SnapshotDialog devpod="alice-a" owner="alice" onClose={() => {}} />, { me: features() });
    fireEvent.click(await screen.findByRole("radio", { name: "自定义地址" }));
    const start = screen.getByRole("button", { name: "开始保存" }) as HTMLButtonElement;
    fireEvent.change(screen.getByLabelText("镜像地址"), { target: { value: "other.test/team/a:v1" } });
    // no credentials is a choice (a registry without a login); half a credential is not
    expect(start.disabled).toBe(false);
    expect(screen.getByText(/仓库不需要登录就留空/)).toBeTruthy();
    fireEvent.change(screen.getByLabelText("用户名"), { target: { value: "robot" } });
    expect(start.disabled).toBe(true);
    const pw = screen.getByLabelText("密码") as HTMLInputElement;
    expect(pw.type).toBe("password");
    fireEvent.change(pw, { target: { value: "s3cret" } });
    expect(screen.getByText(/只用于这一次推送/)).toBeTruthy();
    fireEvent.click(start);
    await waitFor(() => expect(calls.find((c) => c.method === "POST")?.body).toBe('{"image":"other.test/team/a:v1","pushAuth":{"username":"robot","password":"s3cret"},"pause":true}'));
    await screen.findByRole("list", { name: "进度" });
    expect(document.body.textContent).not.toContain("s3cret");

    stream.send(ev({ stage: "commit", lines: [] }));
    fireEvent.click(await screen.findByRole("button", { name: "取消保存" }));
    expect(calls.some((c) => c.method === "DELETE")).toBe(false);
    fireEvent.click(within(await screen.findByRole("dialog", { name: "取消这次保存？" })).getByRole("button", { name: "取消保存" }));
    await waitFor(() => expect(calls.some((c) => c.method === "DELETE" && c.url.pathname === "/api/snapshots/alice-a-abc123")).toBe(true));
    stream.send(ev({ stage: "error", message: "已取消" }));
    stream.end();
    expect(await screen.findByText("已取消")).toBeTruthy();
  });

  it("tags: kept by default (saving again overwrites); 生成新标签 stamps the address in the browser and is remembered", async () => {
    expect(timestampTag(new Date(2026, 10, 11, 16, 30, 12))).toBe("20261111-163012");
    expect(withTag("reg.test:5000/team/a:v1", "t2")).toBe("reg.test:5000/team/a:t2");
    expect(withTag("reg.test:5000/team/a", "t2")).toBe("reg.test:5000/team/a:t2");

    const calls = stubFetch({
      "POST /api/devpods/alice-a/snapshots": new Response(JSON.stringify(view({ phase: "Pending", image: "other.test/team/a:x", customRegistry: true })), { status: 201 }),
      "/api/snapshots/alice-a-abc123/progress": () => progressStream().response,
      "/api/snapshots": { items: [] },
    });
    const first = renderWithProviders(<SnapshotDialog devpod="alice-a" owner="alice" onClose={() => {}} />, { me: features() });
    fireEvent.click(await screen.findByRole("radio", { name: "自定义地址" }));
    expect(screen.getByRole("radio", { name: "保留标签" }).getAttribute("aria-checked")).toBe("true");
    fireEvent.click(screen.getByRole("radio", { name: "生成新标签" }));
    // the address may come without a tag now; the old tag is replaced
    fireEvent.change(screen.getByLabelText("镜像地址"), { target: { value: "other.test/team/a:old" } });
    fireEvent.change(screen.getByLabelText("用户名"), { target: { value: "robot" } });
    fireEvent.change(screen.getByLabelText("密码"), { target: { value: "pw" } });
    fireEvent.click(screen.getByRole("button", { name: "开始保存" }));
    await waitFor(() => expect(calls.some((c) => c.method === "POST")).toBe(true));
    const sent = JSON.parse(calls.find((c) => c.method === "POST")!.body!) as { image: string };
    expect(sent.image).toMatch(/^other\.test\/team\/a:20\d{6}-\d{6}$/);
    first.unmount();

    // the choice comes back next time
    renderWithProviders(<SnapshotDialog devpod="alice-b" owner="alice" onClose={() => {}} />, { me: features() });
    await waitFor(() => expect(screen.getByRole("radio", { name: "生成新标签" }).getAttribute("aria-checked")).toBe("true"));
  });

  it("remembers the last custom address in this browser, never the credentials", async () => {
    const stream = progressStream();
    stubFetch({
      "POST /api/devpods/alice-a/snapshots": new Response(JSON.stringify(view({ phase: "Pending", image: "other.test/team/a:v1", customRegistry: true })), { status: 201 }),
      "/api/snapshots/alice-a-abc123/progress": () => stream.response,
      "/api/snapshots": { items: [] },
    });
    const first = renderWithProviders(<SnapshotDialog devpod="alice-a" owner="alice" onClose={() => {}} />, { me: features() });
    fireEvent.click(await screen.findByRole("radio", { name: "自定义地址" }));
    fireEvent.change(screen.getByLabelText("镜像地址"), { target: { value: "other.test/team/a:v1" } });
    fireEvent.change(screen.getByLabelText("用户名"), { target: { value: "robot" } });
    fireEvent.change(screen.getByLabelText("密码"), { target: { value: "s3cret" } });
    fireEvent.click(screen.getByRole("button", { name: "开始保存" }));
    await screen.findByRole("list", { name: "进度" });
    expect(JSON.stringify({ ...window.localStorage })).not.toContain("s3cret");
    expect(JSON.stringify({ ...window.localStorage })).not.toContain("robot");
    first.unmount();

    // the same DevPod opens on the custom address, prefilled
    renderWithProviders(<SnapshotDialog devpod="alice-a" owner="alice" onClose={() => {}} />, { me: features() });
    await waitFor(() => expect((screen.getByLabelText("镜像地址") as HTMLInputElement).value).toBe("other.test/team/a:v1"));
    expect(screen.getByRole("radio", { name: "自定义地址" }).getAttribute("aria-checked")).toBe("true");
    expect((screen.getByLabelText("密码") as HTMLInputElement).value).toBe("");
  });

  it("a custom address without credentials is pushed as nobody", async () => {
    const calls = stubFetch({
      "POST /api/devpods/alice-a/snapshots": new Response(JSON.stringify(view({ phase: "Pending", image: "10.0.0.8:32500/dh/a:v1", customRegistry: true })), { status: 201 }),
      "/api/snapshots/alice-a-abc123/progress": () => new Response(""),
      "/api/snapshots": { items: [] },
    });
    renderWithProviders(<SnapshotDialog devpod="alice-a" owner="alice" onClose={() => {}} />, { me: features() });
    fireEvent.click(await screen.findByRole("radio", { name: "自定义地址" }));
    fireEvent.change(screen.getByLabelText("镜像地址"), { target: { value: "10.0.0.8:32500/dh/a:v1" } });
    fireEvent.click(screen.getByRole("button", { name: "开始保存" }));
    await waitFor(() => expect(calls.find((c) => c.method === "POST")?.body).toBe('{"image":"10.0.0.8:32500/dh/a:v1","pause":true}'));
  });

  it("an address on a saved registry pushes with the saved credential; 改用其他凭证 brings the fields back", async () => {
    const stream = progressStream();
    const calls = stubFetch({
      "/api/me/registries": { items: [{ host: "harbor.test", username: "robot", pending: false }] },
      "POST /api/devpods/alice-a/snapshots": new Response(JSON.stringify(view({ phase: "Pending", image: "harbor.test/team/a:v1", customRegistry: true })), { status: 201 }),
      "/api/snapshots/alice-a-abc123/progress": () => stream.response,
      "/api/snapshots": { items: [] },
    });
    renderWithProviders(<SnapshotDialog devpod="alice-a" owner="alice" onClose={() => {}} />, { me: features() });
    fireEvent.click(await screen.findByRole("radio", { name: "自定义地址" }));
    const start = screen.getByRole("button", { name: "开始保存" }) as HTMLButtonElement;
    // another registry: credentials are asked for as before
    fireEvent.change(screen.getByLabelText("镜像地址"), { target: { value: "other.test/team/a:v1" } });
    expect(screen.getByLabelText("用户名")).toBeTruthy();
    expect(start.disabled).toBe(false); // no credentials: pushed as nobody
    // the saved registry: no fields, ready to go
    fireEvent.change(screen.getByLabelText("镜像地址"), { target: { value: "harbor.test/team/a:v1" } });
    await waitFor(() => expect(screen.getByRole("status").textContent).toMatch(/将使用已保存的凭证（用户名 robot）/));
    expect(screen.queryByLabelText("用户名")).toBeNull();
    expect(screen.queryByLabelText("密码")).toBeNull();
    expect(start.disabled).toBe(false);
    fireEvent.click(screen.getByRole("button", { name: "改用其他凭证" }));
    expect(screen.getByLabelText("用户名")).toBeTruthy();
    expect(start.disabled).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "改回已保存的凭证" }));
    expect(screen.queryByLabelText("密码")).toBeNull();
    fireEvent.click(start);
    await waitFor(() => expect(calls.find((c) => c.method === "POST")?.body).toBe('{"image":"harbor.test/team/a:v1","pause":true}'));
  });

  it("without a default registry only the custom address is offered", async () => {
    stubFetch({ "/api/snapshots": { items: [] } });
    renderWithProviders(<SnapshotDialog devpod="alice-a" owner="alice" onClose={() => {}} />, { me: features({ snapshotDefaultRegistry: false }) });
    expect(await screen.findByLabelText("镜像地址")).toBeTruthy();
    expect(screen.queryByRole("radiogroup", { name: "目标" })).toBeNull();
    expect(screen.queryByLabelText("名称")).toBeNull();
  });

  it("the images page lists snapshots; delete says the registry image stays", async () => {
    const calls = stubFetch({
      "/api/snapshots": {
        items: [
          view({ id: "s-ok", phase: "Succeeded", sizeBytes: 3 * 1024 * 1024 }),
          view({ id: "s-run", phase: "Running", image: "reg.test/users/alice/b:v2" }),
          view({ id: "s-bad", phase: "Failed", image: "reg.test/users/alice/c:v3", message: "push denied" }),
        ],
      },
      "DELETE /api/snapshots/s-ok": new Response(null, { status: 204 }),
    });
    renderWithProviders(<ImagesPage />, { me: features() });
    const ok = (await screen.findByText("reg.test/users/alice/a:v1")).closest("tr")!;
    expect(within(ok).getByText("已保存")).toBeTruthy();
    expect(within(ok).getByText("3.0M")).toBeTruthy();
    expect(within(ok).getByRole("link", { name: "新建 DevPod" }).getAttribute("href")).toContain("image=reg.test");
    const run = screen.getByText("reg.test/users/alice/b:v2").closest("tr")!;
    expect(within(run).getByRole("button", { name: "查看进度" })).toBeTruthy();
    expect(within(run).queryByRole("button", { name: /删除/ })).toBeNull();
    expect(screen.getByText("push denied")).toBeTruthy();
    expect(screen.queryByLabelText("全部用户")).toBeNull(); // not an admin
    fireEvent.click(within(ok).getByRole("button", { name: /删除/ }));
    expect(await screen.findByText(/仓库里的镜像/)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "删除记录" }));
    await waitFor(() => expect(calls.some((c) => c.method === "DELETE" && c.url.pathname === "/api/snapshots/s-ok")).toBe(true));
  });

  it("image inputs are offered the saved images", async () => {
    stubFetch({ "/api/snapshots": { items: [view({ phase: "Succeeded" }), view({ id: "x", phase: "Failed", image: "reg.test/users/alice/bad:v1" })] } });
    renderWithProviders(<MyImagesList />, { me: features() });
    const list = await screen.findByTestId("my-images");
    expect([...list.querySelectorAll("option")].map((o) => o.getAttribute("value"))).toEqual(["reg.test/users/alice/a:v1"]);
  });
});
