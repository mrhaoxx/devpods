import { expect, test } from "@playwright/test";
import { login, shot } from "./helpers";

test.beforeEach(async ({ context }) => login(context, "alice"));

// The timeline holds the whole bookable window (15 days here); 3/7/14 say
// how many days fit one screen, and it scrolls sideways beyond.
test("reservations: the timeline zooms and scrolls sideways, opens near now, the page does not grow", async ({ page }) => {
  await page.goto("/reservations");
  await expect(page.getByRole("group", { name: "g200 的预约" })).toBeVisible();
  const scroller = page.locator("[data-timeline]");
  const at3 = await scroller.evaluate((el) => ({ sw: el.scrollWidth, cw: el.clientWidth, sl: el.scrollLeft }));
  expect(at3.sw).toBeGreaterThan(at3.cw * 4);
  void at3.sl;
  // opened near now, whatever the hour: the now line is in the visible part
  const view = (await scroller.boundingBox())!;
  const nowLine = (await page.locator("[data-track] .bg-fail.w-px").first().boundingBox())!;
  expect(nowLine.x).toBeGreaterThan(view.x);
  expect(nowLine.x).toBeLessThan(view.x + view.width);
  expect(await page.evaluate(() => document.querySelector("main")!.scrollWidth - document.querySelector("main")!.clientWidth)).toBeLessThanOrEqual(0);
  await shot(page, "reservations-3d");
  await page.getByRole("radio", { name: "14 天" }).click();
  await expect.poll(() => scroller.evaluate((el) => el.scrollWidth / el.clientWidth)).toBeLessThan(1.3);
  await shot(page, "reservations-14d");
});

// A reservation of part of a machine is drawn at its share; a refused one
// says what is free and from when.
test("reservations: a share is half the GPU band; a refusal says what is free", async ({ page }) => {
  await page.goto("/reservations");
  const track = page.getByRole("group", { name: "g201 的预约" });
  await expect(track).toBeVisible();
  const band = (await track.locator('[data-band="gpu"]').boundingBox())!;
  const block = (await track.locator('[data-band="gpu"] [data-res="rc"]').boundingBox())!;
  expect(Math.abs(block.height - band.height / 2)).toBeLessThan(2);
  await page.getByRole("button", { name: "预约节点" }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByRole("button", { name: "清空" }).click();
  await dialog.getByLabel("g205").check();
  await dialog.getByRole("radio", { name: "4 卡" }).click();
  await dialog.getByRole("button", { name: "预约" }).click();
  await expect(dialog).toContainText(/g205：这段时间最多还能约 2 卡 · 32 核；最早 .* 起有空/);
  await shot(page, "reservations-no-room");
});

test.describe("on a phone", () => {
  test.use({ hasTouch: true, isMobile: true, viewport: { width: 390, height: 844 } });

  test("a tap on the timeline opens the form there; another's reservation opens read-only", async ({ page }) => {
    await page.goto("/reservations");
    const track = page.getByRole("group", { name: "g201 的预约" });
    await expect(track).toBeVisible();
    const box = (await page.locator("[data-timeline]").boundingBox())!;
    const row = (await track.boundingBox())!;
    await page.touchscreen.tap(box.x + box.width - 30, row.y + row.height / 2);
    const dialog = page.getByRole("dialog");
    await expect(dialog).toBeVisible();
    await expect(dialog.getByLabel("g201")).toBeChecked();
    await shot(page, "reservations-phone-form");
    await dialog.getByRole("button", { name: "取消" }).click();
    await page.getByRole("group", { name: "g200 的预约" }).getByRole("button", { name: /bob/ }).tap();
    await expect(page.getByRole("dialog")).toContainText("跑基准");
    await expect(page.getByRole("dialog").getByRole("button", { name: /修改|取消预约/ })).toHaveCount(0);
    await shot(page, "reservations-phone-details");
  });
});
