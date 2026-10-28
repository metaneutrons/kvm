import { expect, test } from "@playwright/test";

import {
  API_TEST_PASSWORD,
  TEST_TOKEN_PREFIX,
  apiRpc,
  bearer,
  deleteTestTokens,
  ensureLocalAuthMode,
  openAccessSettings,
} from "./helpers";

const NAME = `${TEST_TOKEN_PREFIX}settings`;

test.describe("API token settings", () => {
  test.setTimeout(120_000);
  test.describe.configure({ mode: "serial" });

  test.beforeEach(async ({ page }) => {
    await ensureLocalAuthMode(page, { mode: "password", password: API_TEST_PASSWORD });
    await openAccessSettings(page);
  });

  test.afterEach(async ({ page }) => {
    await deleteTestTokens(page);
  });

  test("creates a token, shows it once and deletes it", async ({ page }) => {
    const create = page.getByRole("button", { name: "Create Token" });
    // A token needs a name.
    await expect(create).toBeDisabled();
    await page.getByPlaceholder("e.g. Home Assistant").fill(NAME);
    await expect(create).toBeEnabled();

    // View is always part of a token and cannot be turned off.
    const view = page.getByRole("checkbox", { name: /^View/ });
    await expect(view).toBeChecked();
    await expect(view).toBeDisabled();
    await page.getByRole("checkbox", { name: /^Input/ }).check();
    await create.click();

    const shown = page.getByText(/^jkvm_[A-Za-z0-9_-]{40,}$/);
    await expect(shown).toBeVisible();
    const token = (await shown.textContent()) ?? "";

    const row = page.locator("div.py-3", { hasText: NAME });
    await expect(row).toBeVisible();
    await expect(row.getByText("View", { exact: true })).toBeVisible();
    await expect(row.getByText("Input", { exact: true })).toBeVisible();
    await expect(row.getByText("Power", { exact: true })).toHaveCount(0);
    await expect(row).toContainText("Never used");

    expect((await apiRpc(bearer(token), "ping")).status).toBe(200);

    page.once("dialog", dialog => dialog.accept());
    await row.getByRole("button", { name: "Delete" }).click();
    await expect(row).toHaveCount(0);
    expect((await apiRpc(bearer(token), "ping")).status).toBe(401);
  });
});

test.describe("API token settings without a password", () => {
  test.setTimeout(120_000);

  // The password is set again for whatever runs next.
  test.afterAll(async ({ browser }) => {
    const page = await (await browser.newContext({ baseURL: process.env.JETKVM_URL })).newPage();
    await ensureLocalAuthMode(page, { mode: "password", password: API_TEST_PASSWORD });
    await page.close();
  });

  test("has no tokens", async ({ page }) => {
    await ensureLocalAuthMode(page, { mode: "noPassword" });
    await page.goto("/settings/access");
    await expect(page.locator("text=Authentication Mode")).toBeVisible({ timeout: 15000 });

    await expect(page.getByRole("button", { name: "Create Token" })).toHaveCount(0);
    await expect(page.getByText("API Tokens", { exact: true })).toHaveCount(0);
  });
});
