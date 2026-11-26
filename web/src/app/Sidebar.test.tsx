import { afterEach, describe, expect, it, vi } from "vitest";
import { screen } from "@testing-library/react";
import { Sidebar } from "./Sidebar";
import { renderWithProviders, stubFetch, stubMe } from "../test/render";

describe("Sidebar", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("a non-admin with grants sees what they grant", async () => {
    stubFetch({ "/api/devpods": { items: [] } });
    const me = stubMe({ permissions: { viewAudit: true, setReservable: true, disconnectSessions: false } });
    renderWithProviders(<Sidebar me={me} />, { me });
    expect(await screen.findByRole("link", { name: /会话 \/ 回放/ })).toBeTruthy();
    expect(screen.getByRole("link", { name: /告警/ })).toBeTruthy();
    expect(screen.queryByRole("link", { name: /访问记录/ })).toBeNull();
    expect(screen.getByRole("link", { name: /我管理的机器/ })).toBeTruthy();
    expect(screen.queryByRole("link", { name: /全部 DevPod/ })).toBeNull();
  });

  it("without grants, none of it", async () => {
    stubFetch({ "/api/devpods": { items: [] } });
    const me = stubMe();
    renderWithProviders(<Sidebar me={me} />, { me });
    await screen.findAllByRole("link");
    expect(screen.queryByRole("link", { name: /会话 \/ 回放/ })).toBeNull();
    expect(screen.queryByRole("link", { name: /我管理的机器/ })).toBeNull();
  });

  it("队列 only where the queue is on", async () => {
    stubFetch({});
    const off = stubMe();
    const { unmount } = renderWithProviders(<Sidebar me={off} />, { me: off });
    expect(screen.queryByRole("link", { name: /队列/ })).toBeNull();
    unmount();
    const on = stubMe({ features: { ...stubMe().features, queue: true } });
    renderWithProviders(<Sidebar me={on} />, { me: on });
    expect(screen.getByRole("link", { name: /队列/ })).toBeTruthy();
  });
});
