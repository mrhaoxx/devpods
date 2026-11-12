import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { renderWithProviders, stubFetch } from "../../test/render";
import { ClustersPage } from "./ClustersPage";

const mulan = {
  name: "mulan",
  displayName: "木兰",
  connected: true,
  joined: true,
  lastSeen: new Date().toISOString(),
  capabilities: ["snapshots", "reservations"],
  agentVersion: "v0.7.0",
  controllerVersion: "v0.7.0",
  kubernetesVersion: "v1.36.4",
  createdAt: new Date().toISOString(),
};
const fresh = { name: "st2", connected: false, joined: false, tokenExpires: new Date(Date.now() + 3600e3).toISOString(), createdAt: new Date().toISOString() };
const paused = { ...mulan, name: "old", displayName: "", connected: false, paused: true };

const row = (name: string) => screen.getByText(name).closest("tr") as HTMLElement;

describe("集群 page", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("shows each cluster's state, versions and what it offers", async () => {
    stubFetch({ "GET /api/admin/clusters": { items: [mulan, fresh, paused] } });
    renderWithProviders(<ClustersPage />);
    await screen.findByText("mulan");
    const m = within(row("mulan"));
    expect(m.getByText("木兰")).toBeTruthy();
    expect(m.getByText("已连接")).toBeTruthy();
    expect(m.getByText(/v0\.7\.0/)).toBeTruthy();
    expect(m.getByText("snapshots")).toBeTruthy();
    expect(within(row("st2")).getByText("等待加入")).toBeTruthy();
    expect(within(row("old")).getByText("已暂停")).toBeTruthy();
  });

  it("registering a cluster shows the join command, once", async () => {
    const calls = stubFetch({
      "GET /api/admin/clusters": { items: [mulan] },
      "POST /api/admin/clusters": { cluster: fresh, joinToken: "dpj_secret", command: "helm upgrade --install devpod … --set agent.joinToken=dpj_secret" },
    });
    renderWithProviders(<ClustersPage />);
    fireEvent.click(await screen.findByRole("button", { name: "新建集群" }));
    const dlg = within(await screen.findByRole("dialog"));
    fireEvent.change(dlg.getByLabelText("名称"), { target: { value: "st2" } });
    fireEvent.change(dlg.getByLabelText("显示名"), { target: { value: "ST2" } });
    fireEvent.click(dlg.getByRole("button", { name: "创建" }));
    expect(await screen.findByText(/agent\.joinToken=dpj_secret/)).toBeTruthy();
    expect(screen.getByText(/只显示这一次/)).toBeTruthy();
    const post = calls.find((c) => c.method === "POST");
    expect(JSON.parse(post!.body!)).toEqual({ name: "st2", displayName: "ST2" });
  });

  it("a bad name is said before anything is sent", async () => {
    const calls = stubFetch({ "GET /api/admin/clusters": { items: [] } });
    renderWithProviders(<ClustersPage />);
    fireEvent.click(await screen.findByRole("button", { name: "新建集群" }));
    const dlg = within(await screen.findByRole("dialog"));
    fireEvent.change(dlg.getByLabelText("名称"), { target: { value: "devpod" } });
    expect((dlg.getByRole("button", { name: "创建" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(dlg.getByLabelText("名称"), { target: { value: "Bad_Name" } });
    expect((dlg.getByRole("button", { name: "创建" }) as HTMLButtonElement).disabled).toBe(true);
    expect(calls.filter((c) => c.method === "POST").length).toBe(0);
  });

  it("pausing and removing ask first", async () => {
    const calls = stubFetch({
      "GET /api/admin/clusters": { items: [mulan] },
      "PATCH /api/admin/clusters/mulan": { ...mulan, paused: true },
      "DELETE /api/admin/clusters/mulan": new Response(null, { status: 204 }),
    });
    renderWithProviders(<ClustersPage />);
    await screen.findByText("mulan");
    fireEvent.click(within(row("mulan")).getByRole("button", { name: "暂停" }));
    expect(calls.some((c) => c.method === "PATCH")).toBe(false);
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "暂停" }));
    await waitFor(() => expect(calls.some((c) => c.method === "PATCH" && c.body === JSON.stringify({ paused: true }))).toBe(true));

    fireEvent.click(within(row("mulan")).getByRole("button", { name: "移除" }));
    const dlg = within(await screen.findByRole("dialog"));
    const confirm = dlg.getByRole("button", { name: "移除" }) as HTMLButtonElement;
    expect(confirm.disabled).toBe(true); // the name has to be typed
    fireEvent.change(dlg.getByLabelText("输入 mulan 以确认"), { target: { value: "mulan" } });
    fireEvent.click(confirm);
    await waitFor(() => expect(calls.some((c) => c.method === "DELETE")).toBe(true));
  });

  it("a new token is shown once and warns that the agent is withdrawn", async () => {
    stubFetch({
      "GET /api/admin/clusters": { items: [mulan] },
      "POST /api/admin/clusters/mulan/token": { cluster: { ...mulan, joined: false }, joinToken: "dpj_new", command: "helm … dpj_new" },
    });
    renderWithProviders(<ClustersPage />);
    await screen.findByText("mulan");
    fireEvent.click(within(row("mulan")).getByRole("button", { name: "重新签发" }));
    const dlg = within(await screen.findByRole("dialog"));
    expect(dlg.getByText(/现有 agent 将不再被接受/)).toBeTruthy();
    fireEvent.click(dlg.getByRole("button", { name: "重新签发" }));
    expect(await screen.findByText(/dpj_new/)).toBeTruthy();
  });
});
