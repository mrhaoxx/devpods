import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { YamlTab } from "./YamlTab";
import { renderWithProviders, stubFetch } from "../../../test/render";

const text = "apiVersion: devpod.io/v1alpha1\nkind: DevPod\nmetadata:\n  name: alice-x\nspec:\n  owner: alice\n  running: true\n";
const editor = () => screen.getByLabelText("DevPod YAML") as HTMLTextAreaElement;
const button = (name: string) => screen.getByRole("button", { name }) as HTMLButtonElement;

describe("YamlTab", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("shows the manifest; nothing to save until it is edited; 还原 puts it back", async () => {
    stubFetch({ "/api/devpods/alice-x/yaml": { yaml: text, running: true } });
    renderWithProviders(<YamlTab r={{ name: "alice-x" }} />);
    await waitFor(() => expect(editor().value).toBe(text));
    expect(button("保存").disabled).toBe(true);
    expect(button("还原").disabled).toBe(true);
    fireEvent.change(editor(), { target: { value: text + "  shell: zsh\n" } });
    expect(button("保存").disabled).toBe(false);
    // a running DevPod: say what a change to its pod definition does
    expect(screen.getByText(/会重建 Pod 并断开所有会话/)).toBeTruthy();
    fireEvent.click(button("还原"));
    expect(editor().value).toBe(text);
    expect(button("保存").disabled).toBe(true);
  });

  it("saving sends the text; a rebuild is said; the saved text becomes the base", async () => {
    const edited = text + "  shell: zsh\n";
    const calls = stubFetch({
      "PUT /api/devpods/alice-x/yaml": { devpod: { metadata: { name: "alice-x" }, spec: { owner: "alice", running: true } }, rebuild: true },
      "/api/devpods/alice-x/yaml": { yaml: text, running: true },
    });
    renderWithProviders(<YamlTab r={{ name: "alice-x" }} />);
    await waitFor(() => expect(editor().value).toBe(text));
    fireEvent.change(editor(), { target: { value: edited } });
    fireEvent.click(button("保存"));
    await waitFor(() => expect(calls.find((c) => c.method === "PUT")?.body).toBe(JSON.stringify({ yaml: edited })));
    expect((await screen.findByRole("status")).textContent).toMatch(/已保存.*Pod 正在重建/);
    expect(button("保存").disabled).toBe(true);
  });

  it("text that is not YAML cannot be saved, and the editor says where it breaks", async () => {
    stubFetch({ "/api/devpods/alice-x/yaml": { yaml: text, running: false } });
    renderWithProviders(<YamlTab r={{ name: "alice-x" }} />);
    await waitFor(() => expect(editor().value).toBe(text));
    fireEvent.change(editor(), { target: { value: text + "  pod: [unclosed\n" } });
    expect(button("保存").disabled).toBe(true);
    const note = await screen.findByRole("alert", { name: "YAML 语法" });
    expect(note.textContent).toMatch(/第 8 行/);
    fireEvent.change(editor(), { target: { value: text + "  pod: []\n" } });
    expect(button("保存").disabled).toBe(false);
  });

  it("a refusal keeps the text and shows the server's reason", async () => {
    stubFetch({
      "PUT /api/devpods/alice-x/yaml": new Response(JSON.stringify({ code: "BAD_REQUEST", message: "update rejected: spec.pod.spec.containers must contain at least one container" }), { status: 400 }),
      "/api/devpods/alice-x/yaml": { yaml: text, running: false },
    });
    renderWithProviders(<YamlTab r={{ name: "alice-x" }} />);
    await waitFor(() => expect(editor().value).toBe(text));
    const broken = text + "  pod: {spec: {containers: []}}\n";
    fireEvent.change(editor(), { target: { value: broken } });
    fireEvent.click(button("保存"));
    expect((await screen.findByText(/保存失败/)).textContent).toContain("must contain at least one container");
    expect(editor().value).toBe(broken);
    expect(button("保存").disabled).toBe(false);
    // stopped: no rebuild warning
    expect(screen.queryByText(/会重建 Pod/)).toBeNull();
  });
});
