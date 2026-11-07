// AC-01 (§14.1): `./torrentd` starts with no arguments and
// http://127.0.0.1:8080 shows the UI.
import { expect, test } from "@playwright/test";

test("the UI loads at http://127.0.0.1:8080 with no console errors", async ({ page }) => {
  const errors: string[] = [];
  page.on("console", (msg) => { if (msg.type() === "error") errors.push(msg.text()); });
  page.on("pageerror", (err) => errors.push(err.message));

  const resp = await page.goto("/");
  expect(resp?.status()).toBe(200);
  await expect(page).toHaveTitle(/torrentd/i);
  await expect(page.locator("body")).toBeVisible();
  expect(errors, "console errors").toEqual([]);

  // Client-side routes are served the same app (SPA fallback).
  const settings = await page.goto("/settings");
  expect(settings?.status()).toBe(200);
  await expect(page).toHaveTitle(/torrentd/i);
});
