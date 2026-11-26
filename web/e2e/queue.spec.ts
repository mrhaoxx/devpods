import { expect, test } from "@playwright/test";
import { login, shot } from "./helpers";

test.beforeEach(async ({ context }) => login(context, "alice"));

// The queue in the order it runs: the running job, then the waiting with
// why each waits; a row opens the job.
test("queue: lists the jobs, running first, each saying why it waits", async ({ page }) => {
  await page.goto("/queue");
  const table = page.getByRole("table", { name: "队列" });
  await expect(table).toContainText("run01 · sft");
  await expect(table).toContainText("空闲卡不够（要 32，剩 18）");
  await expect(table).toContainText("排在 wait1 后");
  await shot(page, "queue");
  await table.getByText("run01 · sft").click();
  await expect(page).toHaveURL(/\/queue\/-\/run01$/);
  await expect(page.getByRole("table", { name: "各台" })).toContainText("g201");
});

// Submitting: from one of my DevPods, a command, 2 machines of 4 cards; the
// form says where it would stand, then the job's page opens and the queue
// has it.
test("queue: submitting from the form adds a job", async ({ page }) => {
  await page.goto("/queue");
  await page.getByRole("button", { name: "提交任务…" }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByLabel("来自").selectOption({ index: 1 });
  await dialog.getByLabel("命令").fill("torchrun train.py");
  await dialog.getByLabel("台数").fill("2");
  await dialog.getByRole("radio", { name: "4 卡" }).click();
  await expect(dialog).toContainText(/提交前预估：排第 3 位，预计 .* 前后开始/);
  await shot(page, "queue-submit");
  await dialog.getByRole("button", { name: "提交" }).click();
  await expect(page).toHaveURL(/\/queue\/-\/new\d+$/);
  await expect(page.getByText("torchrun train.py")).toBeVisible();
  await page.getByRole("link", { name: "← 队列" }).click();
  await expect(page.getByRole("table", { name: "队列" })).toContainText("2 台 × 4 卡");
});

test.describe("on a phone", () => {
  test.use({ hasTouch: true, isMobile: true, viewport: { width: 390, height: 844 } });

  test("the queue fits: the table scrolls, the page does not grow sideways", async ({ page }) => {
    await page.goto("/queue");
    const table = page.getByRole("table", { name: "队列" });
    await expect(table).toContainText("run01");
    // the table scrolls sideways inside its own scroller
    const scroll = await table.evaluate((el) => {
      let s: HTMLElement | null = el.parentElement;
      while (s && !/(auto|scroll)/.test(getComputedStyle(s).overflowX)) s = s.parentElement;
      return s ? s.scrollWidth - s.clientWidth : 0;
    });
    expect(scroll).toBeGreaterThan(0);
    expect(await page.evaluate(() => document.querySelector("main")!.scrollWidth - document.querySelector("main")!.clientWidth)).toBeLessThanOrEqual(0);
    expect(await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)).toBeLessThanOrEqual(0);
    await shot(page, "queue-phone");
  });
});
