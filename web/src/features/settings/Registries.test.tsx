import { afterEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import { Registries } from "./Registries";
import { renderWithProviders, stubFetch } from "../../test/render";

describe("Registries", () => {
  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it("adds a credential: the pasted address is tidied, the password is sent once and cleared", async () => {
    let items = [] as { host: string; username: string; pending: boolean }[];
    const calls = stubFetch({
      "/api/me/registries": () => ({ items }),
      "PUT /api/me/registries/harbor.example.com": () => {
        items = [{ host: "harbor.example.com", username: "robot", pending: false }];
        return new Response(null, { status: 204 });
      },
    });
    renderWithProviders(<Registries />);
    expect(await screen.findByText("还没有保存任何仓库凭证。")).toBeTruthy();
    const add = screen.getByRole("button", { name: "添加凭证" }) as HTMLButtonElement;
    expect(add.disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("仓库地址"), { target: { value: "https://Harbor.Example.com/harbor/projects" } });
    fireEvent.change(screen.getByLabelText("仓库用户名"), { target: { value: "robot" } });
    const pw = screen.getByLabelText("仓库密码") as HTMLInputElement;
    expect(pw.type).toBe("password");
    expect(add.disabled).toBe(true); // no password yet
    fireEvent.change(pw, { target: { value: "s3cret" } });
    fireEvent.click(add);
    await waitFor(() => expect(calls.find((c) => c.method === "PUT")).toBeTruthy());
    const put = calls.find((c) => c.method === "PUT")!;
    expect(put.url.pathname).toBe("/api/me/registries/harbor.example.com");
    expect(put.body).toBe('{"username":"robot","password":"s3cret"}');
    const table = await screen.findByRole("table", { name: "已保存的仓库" });
    expect(within(table).getByText("harbor.example.com")).toBeTruthy();
    expect(within(table).getByText("robot")).toBeTruthy();
    await waitFor(() => expect((screen.getByLabelText("仓库密码") as HTMLInputElement).value).toBe(""));
    expect((screen.getByLabelText("仓库地址") as HTMLInputElement).value).toBe("");
    expect(document.body.textContent).not.toContain("s3cret");
  });

  it("a host already saved is replaced, and says so; a bad address is refused in the form", async () => {
    stubFetch({ "/api/me/registries": { items: [{ host: "reg.test:5000", username: "old", pending: false }] } });
    renderWithProviders(<Registries />);
    await screen.findByText("reg.test:5000");
    fireEvent.change(screen.getByLabelText("仓库地址"), { target: { value: "reg.test:5000" } });
    expect(screen.getByText(/再次添加会替换它/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "替换凭证" })).toBeTruthy();
    fireEvent.change(screen.getByLabelText("仓库地址"), { target: { value: "not a host" } });
    expect(screen.getByRole("alert").textContent).toMatch(/形如/);
    fireEvent.change(screen.getByLabelText("仓库用户名"), { target: { value: "u" } });
    fireEvent.change(screen.getByLabelText("仓库密码"), { target: { value: "p" } });
    expect((screen.getByRole("button", { name: "添加凭证" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("delete asks first", async () => {
    const calls = stubFetch({
      "/api/me/registries": { items: [{ host: "harbor.example.com", username: "robot", pending: false }] },
      "DELETE /api/me/registries/harbor.example.com": new Response(null, { status: 204 }),
    });
    renderWithProviders(<Registries />);
    fireEvent.click(await screen.findByRole("button", { name: "删除 harbor.example.com" }));
    expect(calls.some((c) => c.method === "DELETE")).toBe(false);
    fireEvent.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "删除" }));
    await waitFor(() => expect(calls.some((c) => c.method === "DELETE" && c.url.pathname === "/api/me/registries/harbor.example.com")).toBe(true));
  });

  it("a pending credential is marked and polled until it has landed, then no more", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let pending = true;
    const calls = stubFetch({ "/api/me/registries": () => ({ items: [{ host: "harbor.example.com", username: "robot", pending }] }) });
    const gets = () => calls.filter((c) => c.method === "GET").length;
    renderWithProviders(<Registries />);
    expect(await screen.findByText("待生效")).toBeTruthy();
    expect(gets()).toBe(1);
    await act(() => vi.advanceTimersByTimeAsync(2100));
    expect(gets()).toBe(2);
    pending = false;
    await act(() => vi.advanceTimersByTimeAsync(2100));
    expect(gets()).toBe(3);
    await waitFor(() => expect(screen.queryByText("待生效")).toBeNull());
    await act(() => vi.advanceTimersByTimeAsync(6500));
    expect(gets()).toBe(3); // landed: polling stopped
  });

  it("a failed save shows the server's message and keeps the form", async () => {
    stubFetch({
      "/api/me/registries": { items: [] },
      "PUT /api/me/registries/reg.test": new Response(JSON.stringify({ code: "BAD_REQUEST", message: "at most 20 registries" }), { status: 400 }),
    });
    renderWithProviders(<Registries />);
    await screen.findByText("还没有保存任何仓库凭证。");
    fireEvent.change(screen.getByLabelText("仓库地址"), { target: { value: "reg.test" } });
    fireEvent.change(screen.getByLabelText("仓库用户名"), { target: { value: "u" } });
    fireEvent.change(screen.getByLabelText("仓库密码"), { target: { value: "p" } });
    fireEvent.click(screen.getByRole("button", { name: "添加凭证" }));
    expect(await screen.findByText(/at most 20 registries/)).toBeTruthy();
    expect((screen.getByLabelText("仓库地址") as HTMLInputElement).value).toBe("reg.test");
  });
});
