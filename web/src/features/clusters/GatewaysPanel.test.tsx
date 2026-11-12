import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { renderWithProviders, stubFetch } from "../../test/render";
import { GatewaysPanel } from "./GatewaysPanel";

const hub = {
  name: "b300",
  displayName: "B300",
  sshAddress: "ssh.example.com:2222",
  httpBaseDomain: "apps.example.com",
  agentAddress: "https://gw.example.com",
  joined: true,
  lastSeen: new Date().toISOString(),
  version: "v0.8.0",
  clusters: ["mulan", "st2"],
  createdAt: new Date().toISOString(),
};
const fresh = { name: "mulan-gw", joined: false, createdAt: new Date().toISOString() };
const row = (name: string) => screen.getByText(name).closest("tr") as HTMLElement;

describe("网关 panel", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("shows where each gateway is, whether it is there, and which clusters reach it", async () => {
    stubFetch({ "GET /api/admin/gateways": { items: [hub, fresh] } });
    renderWithProviders(<GatewaysPanel />);
    await screen.findByText("b300");
    const r = within(row("b300"));
    expect(r.getByText("ssh.example.com:2222")).toBeTruthy();
    expect(r.getByText("apps.example.com")).toBeTruthy();
    expect(r.getByText("在线")).toBeTruthy();
    expect(r.getByText("mulan")).toBeTruthy();
    expect(r.getByText("st2")).toBeTruthy();
    expect(within(row("mulan-gw")).getByText("等待加入")).toBeTruthy();
  });

  it("registering a gateway sends where it is and shows the join command once", async () => {
    const calls = stubFetch({
      "GET /api/admin/gateways": { items: [] },
      "POST /api/admin/gateways": { gateway: fresh, joinToken: "dpj_gw", command: "helm … --set gateway.hub.joinToken=dpj_gw" },
    });
    renderWithProviders(<GatewaysPanel />);
    fireEvent.click(await screen.findByRole("button", { name: "新建网关" }));
    const dlg = within(await screen.findByRole("dialog"));
    fireEvent.change(dlg.getByLabelText("名称"), { target: { value: "mulan-gw" } });
    fireEvent.change(dlg.getByLabelText("SSH 地址"), { target: { value: "ssh-mulan.example.com" } });
    fireEvent.change(dlg.getByLabelText("端口链接域名"), { target: { value: "apps-mulan.example.com" } });
    fireEvent.change(dlg.getByLabelText("agent 连接地址"), { target: { value: "https://gw-mulan.example.com" } });
    fireEvent.click(dlg.getByRole("button", { name: "创建" }));
    expect(await screen.findByText(/gateway\.hub\.joinToken=dpj_gw/)).toBeTruthy();
    expect(screen.getByText(/只显示这一次/)).toBeTruthy();
    expect(JSON.parse(calls.find((c) => c.method === "POST")!.body!)).toEqual({
      name: "mulan-gw",
      sshAddress: "ssh-mulan.example.com",
      httpBaseDomain: "apps-mulan.example.com",
      agentAddress: "https://gw-mulan.example.com",
    });
  });

  it("removing asks for the name first", async () => {
    const calls = stubFetch({
      "GET /api/admin/gateways": { items: [hub] },
      "DELETE /api/admin/gateways/b300": new Response(null, { status: 204 }),
    });
    renderWithProviders(<GatewaysPanel />);
    await screen.findByText("b300");
    fireEvent.click(within(row("b300")).getByRole("button", { name: "移除" }));
    const dlg = within(await screen.findByRole("dialog"));
    const confirm = dlg.getByRole("button", { name: "移除" }) as HTMLButtonElement;
    expect(confirm.disabled).toBe(true);
    fireEvent.change(dlg.getByLabelText("输入 b300 以确认"), { target: { value: "b300" } });
    fireEvent.click(confirm);
    await waitFor(() => expect(calls.some((c) => c.method === "DELETE")).toBe(true));
  });
});
