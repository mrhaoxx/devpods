import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { Route, Routes } from "react-router-dom";
import { JobPage } from "./JobPage";
import { renderWithProviders, stubFetch, stubMe } from "../../test/render";

const me = () => stubMe({ user: "u", features: { ...stubMe().features, queue: true } });
const ended = {
  cluster: "st", id: "abc12", title: "sft", owner: "u", phase: "Failed", detail: { exit: "3" }, nodes: 2, gpus: 8, cpus: 96, timeLimitSeconds: 43200, priority: 5,
  mine: true, canManage: true, submitted: "2026-11-27T00:00:00Z", startedAt: "2026-11-27T01:00:00Z", finishedAt: "2026-11-27T02:00:00Z", attempts: 1,
  command: "torchrun t.py", workdir: "/home/u/x",
  ranks: [
    { rank: 0, node: "g1", devpod: "u-abc12-0", phase: "Succeeded", exitCode: 0, log: "jobs/st/abc12/rank-0.log" },
    { rank: 1, node: "g2", devpod: "u-abc12-1", phase: "Failed", exitCode: 3, log: "jobs/st/abc12/rank-1.log" },
  ],
};
const page = (route = "/queue/st/abc12") =>
  renderWithProviders(
    <Routes>
      <Route path="/queue/:cluster/:id" element={<JobPage />} />
      <Route path="/queue" element={<div>queue page</div>} />
    </Routes>,
    { me: me(), route },
  );

describe("JobPage", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("shows the job, its ranks and how each ended, and an ended rank's kept output", async () => {
    const calls = stubFetch({
      "/api/jobs/abc12": ended,
      "/api/jobs/abc12/logs": (u: URL) => new Response(`output of rank ${u.searchParams.get("rank")}\n`, { status: 200, headers: { "Content-Type": "text/plain" } }),
    });
    page();
    expect(await screen.findByText("失败（退出码 3）")).toBeTruthy();
    expect(screen.getByText("torchrun t.py")).toBeTruthy();
    const ranks = within(screen.getByRole("table", { name: "各台" }));
    expect(ranks.getByText("g2")).toBeTruthy();
    expect(ranks.getByText("3")).toBeTruthy();
    expect(await screen.findByText(/output of rank 0/)).toBeTruthy();
    fireEvent.change(screen.getByLabelText("看第几台"), { target: { value: "1" } });
    expect(await screen.findByText(/output of rank 1/)).toBeTruthy();
    expect(calls.some((c) => c.url.pathname === "/api/jobs/abc12/logs" && c.url.searchParams.get("rank") === "1" && !c.url.searchParams.has("follow"))).toBe(true);
    expect(screen.getByRole("link", { name: "下载" }).getAttribute("href")).toBe("/api/jobs/abc12/logs?rank=1");
  });

  it("记录 lists what was done to the job", async () => {
    stubFetch({
      "/api/jobs/abc12": ended,
      "/api/jobs/abc12/logs": () => new Response("", { status: 200, headers: { "Content-Type": "text/plain" } }),
      "/api/jobs/abc12/history": {
        items: [{ ts: "2026-11-27T09:00:00Z", kind: "job", object: "abc12", owner: "alice", actor: "system:queue", via: "system", action: "start", detail: "g1,g2" }],
        next: "",
      },
    });
    page();
    expect(await screen.findByText("启动")).toBeTruthy();
    expect(screen.getByText("g1,g2")).toBeTruthy();
  });

  it("a job started more than once: each start's output, the last first", async () => {
    const calls = stubFetch({
      "/api/jobs/abc12": { ...ended, attempts: 2, preemptions: 1 },
      "/api/jobs/abc12/logs": (u: URL) => new Response(`attempt ${u.searchParams.get("attempt") ?? "last"}\n`, { status: 200, headers: { "Content-Type": "text/plain" } }),
    });
    page();
    expect(await screen.findByText(/attempt last/)).toBeTruthy();
    fireEvent.change(screen.getByLabelText("看第几次"), { target: { value: "1" } });
    expect(await screen.findByText(/attempt 1/)).toBeTruthy();
    expect(calls.some((c) => c.url.pathname === "/api/jobs/abc12/logs" && c.url.searchParams.get("attempt") === "1")).toBe(true);
    expect(screen.getByRole("link", { name: "下载" }).getAttribute("href")).toBe("/api/jobs/abc12/logs?rank=0&attempt=1");
  });

  it("a failed refresh keeps the job shown, saying so", async () => {
    let n = 0;
    stubFetch({
      "/api/jobs/abc12": () => (n++ === 0 ? ended : new Response(JSON.stringify({ code: "UNAVAILABLE", message: "down" }), { status: 503 })),
      "/api/jobs/abc12/logs": new Response("x", { status: 200 }),
    });
    const { client } = page();
    expect(await screen.findByText("失败（退出码 3）")).toBeTruthy();
    await act(() => client.refetchQueries({ queryKey: ["job", "abc12"] }));
    expect(await screen.findByText(/刷新失败/)).toBeTruthy();
    expect(screen.getByText("失败（退出码 3）")).toBeTruthy();
  });

  it("another user's job: no output, no command, no buttons", async () => {
    stubFetch({ "/api/jobs/abc12": { ...ended, owner: "carol", mine: false, canManage: false, command: undefined, workdir: undefined, phase: "Running", detail: undefined } });
    page();
    expect(await screen.findByText(/运行中/)).toBeTruthy();
    expect(screen.queryByLabelText("看第几台")).toBeNull();
    expect(screen.queryByText("torchrun t.py")).toBeNull();
    expect(screen.queryByRole("button", { name: "取消任务" })).toBeNull();
  });

  it("取消 asks first, then cancels", async () => {
    const calls = stubFetch({
      "GET /api/jobs/abc12": { ...ended, phase: "Queued", position: 2, reason: "behind", detail: { head: "x" }, ranks: [], startedAt: undefined, finishedAt: undefined },
      "DELETE /api/jobs/abc12": new Response(null, { status: 204 }),
    });
    page();
    fireEvent.click(await screen.findByRole("button", { name: "取消任务" }));
    expect(calls.some((c) => c.method === "DELETE")).toBe(false);
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "取消任务" }));
    await waitFor(() => expect(calls.some((c) => c.method === "DELETE" && c.url.pathname === "/api/jobs/abc12")).toBe(true));
  });

  it("重新提交 makes the same job again and goes to it", async () => {
    const calls = stubFetch({
      "/api/jobs/abc12": ended,
      "/api/jobs/abc12/logs": new Response("x", { status: 200 }),
      "POST /api/jobs/abc12/resubmit": { ...ended, id: "new77", phase: "Queued" },
      "/api/jobs/new77": { ...ended, id: "new77", phase: "Queued", position: 1, reason: "capacity", detail: { need: "16", free: "8" }, ranks: [] },
    });
    page();
    fireEvent.click(await screen.findByRole("button", { name: "重新提交" }));
    await waitFor(() => expect(calls.some((c) => c.method === "POST" && c.url.pathname === "/api/jobs/abc12/resubmit")).toBe(true));
    expect(await screen.findByText(/new77/)).toBeTruthy();
  });
});
