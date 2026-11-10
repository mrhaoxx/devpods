import { afterEach, describe, expect, it } from "vitest";
import { act, screen, waitFor } from "@testing-library/react";
import { useLocation } from "react-router-dom";
import { vi } from "vitest";
import { AppRoutes } from "./routes";
import { renderWithProviders, stubFetch, stubMe } from "../test/render";
import { LiveHub } from "../data/live/LiveHub";
import { devpodsStream } from "../data/live/streams";
import type { SseHandlers } from "../data/sse";

function Where() {
  const loc = useLocation();
  return <div data-testid="where">{loc.pathname}</div>;
}

const app = () => (
  <>
    <AppRoutes />
    <Where />
  </>
);

const empty = { items: [], pods: {} };

describe("console shell and routes", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("redirects /settings/pubkeys and /settings/password to /settings", async () => {
    stubFetch({ "/api/me/pubkeys": { pubkeys: [] }, "/api/devpods": empty });
    for (const route of ["/settings/pubkeys", "/settings/password"]) {
      const r = renderWithProviders(app(), { route });
      await waitFor(() => expect(screen.getByTestId("where").textContent).toBe("/settings"));
      r.unmount();
    }
  });

  it("redirects /admin/topology to /admin/nodes", async () => {
    stubFetch({ "/api/devpods": empty });
    renderWithProviders(app(), { route: "/admin/topology", me: stubMe({ admin: true }) });
    await waitFor(() => expect(screen.getByTestId("where").textContent).toBe("/admin/nodes"));
  });

  it("redirects /audit/sessions/abc to /sessions/abc", async () => {
    stubFetch({ "/api/devpods": empty });
    renderWithProviders(app(), { route: "/audit/sessions/abc" });
    await waitFor(() => expect(screen.getByTestId("where").textContent).toBe("/sessions/abc"));
  });

  it("non-admin visiting /admin/users is sent to / and sees no 集群 group", async () => {
    stubFetch({ "/api/devpods": empty });
    renderWithProviders(app(), { route: "/admin/users?user=alice" });
    await waitFor(() => expect(screen.getByTestId("where").textContent).toBe("/"));
    expect(screen.queryByText(/集群/)).toBeNull();
  });

  it("admin sees 集群 and 审计 groups", async () => {
    stubFetch({ "/api/devpods": empty });
    renderWithProviders(app(), { route: "/settings", me: stubMe({ admin: true }) });
    expect(await screen.findByText("── 集群")).toBeTruthy();
    expect(screen.getByText("── 审计")).toBeTruthy();
    expect(screen.getByRole("link", { name: /用户与配额/ })).toBeTruthy();
  });

  it("audit entries hidden when features.audit is false", async () => {
    stubFetch({ "/api/devpods": empty });
    const me = stubMe({ admin: true });
    me.features.audit = false;
    renderWithProviders(app(), { route: "/settings", me });
    expect(await screen.findByText("── 集群")).toBeTruthy();
    expect(screen.queryByText("── 审计")).toBeNull();
    expect(screen.queryByRole("link", { name: /我的会话/ })).toBeNull();
  });

  it("shows 实时数据重连中… while a stream is reconnecting", async () => {
    stubFetch({ "/api/devpods": empty, "/api/me/pubkeys": { pubkeys: [] } });
    let h: SseHandlers | undefined;
    const hub = new LiveHub({
      open: (_u, handlers) => {
        h = handlers;
        return () => {};
      },
    });
    const me = stubMe();
    me.features.telemetry = true;
    renderWithProviders(app(), { route: "/settings", me, hub });
    await screen.findByText("SSH 公钥");
    expect(screen.queryByText("实时数据重连中…")).toBeNull();
    act(() => {
      hub.acquire(devpodsStream("mine"));
      h!.onError();
    });
    expect(screen.getByText("实时数据重连中…")).toBeTruthy();
  });

  it("settings keys are read-only when pubkeySelfService is false", async () => {
    stubFetch({ "/api/me/pubkeys": { pubkeys: ["ssh-ed25519 AAAA x"] }, "/api/devpods": empty });
    const me = stubMe();
    me.features.pubkeySelfService = false;
    renderWithProviders(app(), { route: "/settings", me });
    const ta = (await screen.findByLabelText("SSH 公钥")) as HTMLTextAreaElement;
    expect(ta.readOnly).toBe(true);
    expect(screen.queryByRole("button", { name: "保存公钥" })).toBeNull();
  });
});
