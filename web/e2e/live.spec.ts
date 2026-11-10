import { expect, test } from "@playwright/test";
import { login, shot } from "./helpers";

test("a CPU cell changes within 3 s", async ({ page, context }) => {
  await login(context, "alice");
  await page.goto("/");
  const row = page.getByRole("row").filter({ has: page.getByRole("link", { name: "alice-train" }) });
  const cpu = row.locator("td").nth(1);
  await expect(cpu).toContainText("/8");
  const first = await cpu.textContent();
  await expect.poll(async () => cpu.textContent(), { timeout: 3000, intervals: [250] }).not.toBe(first);
  await shot(page, "devpods");
});
