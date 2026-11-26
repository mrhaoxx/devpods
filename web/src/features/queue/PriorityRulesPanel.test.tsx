import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { PriorityRulesPanel } from "./PriorityRulesPanel";
import { renderWithProviders, stubFetch } from "../../test/render";

describe("PriorityRulesPanel", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("loads the rules, edits one, adds one and saves the list", async () => {
    const calls = stubFetch({
      "GET /api/admin/job-priority-rules": { items: [{ condition: 'group = ml', maxPriority: 100 }] },
      "PUT /api/admin/job-priority-rules": (_u: URL, init?: RequestInit) => JSON.parse(String(init?.body)),
    });
    renderWithProviders(<PriorityRulesPanel />);
    const max = await screen.findByLabelText("规则 1 最高优先级");
    fireEvent.change(max, { target: { value: "200" } });
    fireEvent.click(screen.getByRole("button", { name: "添加规则" }));
    fireEvent.change(screen.getByLabelText("规则 2 条件"), { target: { value: 'user = boss' } });
    fireEvent.change(screen.getByLabelText("规则 2 最高优先级"), { target: { value: "1000" } });
    fireEvent.click(screen.getByRole("button", { name: "保存" }));
    await waitFor(() => expect(calls.some((c) => c.method === "PUT")).toBe(true));
    expect(JSON.parse(calls.find((c) => c.method === "PUT")!.body!)).toEqual({
      items: [
        { condition: 'group = ml', maxPriority: 200 },
        { condition: 'user = boss', maxPriority: 1000 },
      ],
    });
  });

  it("a broken condition shows what the server said", async () => {
    stubFetch({
      "GET /api/admin/job-priority-rules": { items: [{ condition: "user =", maxPriority: 1 }] },
      "PUT /api/admin/job-priority-rules": new Response(JSON.stringify({ code: "BAD_CONDITION", message: "rule 1: syntax error", detail: { index: 0 } }), { status: 400 }),
    });
    renderWithProviders(<PriorityRulesPanel />);
    fireEvent.change(await screen.findByLabelText("规则 1 最高优先级"), { target: { value: "2" } });
    fireEvent.click(screen.getByRole("button", { name: "保存" }));
    expect(await screen.findByText(/rule 1: syntax error/)).toBeTruthy();
  });
});
