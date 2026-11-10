import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, within } from "@testing-library/react";
import { Route, Routes } from "react-router-dom";
import { renderWithProviders, stubFetch, stubMe } from "../../test/render";
import { replayable } from "./replay";
import { SessionPage } from "./SessionPage";
import { SessionsPage } from "./SessionsPage";
import type { ChannelRow, SessionDetail } from "../../data/api";

vi.mock("./SessionPlayer", () => ({
  SessionPlayer: ({ sessionId, channel, open, seek, markers }: { sessionId: string; channel: number; open: boolean; seek?: { t: number }; markers?: { t: number; label: string }[] }) => (
    <div data-testid="player" data-markers={(markers ?? []).map((m) => `${m.t}:${m.label}`).join(",")}>
      {sessionId}/{channel}|{open ? "open" : "ended"}|{seek?.t ?? ""}
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
    expect((await screen.findByTestId("player")).textContent).toBe("s1/0|ended|");
    // the command is a tick on the player's scrubber, 30 s in
    expect(screen.getByTestId("player").dataset.markers).toMatch(/^30000:.*make test/);
    const row = screen.getByText(/make test/).closest("li")!;
    fireEvent.click(within(row).getByRole("button", { name: /跳到/ }));
    expect(screen.getByTestId("player").textContent).toBe("s1/0|ended|30000");
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

  it("an open session uses the same player, live, and a timeline click seeks it", async () => {
    const d = session([ch({ pty: true })], {
      commands: [{ event_id: "c1", ts: "2026-11-10T10:00:30Z", session_id: "s1", user: "alice", source: "hook", channel_id: 0, cwd: "/", cmdline: "make test", exit_code: 0, duration_ms: 1, attribution: "tree" }],
    });
    d.session.closed_at = null;
    renderSession(d);
    expect((await screen.findByTestId("player")).textContent).toBe("s1/0|open|");
    expect(screen.queryByRole("radio")).toBeNull(); // no separate live / replay modes
    fireEvent.click(within(screen.getByText(/make test/).closest("li")!).getByRole("button", { name: /跳到/ }));
    expect(screen.getByTestId("player").textContent).toBe("s1/0|open|30000");
  });

  it("an ended session plays its recording", async () => {
    renderSession(session([ch({ pty: true })]));
    expect((await screen.findByTestId("player")).textContent).toBe("s1/0|ended|");
  });

  it("open sessions in the list link 直播", async () => {
    const row = { ...session([]).session, commands: 0, files: 0, policy_hits: 0, alerts: 0 };
    stubFetch({ "/api/audit/sessions": { items: [{ ...row, session_id: "on", closed_at: null }, { ...row, session_id: "off" }], next: "" } });
    renderWithProviders(<SessionsPage />, { route: "/sessions" });
    expect((await screen.findByRole("link", { name: "直播" })).getAttribute("href")).toBe("/sessions/on");
    expect(screen.getByRole("link", { name: "回放" }).getAttribute("href")).toBe("/sessions/off");
  });
});
