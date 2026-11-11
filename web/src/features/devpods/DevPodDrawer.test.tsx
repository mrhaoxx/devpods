import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { useLocation } from "react-router-dom";
import { MyDevPodsPage } from "./MyDevPodsPage";
import { DevPodDrawer } from "./DevPodDrawer";
import { renderWithProviders, stubFetch } from "../../test/render";
import type { DevPod } from "../../data/api";

function Where() {
  const l = useLocation();
  return <div data-testid="where">{l.pathname + l.search}</div>;
}

const running: DevPod = {
  metadata: { name: "alice-x" },
  spec: { owner: "alice", running: true, pod: { spec: { containers: [{ name: "dev", image: "ubuntu:24.04", resources: { limits: { cpu: "2", memory: "4Gi" } } }] } } },
  status: { phase: "Running" },
};

describe("DevPodDrawer", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("opens from ?new=1 and clearing it closes", async () => {
    stubFetch({ "/api/devpods": { items: [], pods: {} }, "/api/templates": { items: [] } });
    renderWithProviders(
      <>
        <MyDevPodsPage />
        <Where />
      </>,
      { route: "/?new=1" },
    );
    expect(await screen.findByRole("dialog", { name: "新建 DevPod" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "关闭" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(screen.getByTestId("where").textContent).toBe("/");
  });

  it("edit of image on a running DevPod shows the rebuild warning", async () => {
    stubFetch({ "/api/templates": { items: [] } });
    renderWithProviders(<DevPodDrawer mode="edit" devpod={running} open onClose={() => {}} />);
    expect(screen.queryByText(/会重建 Pod 并断开所有会话/)).toBeNull();
    fireEvent.change(screen.getByLabelText("镜像"), { target: { value: "ubuntu:25.04" } });
    expect(screen.getByText(/会重建 Pod 并断开所有会话/)).toBeTruthy();
  });

  it("server QUOTA_EXCEEDED shows next to the field", async () => {
    stubFetch({
      "/api/templates": { items: [] },
      "POST /api/devpods": new Response(
        JSON.stringify({ code: "QUOTA_EXCEEDED", message: "quota exceeded", detail: [{ resource: "cpu", requested: "4", used: "6", limit: "8" }] }),
        { status: 409 },
      ),
    });
    renderWithProviders(<DevPodDrawer mode="create" open onClose={() => {}} />);
    fireEvent.change(screen.getByLabelText("名称"), { target: { value: "big" } });
    fireEvent.click(screen.getByRole("button", { name: "创建" }));
    const alert = await screen.findByText(/已用 6，上限 8/);
    expect(alert.closest("label")?.textContent).toContain("CPU");
  });

  it("the YAML editor loads a saved config and hands its text over to be saved as one", async () => {
    stubFetch({
      "/api/templates": { items: [] },
      "GET /api/configs": { items: [{ name: "bob-sg", owner: "bob", title: "SGLang", description: "", sharedWith: [], public: true, variables: [], mine: false, updatedAt: "2026-11-11T00:00:00Z" }] },
      "GET /api/configs/bob-sg": { name: "bob-sg", title: "SGLang", content: "kind: DevPod\nmetadata:\n  name: ${NAME}\n" },
    });
    renderWithProviders(
      <>
        <DevPodDrawer mode="create" open onClose={() => {}} />
        <Where />
      </>,
    );
    expect(screen.queryByRole("button", { name: "保存为配置" })).toBeNull(); // only with the YAML editor
    fireEvent.click(screen.getByLabelText("使用 YAML"));
    expect((screen.getByRole("button", { name: "保存为配置" }) as HTMLButtonElement).disabled).toBe(true); // nothing to save yet
    fireEvent.change(await screen.findByLabelText("从配置载入"), { target: { value: "bob-sg" } });
    await waitFor(() => expect((screen.getByLabelText("DevPod YAML") as HTMLTextAreaElement).value).toContain("name: ${NAME}"));
    fireEvent.click(screen.getByRole("button", { name: "保存为配置" }));
    await waitFor(() => expect(screen.getByTestId("where").textContent).toBe("/configs/new"));
  });
});
