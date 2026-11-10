import type { BrowserContext, Page } from "@playwright/test";

/** login sets the mock's session cookie directly. */
export async function login(context: BrowserContext, user: "alice" | "admin") {
  await context.addCookies([{ name: "s", value: user, domain: "localhost", path: "/" }]);
}

export const shot = (page: Page, name: string) => page.screenshot({ path: `e2e/screenshots/${name}.png` });
