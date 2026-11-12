import { expect, test, type Page } from "@playwright/test";
import { login, shot } from "./helpers";

/** the editor of the YAML tab, once Monaco runs in it */
const monacoEditor = (page: Page) => page.getByTestId("yaml-monaco").locator(".monaco-editor");

test("the YAML editor does not swap a text box for Monaco: one box, the same size, from the start", async ({ page, context }) => {
  await login(context, "alice");
  // Monaco arrives late: what is shown meanwhile must already be the editor's box
  let release!: () => void;
  const held = new Promise<void>((r) => (release = r));
  await page.route(/\/assets\/monaco-[^/]*\.js$/, async (route) => {
    await held;
    await route.continue();
  });
  await page.goto("/devpods/alice-train?tab=yaml");
  const box = page.getByTestId("yaml-monaco");
  await expect(box).toBeVisible();
  await expect(box).toContainText("owner: alice");
  // no plain text box in the meantime
  await expect(page.getByTestId("yaml-plain")).toHaveCount(0);
  const before = await box.boundingBox();
  await shot(page, "yaml-loading");
  release();
  await expect(monacoEditor(page)).toBeVisible();
  const after = await box.boundingBox();
  expect(after?.height).toBe(before?.height);
  expect(after?.y).toBe(before?.y);
  await expect(page.getByTestId("yaml-plain")).toHaveCount(0);
});

test("the YAML editor knows what a DevPod is: completion, hover, unknown fields", async ({ page, context }) => {
  await login(context, "alice");
  await page.goto("/devpods/alice-train?tab=yaml");
  await expect(monacoEditor(page)).toBeVisible();
  const lines = page.getByTestId("yaml-monaco").locator(".view-line");

  // hover over a field: its description from the DevPod schema
  await lines.filter({ hasText: "owner: alice" }).getByText("owner").hover();
  await expect(page.locator(".monaco-hover")).toContainText(/owner/i);
  await expect(page.locator(".monaco-hover")).toContainText(/user/i);
  await shot(page, "yaml-hover");

  // completion: the fields of spec
  await lines.filter({ hasText: "running: true" }).click();
  await page.keyboard.press("End");
  await page.keyboard.press("Enter");
  await page.keyboard.press("Control+Space");
  const suggest = page.locator(".suggest-widget");
  await expect(suggest).toBeVisible();
  await expect(suggest).toContainText("collaborators");
  await expect(suggest).toContainText("persistence");
  await shot(page, "yaml-complete");
  await page.keyboard.press("Escape");

  // a misspelt field (the API server would drop it without a word) is pointed out
  await page.keyboard.type("runnning: false");
  const note = page.getByLabel("YAML 字段");
  await expect(note).toBeVisible({ timeout: 10_000 });
  await expect(note).toContainText("runnning");
  await expect(page.getByTestId("yaml-monaco").locator(".squiggly-warning, .squiggly-error").first()).toBeVisible();
  await shot(page, "yaml-unknown-field");
});
