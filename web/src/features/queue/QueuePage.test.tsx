import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { Route, Routes } from "react-router-dom";
import { QueuePage } from "./QueuePage";
import { renderWithProviders, stubFetch, stubMe } from "../../test/render";

const me = () => stubMe({ features: { ...stubMe().features, queue: true } });
const jobs = [
  { id: "run01", title: "sft", owner: "alice", phase: "Running", nodes: 2, gpus: 8, cpus: 96, timeLimitSeconds: 43200, priority: 200, mine: false, canManage: false, submitted: "2026-11-27T00:00:00Z", startedAt: "2026-11-27T01:00:00Z", preemptible: true },
  { id: "mine1", title: "eval", owner: "u", phase: "Queued", nodes: 1, gpus: 4, cpus: 48, timeLimitSeconds: 3600, priority: 0, mine: true, canManage: true, submitted: "2026-11-27T02:00:00Z", position: 1, reason: "behind", detail: { head: "run01" } },
];
const page = () =>
  renderWithProviders(
    <Routes>
      <Route path="/queue" element={<QueuePage />} />
      <Route path="/queue/:cluster/:id" element={<div>job page</div>} />
    </Routes>,
    { me: me(), route: "/queue" },
  );

describe("QueuePage", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  it("lists the queue: place, job, user, size, priority, limit, state; 我的 filters; a row opens the job", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-11-27T04:00:00Z"));
    const calls = stubFetch({ "/api/jobs": { items: jobs }, "/api/jobs/free": { items: [{ gpu: 4, cpu: 188, maxGPU: 4, machines: 1 }] } });
    page();
    expect(await screen.findByText("队列现在可用：4 卡 · 188 核（单台最多 4 卡）")).toBeTruthy();
    expect(screen.getByText("dp job submit")).toBeTruthy(); // inside a DevPod it is dp's
    await screen.findByText(/run01 · sft/);
    const table = screen.getByRole("table");
    for (const h of ["排位", "任务", "用户", "规模", "优先级", "时长上限", "状态"]) expect(within(table).getByText(h)).toBeTruthy();
    expect(within(table).getByText(/run01 · sft/)).toBeTruthy();
    expect(within(table).getByText("⚡")).toBeTruthy();
    expect(within(table).getByText("2 台 × 8 卡")).toBeTruthy();
    expect(within(table).getByText("● 运行中 · 已 3h，最多再 9h")).toBeTruthy();
    expect(within(table).getByText("○ 等待 · 排在 run01 后")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "我的" }));
    await waitFor(() => expect(calls.some((c) => c.url.pathname === "/api/jobs" && c.url.searchParams.get("mine") === "1")).toBe(true));
    // 全部 clears both at once
    fireEvent.click(screen.getByRole("button", { name: "已结束" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "已结束" }).getAttribute("aria-pressed")).toBe("true"));
    fireEvent.click(screen.getByRole("button", { name: "全部" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "全部" }).getAttribute("aria-pressed")).toBe("true"));
    expect(screen.getByRole("button", { name: "我的" }).getAttribute("aria-pressed")).toBe("false");
    expect(screen.getByRole("button", { name: "已结束" }).getAttribute("aria-pressed")).toBe("false");
    fireEvent.click(await screen.findByText(/run01 · sft/));
    expect(await screen.findByText("job page")).toBeTruthy();
  });

  it("a failed refresh keeps what was shown, saying so", async () => {
    let n = 0;
    stubFetch({
      "/api/jobs": () => (n++ === 0 ? { items: jobs } : new Response(JSON.stringify({ code: "UNAVAILABLE", message: "down" }), { status: 503 })),
    });
    const { client } = page();
    await screen.findByText(/run01 · sft/);
    await act(() => client.refetchQueries({ queryKey: ["jobs"] }));
    expect(await screen.findByText(/刷新失败/)).toBeTruthy();
    expect(screen.getByText(/run01 · sft/)).toBeTruthy();
  });

  it("DevPods waiting in line are listed in it, marked so, in their place", async () => {
    const waiting = { ...jobs[0], id: "qq111", title: "", phase: "Queued", position: 1, reason: "capacity", ranks: undefined, startedAt: undefined };
    stubFetch({
      "/api/jobs": { items: [waiting], devpods: [{ name: "u-dev", owner: "u", mine: true, priority: 0, position: 2, reason: "behind", detail: { head: "qq111" }, queuedAt: "2026-11-27T01:00:00Z", gpus: 1, cpus: 4 }] },
      "/api/jobs/free": { items: [] },
    });
    page();
    expect(await screen.findByText(/DevPod u-dev/)).toBeTruthy();
    const rows = screen.getAllByRole("row").map((r) => r.textContent ?? "");
    const job = rows.findIndex((t) => t.includes("qq111") && t.includes("#1"));
    const devpod = rows.findIndex((t) => t.includes("DevPod u-dev") && t.includes("#2"));
    expect(job).toBeGreaterThan(0);
    expect(devpod).toBeGreaterThan(job);
  });

  it("one's own jobs and DevPods in line are managed from the list: a row's menu, or several picked at once", async () => {
    const mine2 = { ...jobs[1], id: "mine2", position: 2 };
    const calls = stubFetch({
      "/api/jobs": { items: [...jobs, mine2], devpods: [{ name: "u-dev", owner: "u", mine: true, priority: 0, position: 3, reason: "behind", queuedAt: "2026-11-27T01:00:00Z", gpus: 1, cpus: 4 }] },
      "/api/jobs/free": { items: [] },
      "PATCH /api/jobs/mine1": jobs[1],
      "PATCH /api/jobs/mine2": mine2,
      "PATCH /api/devpods/u-dev": { devpod: {}, rebuild: false },
    });
    page();
    await screen.findByText(/DevPod u-dev/);
    // another's running job: nothing to pick, no menu
    expect(screen.queryByLabelText("选择 /run01")).toBeNull();
    expect(screen.queryByRole("button", { name: "run01 操作" })).toBeNull();
    fireEvent.keyDown(screen.getByRole("button", { name: "mine1 操作" }), { key: "Enter" });
    for (const a of ["改优先级…", "暂停", "取消任务…"]) expect(await screen.findByRole("menuitem", { name: a })).toBeTruthy();
    fireEvent.keyDown(screen.getByRole("menu"), { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("menu")).toBeNull());
    fireEvent.click(screen.getByLabelText("选择 /mine1"));
    fireEvent.click(screen.getByLabelText("选择 /mine2"));
    fireEvent.click(await screen.findByRole("button", { name: "暂停" }));
    await waitFor(() =>
      expect(["mine1", "mine2"].every((id) => calls.some((c) => c.method === "PATCH" && c.url.pathname === `/api/jobs/${id}` && c.body === JSON.stringify({ held: true })))).toBe(true),
    );
    fireEvent.keyDown(screen.getByRole("button", { name: "DevPod u-dev 操作" }), { key: "Enter" });
    fireEvent.click(await screen.findByRole("menuitem", { name: "取消排队" }));
    await waitFor(() => expect(calls.some((c) => c.method === "PATCH" && c.url.pathname === "/api/devpods/u-dev" && c.body === JSON.stringify({ running: false }))).toBe(true));
  });

  it("已结束 lists the ended jobs, the last to end first, a page at a time", async () => {
    const old = (id: string, finishedAt: string) => ({ ...jobs[1], id, phase: "Succeeded", position: undefined, reason: undefined, detail: undefined, finishedAt });
    const calls = stubFetch({
      "/api/jobs": (u: URL) =>
        u.searchParams.get("ended") !== "1"
          ? { items: jobs }
          : u.searchParams.get("cursor") === "c2"
            ? { items: [old("old02", "2026-11-26T01:00:00Z")], next: "" }
            : { items: [old("old01", "2026-11-27T01:00:00Z")], next: "c2" },
      "/api/jobs/free": { items: [] },
    });
    // the table's box has a height (jsdom gives none): its end is in sight
    const own = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "clientHeight");
    Object.defineProperty(HTMLElement.prototype, "clientHeight", {
      configurable: true,
      get() {
        return (this as HTMLElement).hasAttribute("data-table-scroller") ? 290 : 0;
      },
    });
    try {
      page();
      await screen.findByText(/mine1/);
      fireEvent.click(screen.getByRole("button", { name: "已结束" }));
      expect(await screen.findByText(/old01/)).toBeTruthy();
      expect(await screen.findByText(/old02/)).toBeTruthy();
      expect(calls.some((c) => c.url.searchParams.get("ended") === "1" && c.url.searchParams.get("cursor") === "c2")).toBe(true);
    } finally {
      if (own) Object.defineProperty(HTMLElement.prototype, "clientHeight", own);
      else delete (HTMLElement.prototype as { clientHeight?: number }).clientHeight;
    }
  });

  it("a working directory the machines may not share is warned of, not refused", async () => {
    const calls = stubFetch({
      "/api/jobs": { items: [] },
      "/api/jobs/limits": { maxPriority: 0, unlimitedPriority: false, defaultTimeLimitSeconds: 86400, maxTimeLimitSeconds: 259200 },
      "/api/devpods": { items: [{ metadata: { name: "u-dev" }, spec: { owner: "u", running: true }, status: {} }] },
      "/api/configs": { items: [] },
      "POST /api/jobs": (_u: URL, init?: RequestInit) =>
        JSON.parse(String(init?.body)).dryRun ? { position: 1, cpus: 4, warnings: ["the job runs in /root, which the machines may not share"] } : { id: "new01", phase: "Queued" },
    });
    page();
    fireEvent.click(await screen.findByRole("button", { name: "提交任务…" }));
    const dialog = within(await screen.findByRole("dialog"));
    await dialog.findByRole("option", { name: "DevPod：u-dev" });
    fireEvent.change(dialog.getByLabelText("来自"), { target: { value: "devpod:u-dev" } });
    fireEvent.change(dialog.getByLabelText("命令"), { target: { value: "make" } });
    fireEvent.change(dialog.getByLabelText("工作目录"), { target: { value: "/root" } });
    expect(await dialog.findByText(/提醒：the job runs in \/root/)).toBeTruthy();
    fireEvent.click(dialog.getByRole("button", { name: "提交" }));
    await waitFor(() => expect(calls.some((c) => c.method === "POST" && !JSON.parse(c.body ?? "{}").dryRun)).toBe(true));
  });

  it("the form submits what it says, shows where it would stand, and holds the priority to the cap", async () => {
    const calls = stubFetch({
      "/api/jobs": { items: [] },
      "/api/jobs/limits": { maxPriority: 0, unlimitedPriority: false, defaultTimeLimitSeconds: 86400, maxTimeLimitSeconds: 259200 },
      "/api/devpods": { items: [{ metadata: { name: "u-dev" }, spec: { owner: "u", running: true }, status: {} }] },
      "/api/configs": { items: [] },
      "POST /api/jobs": (_u: URL, init?: RequestInit) =>
        JSON.parse(String(init?.body)).dryRun ? { position: 3, expectedStart: "2026-11-27T18:10:00Z", reason: "capacity", cpus: 192 } : { id: "new01", phase: "Queued" },
    });
    page();
    fireEvent.click(await screen.findByRole("button", { name: "提交任务…" }));
    const dialog = within(await screen.findByRole("dialog"));
    await dialog.findByRole("option", { name: "DevPod：u-dev" });
    fireEvent.change(dialog.getByLabelText("来自"), { target: { value: "devpod:u-dev" } });
    fireEvent.change(dialog.getByLabelText("命令"), { target: { value: "torchrun t.py" } });
    fireEvent.change(dialog.getByLabelText("台数"), { target: { value: "2" } });
    fireEvent.click(dialog.getByRole("radio", { name: "8 卡" }));
    fireEvent.change(dialog.getByLabelText("时长上限（小时）"), { target: { value: "12" } });
    fireEvent.click(dialog.getByLabelText(/可被抢占/));
    expect(await dialog.findByText(/提交前预估：排第 3 位，预计/)).toBeTruthy();
    fireEvent.change(dialog.getByLabelText("优先级"), { target: { value: "5" } });
    expect(dialog.getByText(/你最高可填 0/)).toBeTruthy();
    expect((dialog.getByRole("button", { name: "提交" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(dialog.getByLabelText("优先级"), { target: { value: "0" } });
    fireEvent.click(dialog.getByRole("button", { name: "提交" }));
    await waitFor(() => expect(calls.some((c) => c.method === "POST" && !JSON.parse(c.body ?? "{}").dryRun)).toBe(true));
    const sent = JSON.parse(calls.filter((c) => c.method === "POST" && !JSON.parse(c.body ?? "{}").dryRun)[0].body!);
    expect(sent).toMatchObject({ from: { devpod: "u-dev" }, nodes: 2, gpus: 8, timeLimitSeconds: 43200, priority: 0, preemptible: true, command: "torchrun t.py" });
    expect(sent.requeue).toBeUndefined(); // waiting again is the default: not said
  });

  it("unchecking 被停时重新排队 submits a job that ends when stopped", async () => {
    const calls = stubFetch({
      "/api/jobs": { items: [] },
      "/api/jobs/limits": { maxPriority: 0, unlimitedPriority: false, defaultTimeLimitSeconds: 86400, maxTimeLimitSeconds: 259200 },
      "/api/devpods": { items: [{ metadata: { name: "u-dev" }, spec: { owner: "u", running: true }, status: {} }] },
      "/api/configs": { items: [] },
      "POST /api/jobs": (_u: URL, init?: RequestInit) => (JSON.parse(String(init?.body)).dryRun ? { position: 1, reason: "capacity", cpus: 24 } : { id: "new02", phase: "Queued" }),
    });
    page();
    fireEvent.click(await screen.findByRole("button", { name: "提交任务…" }));
    const dialog = within(await screen.findByRole("dialog"));
    await dialog.findByRole("option", { name: "DevPod：u-dev" });
    fireEvent.change(dialog.getByLabelText("来自"), { target: { value: "devpod:u-dev" } });
    fireEvent.change(dialog.getByLabelText("命令"), { target: { value: "true" } });
    const box = dialog.getByLabelText(/被停时重新排队/) as HTMLInputElement;
    expect(box.checked).toBe(true);
    fireEvent.click(box);
    fireEvent.click(dialog.getByRole("button", { name: "提交" }));
    await waitFor(() => expect(calls.some((c) => c.method === "POST" && !JSON.parse(c.body ?? "{}").dryRun)).toBe(true));
    const sent = JSON.parse(calls.filter((c) => c.method === "POST" && !JSON.parse(c.body ?? "{}").dryRun)[0].body!);
    expect(sent.requeue).toBe(false);
  });
  it("submits ranks on each machine and a GPU model from the free cards' models", async () => {
    const calls = stubFetch({
      "/api/jobs": { items: [] },
      "/api/jobs/free": { items: [{ gpu: 4, cpu: 188, maxGPU: 4, machines: 2, models: [{ model: "NVIDIA-A100-80GB-PCIe", gpu: 0, total: 3, machines: 3 }, { model: "NVIDIA-GeForce-RTX-4090", gpu: 4, total: 4, machines: 1 }] }] },
      "/api/jobs/limits": { maxPriority: 0, unlimitedPriority: false, defaultTimeLimitSeconds: 86400, maxTimeLimitSeconds: 259200 },
      "/api/devpods": { items: [{ metadata: { name: "u-dev" }, spec: { owner: "u", running: true }, status: {} }] },
      "/api/configs": { items: [] },
      "POST /api/jobs": (_u: URL, init?: RequestInit) => (JSON.parse(String(init?.body)).dryRun ? { position: 1, reason: "capacity", cpus: 24 } : { id: "new03", phase: "Queued" }),
    });
    page();
    fireEvent.click(await screen.findByRole("button", { name: "提交任务…" }));
    const dialog = within(await screen.findByRole("dialog"));
    await dialog.findByRole("option", { name: "DevPod：u-dev" });
    fireEvent.change(dialog.getByLabelText("来自"), { target: { value: "devpod:u-dev" } });
    fireEvent.change(dialog.getByLabelText("命令"), { target: { value: "torchrun t.py" } });
    fireEvent.change(dialog.getByLabelText("每台任务数"), { target: { value: "2" } });
    fireEvent.click(dialog.getByRole("radio", { name: "1 卡" }));
    expect(dialog.getByText("每个任务")).toBeTruthy();
    await dialog.findByRole("option", { name: /NVIDIA-GeForce-RTX-4090/ });
    fireEvent.change(dialog.getByLabelText("GPU 型号"), { target: { value: "NVIDIA-GeForce-RTX-4090" } });
    fireEvent.click(dialog.getByRole("button", { name: "提交" }));
    await waitFor(() => expect(calls.some((c) => c.method === "POST" && !JSON.parse(c.body ?? "{}").dryRun)).toBe(true));
    const sent = JSON.parse(calls.filter((c) => c.method === "POST" && !JSON.parse(c.body ?? "{}").dryRun)[0].body!);
    expect(sent).toMatchObject({ nodes: 1, tasksPerNode: 2, gpus: 1, gpuModel: "NVIDIA-GeForce-RTX-4090" });
  });
  it("a GPU model of one cluster is not sent for a job of another", async () => {
    const free = (cluster: string, model: string) => ({ cluster, gpu: 4, cpu: 100, maxGPU: 4, machines: 1, models: [{ model, gpu: 4, total: 4, machines: 1 }] });
    const limits = { maxPriority: 0, unlimitedPriority: false, defaultTimeLimitSeconds: 86400, maxTimeLimitSeconds: 259200 };
    const post = (_u: URL, init?: RequestInit) => (JSON.parse(String(init?.body)).dryRun ? { position: 1, reason: "capacity", cpus: 24 } : { id: "new04", phase: "Queued" });
    const calls = stubFetch({
      "/api/jobs": { items: [] },
      "/api/clusters": { items: [{ name: "a", capabilities: ["jobs"] }, { name: "b", capabilities: ["jobs"] }] },
      "/api/jobs/free": { items: [free("a", "NVIDIA-A100-80GB-PCIe"), free("b", "NVIDIA-GeForce-RTX-4090")] },
      "/api/clusters/a/jobs/limits": limits,
      "/api/clusters/b/jobs/limits": limits,
      "/api/devpods": {
        items: [
          { metadata: { name: "u-a" }, cluster: "a", spec: { owner: "u", running: true }, status: {} },
          { metadata: { name: "u-b" }, cluster: "b", spec: { owner: "u", running: true }, status: {} },
        ],
      },
      "/api/configs": { items: [] },
      "POST /api/clusters/a/jobs": post,
      "POST /api/clusters/b/jobs": post,
    });
    page();
    fireEvent.click(await screen.findByRole("button", { name: "提交任务…" }));
    const dialog = within(await screen.findByRole("dialog"));
    await dialog.findByRole("option", { name: /DevPod：u-a/ });
    fireEvent.change(dialog.getByLabelText("来自"), { target: { value: "devpod:a/u-a" } });
    fireEvent.change(dialog.getByLabelText("命令"), { target: { value: "true" } });
    await dialog.findByRole("option", { name: /NVIDIA-A100-80GB-PCIe/ });
    fireEvent.change(dialog.getByLabelText("GPU 型号"), { target: { value: "NVIDIA-A100-80GB-PCIe" } });
    fireEvent.change(dialog.getByLabelText("来自"), { target: { value: "devpod:b/u-b" } });
    await dialog.findByRole("option", { name: /NVIDIA-GeForce-RTX-4090/ });
    expect((dialog.getByLabelText("GPU 型号") as HTMLSelectElement).value).toBe("");
    fireEvent.click(dialog.getByRole("button", { name: "提交" }));
    await waitFor(() => expect(calls.some((c) => c.method === "POST" && c.url.pathname === "/api/clusters/b/jobs" && !JSON.parse(c.body ?? "{}").dryRun)).toBe(true));
    const sent = JSON.parse(calls.filter((c) => c.method === "POST" && c.url.pathname === "/api/clusters/b/jobs" && !JSON.parse(c.body ?? "{}").dryRun)[0].body!);
    expect(sent.gpuModel).toBeUndefined();
  });
});
