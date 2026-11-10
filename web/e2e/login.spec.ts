import { expect, test } from "@playwright/test";
import { shot } from "./helpers";

test("password login lands on 我的 DevPod", async ({ page }) => {
  await page.goto("/login");
  await page.getByLabel("用户名").fill("alice");
  await page.getByLabel("密码").fill("pw");
  await shot(page, "login");
  await page.getByRole("button", { name: "登录" }).click();
  await expect(page).toHaveURL("/");
  await expect(page.getByRole("link", { name: "alice-train" })).toBeVisible();
});
