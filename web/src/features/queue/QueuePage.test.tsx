import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
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
    const calls = stubFetch({ "/api/jobs": { items: jobs } });
    page();
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
    fireEvent.click(await screen.findByText(/run01 · sft/));
    expect(await screen.findByText("job page")).toBeTruthy();
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
  });
});
