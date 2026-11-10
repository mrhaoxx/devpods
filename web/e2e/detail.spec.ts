import { expect, test } from "@playwright/test";
import { login, shot } from "./helpers";

test("detail tabs follow the URL", async ({ page, context }) => {
  await login(context, "alice");
  await page.goto("/devpods/alice-train");
  await expect(page.getByRole("heading", { name: "alice-train" })).toBeVisible();
  await expect(page.getByTestId("card-cpu")).toContainText("/8");
  await shot(page, "detail");
  await page.getByRole("tab", { name: "监控" }).click();
  await expect(page).toHaveURL(/tab=monitor/);
  await page.getByRole("radio", { name: "1h" }).click();
  await expect(page).toHaveURL(/range=1h/);
  await expect(page.locator(".uplot").first()).toBeVisible();
  await shot(page, "monitor");
  await page.getByRole("tab", { name: "进程" }).click();
  await expect(page).toHaveURL(/tab=processes/);
  await expect(page.getByText("python train.py --worker 30")).toBeVisible();
});
