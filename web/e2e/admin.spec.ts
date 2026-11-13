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

test("400 DevPods with every column never scroll the page sideways (the table scrolls inside)", async ({ page }) => {
  await page.goto("/admin/devpods");
  await expect(page.getByRole("link", { name: "alice-train" })).toBeVisible();
  const overflow = await page.evaluate(() => document.scrollingElement!.scrollWidth - document.scrollingElement!.clientWidth);
  expect(overflow).toBeLessThanOrEqual(0);
  await shot(page, "all-devpods");
});

test("400 DevPods: the filters and header stay in view, only the rows in sight are drawn, and the choices come back", async ({ page }) => {
  await page.goto("/admin/devpods");
  await expect(page.getByRole("link", { name: "alice-train" })).toBeVisible();
  const drawn = () => page.locator("tbody tr[data-row]").count();
  expect(await drawn()).toBeLessThan(80);

  const scroller = page.locator("[data-table-scroller]");
  await scroller.evaluate((el) => (el.scrollTop = el.scrollHeight));
  await expect(page.getByLabel("过滤")).toBeInViewport();
  await expect(page.locator("thead")).toBeInViewport();
  await expect(page.locator("tbody tr[data-row]").last()).toBeInViewport();
  expect(await drawn()).toBeLessThan(80);
  expect(await page.evaluate(() => document.querySelector("main")!.scrollTop)).toBe(0); // the page stays; the table scrolls
  await shot(page, "all-devpods-scrolled");

  await page.getByRole("button", { name: /^Running/ }).click();
  await expect(page).toHaveURL(/phase=Running/);
  await page.goto("/admin");
  await page.goto("/admin/devpods");
  await expect(page).toHaveURL(/phase=Running/);
  await shot(page, "all-devpods-remembered");
});

test("overview shows live KPIs", async ({ page }) => {
  await page.goto("/admin");
  await expect(page.getByText("GPU 已占/总数")).toBeVisible();
  await expect(page.getByText("48/96")).toBeVisible();
  await shot(page, "overview");
});
