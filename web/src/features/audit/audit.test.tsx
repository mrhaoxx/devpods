import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, within } from "@testing-library/react";
import { Route, Routes } from "react-router-dom";
import { renderWithProviders, stubFetch, stubMe } from "../../test/render";
import { replayable } from "./replay";
import { SessionPage } from "./SessionPage";
import { SessionsPage } from "./SessionsPage";
import type { ChannelRow, SessionDetail } from "../../data/api";

vi.mock("./CastPlayer", () => ({
  CastPlayer: ({ src, seekTo }: { src: string; seekTo?: number }) => (
    <div data-testid="player">
      {src}|{seekTo ?? ""}
    </div>
  ),
}));

const ch = (o: Partial<ChannelRow>): ChannelRow => ({
  session_id: "s1",
  channel_id: 0,
  type: "session",
  command: "",
  subsystem: "",
  target: "",
  pty: false,
  term: "",
  opened_at: "2026-11-10T10:00:00Z",
  closed_at: null,
  exit_status: null,
  bytes_in: 0,
  bytes_out: 0,
  raw_recorded: true,
  ...o,
});

const session = (channels: ChannelRow[], extra: Partial<SessionDetail> = {}): SessionDetail => ({
  session: {
    session_id: "s1",
    user: "alice",
    devpod: "alice-x",
    devpod_uid: "u1",
    owner: "alice",
    gateway_pod: "gw",
    client_ip: "10.0.0.9",
    pubkey_fp: "",
    auth_kind: "pubkey",
    proxy_alias: "",
    backend_local_addr: "",
    opened_at: "2026-11-10T10:00:00Z",
    closed_at: "2026-11-10T10:10:00Z",
    close_reason: "",
    bytes_in: 0,
    bytes_out: 0,
    policy_snapshot: "{}",
    gaps: 0,
  },
  channels,
  commands: [],
  files: [],
  policy: [],
  alerts: [],
  ...extra,
});

function renderSession(d: SessionDetail, admin = false) {
  stubFetch({ "/api/audit/sessions/s1": d });
  renderWithProviders(
    <Routes>
      <Route path="/sessions/:id" element={<SessionPage />} />
    </Routes>,
    { route: "/sessions/s1", me: stubMe({ admin }) },
  );
}

describe("audit pages", () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("replayable is true only for pty and shell", () => {
    expect(replayable(ch({ pty: true, command: "vim" }))).toBe(true);
    expect(replayable(ch({}))).toBe(true); // shell
    expect(replayable(ch({ subsystem: "sftp" }))).toBe(false);
    expect(replayable(ch({ command: "scp -t /tmp" }))).toBe(false);
    expect(replayable(ch({ command: "ls -la" }))).toBe(false);
    expect(replayable(ch({ type: "direct-tcpip" }))).toBe(false);
  });

  it("sftp channel shows raw download for admins only", async () => {
    const d = session([ch({ subsystem: "sftp" })]);
    renderSession(d, true);
    await screen.findByText(/不提供回放/);
    expect(screen.queryByTestId("player")).toBeNull();
    expect(screen.getByRole("link", { name: "out" })).toBeTruthy();
  });
  it("sftp channel hides raw download from non-admins", async () => {
    renderSession(session([ch({ subsystem: "sftp" })]), false);
    await screen.findByText(/不提供回放/);
    expect(screen.queryByRole("link", { name: "out" })).toBeNull();
  });

  it("download asks 此操作将被记录 before fetching", async () => {
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    const file = { event_id: "f1", ts: "2026-11-10T10:01:00Z", session_id: "s1", channel_id: 0, user: "alice", protocol: "sftp", op: "download", path: "/etc/hosts", path2: "", size: 120, sha256: "ab", complete: true, blocked: false, s3_key: "k" };
    renderSession(session([ch({ subsystem: "sftp" })], { files: [file] }), true);
    const link = (await screen.findAllByRole("link", { name: /下载/ }))[0];
    const ev = new MouseEvent("click", { bubbles: true, cancelable: true });
    link.dispatchEvent(ev);
    expect(confirm).toHaveBeenCalledWith("此操作将被记录");
    expect(ev.defaultPrevented).toBe(true);
  });

  it("clicking a timeline command seeks the player", async () => {
    const d = session([ch({ pty: true })], {
      commands: [{ event_id: "c1", ts: "2026-11-10T10:00:30Z", session_id: "s1", user: "alice", source: "hook", channel_id: 0, cwd: "/", cmdline: "make test", exit_code: 0, duration_ms: 1, attribution: "tree" }],
    });
    renderSession(d);
    expect((await screen.findByTestId("player")).textContent).toBe("/api/audit/sessions/s1/channels/0/cast|");
    const row = screen.getByText(/make test/).closest("li")!;
    fireEvent.click(within(row).getByRole("button", { name: /跳到/ }));
    expect(screen.getByTestId("player").textContent).toBe("/api/audit/sessions/s1/channels/0/cast|30");
  });

  it("non-admin sessions page has no user filter", async () => {
    stubFetch({ "/api/audit/sessions": { items: [], next: "" } });
    renderWithProviders(<SessionsPage />, { route: "/sessions" });
    await screen.findByText(/没有会话/);
    expect(screen.queryByLabelText("用户")).toBeNull();
    expect(screen.getByLabelText("DevPod")).toBeTruthy();
  });

  it("AUDIT_UNAVAILABLE shows the error panel", async () => {
    stubFetch({ "/api/audit/sessions": new Response(JSON.stringify({ code: "AUDIT_UNAVAILABLE", message: "clickhouse down" }), { status: 503 }) });
    renderWithProviders(<SessionsPage />, { route: "/sessions", me: stubMe({ admin: true }) });
    expect(await screen.findByText("审计数据暂不可用")).toBeTruthy();
  });
});
