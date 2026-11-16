import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen } from "@testing-library/react";
import { LoginPage } from "./LoginPage";
import { renderWithProviders, stubFetch } from "../test/render";

const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

describe("LoginPage", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("after too many failed sign-ins, says how long to wait", async () => {
    stubFetch({
      "/api/auth/config": { password: true, oauth: false },
      "POST /api/auth/password": () => json(429, { code: "TOO_MANY_ATTEMPTS", message: "too many failed sign-ins: try again in 125 s", detail: { retryAfter: 125 } }),
    });
    renderWithProviders(<LoginPage />);
    fireEvent.change(await screen.findByLabelText("用户名"), { target: { value: "alice" } });
    fireEvent.change(screen.getByLabelText("密码"), { target: { value: "x" } });
    fireEvent.click(screen.getByRole("button", { name: "登录" }));
    expect(await screen.findByText("登录失败次数过多，请 3 分钟后再试")).toBeTruthy();
  });

  it("a wrong password is said plainly", async () => {
    stubFetch({
      "/api/auth/config": { password: true, oauth: false },
      "POST /api/auth/password": () => json(401, { code: "UNAUTHORIZED", message: "invalid username or password" }),
    });
    renderWithProviders(<LoginPage />);
    fireEvent.change(await screen.findByLabelText("用户名"), { target: { value: "alice" } });
    fireEvent.change(screen.getByLabelText("密码"), { target: { value: "x" } });
    fireEvent.click(screen.getByRole("button", { name: "登录" }));
    expect(await screen.findByText("用户名或密码不对")).toBeTruthy();
  });
});
