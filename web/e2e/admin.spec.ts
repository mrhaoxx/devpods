import { expect, test } from "@playwright/test";
import { login, shot } from "./helpers";

test.beforeEach(async ({ context }) => login(context, "admin"));

test("batch 设配额 for two users reports two successes", async ({ page }) => {
  await page.goto("/admin/users");
  await page.getByLabel("选择 alice").check();
  await page.getByLabel("选择 user01").check();
  await expect(page.getByText("已选 2")).toBeVisible();
  await shot(page, "users");
  await page.getByRole("button", { name: "设配额" }).click();
  await page.getByLabel("cpu", { exact: true }).fill("4");
  await page.getByRole("button", { name: "应用配额" }).click();
  await expect(page.getByText("成功 2，失败 0")).toBeVisible();
});

test("400 DevPods with every column never scroll the page sideways", async ({ page }) => {
  await page.goto("/admin/devpods");
  await expect(page.getByRole("link", { name: "alice-train" })).toBeVisible();
  const showAll = page.getByRole("button", { name: /显示全部列/ });
  if (await showAll.isVisible()) await showAll.click();
  const overflow = await page.evaluate(() => document.scrollingElement!.scrollWidth - document.scrollingElement!.clientWidth);
  expect(overflow).toBeLessThanOrEqual(0);
  await shot(page, "all-devpods");
});

test("overview shows live KPIs", async ({ page }) => {
  await page.goto("/admin");
  await expect(page.getByText("GPU 已占/总数")).toBeVisible();
  await expect(page.getByText("48/96")).toBeVisible();
  await shot(page, "overview");
});
